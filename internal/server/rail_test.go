package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/rail"
	"github.com/angelospk/athens-transit-rt/internal/sched"
)

// newAppConfig is newApp for a handler that needs its own config (an upstream URL), without a
// static feed.
func newAppConfig(t *testing.T, now time.Time, cfg Config) *App {
	t.Helper()
	cfg.StateDir = t.TempDir()
	cfg.RPS = 4
	cfg.Matcher = match.Memory
	cfg.Sched = sched.DefaultConfig()
	a := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.now = func() time.Time { return now }
	a.sched.SetClock(a.now)
	return a
}

const suburbanStationsBody = `{"corridors":[{"id":"KIATO","name":"Πειραιάς – Κιάτο","stations":[
 {"stationId":"ST1","stationIds":["ST1"],"nameGreek":"Πειραιάς","name":"Πειραιάς"}]}]}`

const suburbanBoardBody = `{"schedules":[{"trainNumber":"2539","status":"scheduled","delay":0,
 "destination":{"nameGreek":"Κιάτο"},
 "routeStations":[{"stationId":"ST1","nameGreek":"Πειραιάς","role":"origin","index":0,
  "scheduledDeparture":"2026-10-05T07:20:00.000Z","platform":"2"}]}]}`

func TestSTASYNotLoaded(t *testing.T) {
	a := newAppConfig(t, monday1020, Config{})
	res, body := get(t, a, "/v1/rail/stasy/departures?stop=ΣΥΝΤΑΓΜΑ")
	if res.StatusCode != 503 || !strings.Contains(string(body), `"warming_up"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = get(t, a, "/v1/rail/stasy/departures")
	if res.StatusCode != 400 || !strings.Contains(string(body), `"missing_stop"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestSuburbanHandlers(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/api/public/corridor-stations":
			io.WriteString(w, suburbanStationsBody)
		case "/api/public/schedule-board":
			io.WriteString(w, suburbanBoardBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := newAppConfig(t, monday1020, Config{SuburbanURL: srv.URL})

	res, body := get(t, a, "/v1/rail/suburban/stations")
	if res.StatusCode != 200 || !strings.Contains(string(body), `"Πειραιάς"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = get(t, a, "/v1/rail/suburban/stations/ST1/departures")
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if ma := maxAge(t, res); ma != suburbanMaxAge {
		t.Errorf("max-age %d", ma)
	}
	var sd struct {
		Station    string `json:"station"`
		Departures []struct {
			Train    string  `json:"train"`
			To       string  `json:"to"`
			T        string  `json:"t"`
			Platform *string `json:"platform"`
			Status   string  `json:"status"`
		} `json:"departures"`
	}
	if err := json.Unmarshal(body, &sd); err != nil {
		t.Fatal(err)
	}
	// 07:20Z is 10:20 in Athens (EEST until 2026-10-25).
	if sd.Station != "Πειραιάς" || len(sd.Departures) != 1 || sd.Departures[0].Train != "2539" ||
		sd.Departures[0].T != "10:20" || sd.Departures[0].To != "Κιάτο" || sd.Departures[0].Status != "scheduled" ||
		sd.Departures[0].Platform == nil || *sd.Departures[0].Platform != "2" {
		b, _ := json.Marshal(sd)
		t.Fatalf("departures %s", b)
	}

	// Nothing cached: an upstream failure is a gateway error, not an empty list.
	a2 := newAppConfig(t, monday1020, Config{SuburbanURL: srv.URL, MPassURL: srv.URL})
	fail = true
	res, body = get(t, a2, "/v1/rail/suburban/stations")
	if res.StatusCode != 502 || !strings.Contains(string(body), `"upstream_unavailable"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = get(t, a2, "/v1/rail/suburban/stations/ST1/departures")
	if res.StatusCode != 502 || !strings.Contains(string(body), `"upstream_unavailable"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

// stasyApp is an App with the STASY test timetable loaded from disk, at now.
func stasyApp(t *testing.T, now time.Time) *App {
	t.Helper()
	a := newAppConfig(t, now, Config{STASYURL: "http://127.0.0.1:1/none"})
	if err := os.CopyFS(a.stasy.Dir, os.DirFS(filepath.Join("..", "rail", "testdata", "stasy"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run loads the files on disk, then stops at once: no catalogue check
	a.stasy.Run(ctx)
	if _, ok := a.stasy.Feed(); !ok {
		t.Fatal("stasy not loaded")
	}
	return a
}

func TestSTASYVehiclesHandler(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 5, 0, 0, gtfs.Athens)
	a := stasyApp(t, now)
	res, body := get(t, a, "/v1/rail/stasy/vehicles")
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "public, max-age=15" {
		t.Fatalf("%d %q %s", res.StatusCode, res.Header.Get("Cache-Control"), body)
	}
	var v rail.STASYVehicles
	if err := json.Unmarshal(body, &v); err != nil || len(v.Vehicles) != 4 || v.UpdatedAt != now.Unix() {
		t.Fatalf("%v %s", err, body)
	}

	res, body = get(t, a, "/v1/rail/stasy/trips/M2-1")
	if res.StatusCode != 200 || !strings.Contains(string(body), `"stops":[{"name":"ΣΥΝΤΑΓΜΑ"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = get(t, a, "/v1/rail/stasy/trips/nope")
	if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_trip"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}

	cold := newAppConfig(t, now, Config{})
	for _, path := range []string{"/v1/rail/stasy/vehicles", "/v1/rail/stasy/trips/M2-1"} {
		if res, body := get(t, cold, path); res.StatusCode != 503 || !strings.Contains(string(body), `"warming_up"`) {
			t.Fatalf("%s: %d %s", path, res.StatusCode, body)
		}
	}
}

func TestSuburbanLiveHandlers(t *testing.T) {
	positions := `{"positions":[{"trainNumber":"2539","scheduleId":"s1","serviceType":"Suburban","origin":"Πειραιάς",` +
		`"destination":"Κιάτο","nextStation":"Λεύκα","delay":7,"corridor":"KIATO","lat":37.95,"lng":23.64,"speed":40,` +
		`"course":300,"hasCourse":true,"timestamp":"2026-10-05T07:19:50Z"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/public/corridor-stations":
			io.WriteString(w, suburbanStationsBody)
		case "/api/public/schedule-board":
			io.WriteString(w, suburbanBoardBody)
		case "/api/train-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: trainPositionsUx\ndata: "+positions+"\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := newAppConfig(t, monday1020, Config{SuburbanURL: srv.URL})

	// Before the stream runs, and before a client asks, there is no snapshot.
	res, body := get(t, a, "/v1/rail/suburban/vehicles")
	if res.StatusCode != 503 || !strings.Contains(string(body), `"warming_up"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	// Without the stream the board's own delay is kept.
	if _, body := get(t, a, "/v1/rail/suburban/stations/ST1/departures"); !strings.Contains(string(body), `"delay_min":0`) {
		t.Fatalf("board delay: %s", body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.trains.Run(ctx)
	a.trains.Vehicles() // the request above woke it before Run listened; this one is heard
	deadline := time.Now().Add(3 * time.Second)
	for res.StatusCode != 200 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		res, body = get(t, a, "/v1/rail/suburban/vehicles")
	}
	if res.StatusCode != 200 || maxAge(t, res) != suburbanVehAge {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var v rail.SuburbanVehicles
	if err := json.Unmarshal(body, &v); err != nil || len(v.Trains) != 1 || v.Trains[0].Train != "2539" || v.Trains[0].Next != "Λεύκα" {
		t.Fatalf("%v %s", err, body)
	}

	// The live delay replaces the board's, in the departures and in the train's run.
	if _, body := get(t, a, "/v1/rail/suburban/stations/ST1/departures"); !strings.Contains(string(body), `"delay_min":7`) {
		t.Fatalf("departures: %s", body)
	}
	res, body = get(t, a, "/v1/rail/suburban/trains/2539")
	if res.StatusCode != 200 || maxAge(t, res) != suburbanTrainAge || !strings.Contains(string(body), `"delay_min":7,"stops":[{"id":"ST1","name":"Πειραιάς","t":"10:20"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	res, body = get(t, a, "/v1/rail/suburban/trains/0000")
	if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_train"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}
