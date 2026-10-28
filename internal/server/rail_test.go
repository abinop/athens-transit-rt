package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/match"
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
