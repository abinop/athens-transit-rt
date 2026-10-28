package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/meta"
	"github.com/angelospk/athens-transit-rt/internal/sched"

	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

func TestDailyToSchedule(t *testing.T) {
	d := telematics.DailySchedule{Descr: "ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ", Go: []string{"05:00", "05:30"}, Come: []string{"05:55"}}
	got := dailyToSchedule("608", "2026-10-28", d)
	want := ScheduleResponse{Line: "608", Date: "2026-10-28", Source: "oasa-daily", Directions: []ScheduleDirection{
		{Direction: 0, Headsign: "ΝΕΚΡ. ΖΩΓΡΑΦΟΥ", Departures: []ScheduleDeparture{{T: "05:00"}, {T: "05:30"}}},
		{Direction: 1, Headsign: "ΓΑΛΑΤΣΙ", Departures: []ScheduleDeparture{{T: "05:55"}}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// A circular line (or a line without "come" runs) has one direction; no " - " keeps the whole name.
	c := dailyToSchedule("021", "2026-10-28", telematics.DailySchedule{Descr: "ΚΥΚΛΙΚΗ", Circular: true, Go: []string{"07:30"}})
	if len(c.Directions) != 1 || c.Directions[0].Headsign != "ΚΥΚΛΙΚΗ" || c.Directions[0].Direction != 0 {
		t.Fatalf("circular %+v", c)
	}
	// Two vehicles at one time (one ends its shift there, QA line Β9 11:45) are one departure.
	dup := dailyToSchedule("Β9", "2026-10-28", telematics.DailySchedule{Descr: "Α - Β", Go: []string{"11:45", "11:45", "12:15"}, Come: []string{"08:45", "08:45"}})
	if g, c := dup.Directions[0].Departures, dup.Directions[1].Departures; len(g) != 2 || len(c) != 1 {
		t.Fatalf("duplicates kept: %+v", dup)
	}
}

// Today the timetable comes from OASA's daily schedule of every line code of the line (merged,
// sorted); seen flags are not given then (the matcher works on the GTFS trips).
func TestLineScheduleUsesDaily(t *testing.T) {
	a := newApp(t, monday1020)
	var asked []string
	a.daily = func(ctx context.Context, code string) (telematics.DailySchedule, error) {
		asked = append(asked, code)
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"10:05", "09:00"}, Come: []string{"11:00"}}, nil
	}
	res, body := get(t, a, "/v1/lines/L/schedule")
	if res.StatusCode != 200 {
		t.Fatalf("status %d %s", res.StatusCode, body)
	}
	var sr ScheduleResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Source != "oasa-daily" || sr.Expired || len(sr.Directions) != 2 {
		t.Fatalf("response %+v", sr)
	}
	if got := sr.Directions[0].Departures; len(got) != 2 || got[0].T != "09:00" || got[1].T != "10:05" || got[0].Seen != nil {
		t.Fatalf("go %+v", got)
	}
	if !reflect.DeepEqual(asked, []string{"R1"}) {
		t.Fatalf("asked %v", asked)
	}
	// Cached for the day: a second request does not ask OASA again.
	get(t, a, "/v1/lines/L/schedule")
	if len(asked) != 1 {
		t.Fatalf("asked again: %v", asked)
	}
}

// OASA down, empty, or another day than today: the GTFS timetable, marked as such.
func TestLineScheduleDailyFallback(t *testing.T) {
	for name, fn := range map[string]func(context.Context, string) (telematics.DailySchedule, error){
		"error": func(context.Context, string) (telematics.DailySchedule, error) {
			return telematics.DailySchedule{}, errors.New("down")
		},
		"empty": func(context.Context, string) (telematics.DailySchedule, error) {
			return telematics.DailySchedule{}, nil
		},
	} {
		a := newApp(t, monday1020)
		a.daily = fn
		_, body := get(t, a, "/v1/lines/L/schedule")
		var sr ScheduleResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			t.Fatal(err)
		}
		if sr.Source != "gtfs" || len(sr.Directions) != 1 || len(sr.Directions[0].Departures) != 6 {
			t.Fatalf("%s: %+v", name, sr)
		}
	}
	a := newApp(t, monday1020)
	a.daily = func(context.Context, string) (telematics.DailySchedule, error) {
		t.Fatal("daily schedule asked for another day")
		return telematics.DailySchedule{}, nil
	}
	_, body := get(t, a, "/v1/lines/L/schedule?date=2026-10-06")
	var sr ScheduleResponse
	json.Unmarshal(body, &sr)
	if sr.Source != "gtfs" {
		t.Fatalf("tomorrow %+v", sr)
	}
}

// One of the line's codes failing: the runs of the others are served, but asked again after the
// negative TTL, not kept for the day.
func TestLineScheduleDailyPartialRetries(t *testing.T) {
	now := monday1020
	a := appWithLines(t, func() time.Time { return now }, []telematics.Line{{LineCode: "R1", LineID: "L"}, {LineCode: "R2", LineID: "L"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "R2": {{RouteCode: "SH2"}}})
	var calls atomic.Int32
	a.daily = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		calls.Add(1)
		if code == "R2" {
			return telematics.DailySchedule{}, errors.New("down")
		}
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	_, body := get(t, a, "/v1/lines/L/schedule")
	var sr ScheduleResponse
	json.Unmarshal(body, &sr)
	if sr.Source != "oasa-daily" || calls.Load() != 2 {
		t.Fatalf("first %+v calls %d", sr, calls.Load())
	}
	get(t, a, "/v1/lines/L/schedule")
	if calls.Load() != 2 {
		t.Fatalf("asked again within the TTL: %d", calls.Load())
	}
	now = now.Add(dailyNegativeTTL + time.Second)
	get(t, a, "/v1/lines/L/schedule")
	if calls.Load() != 4 {
		t.Fatalf("partial result kept past the TTL: %d calls", calls.Load())
	}
}

// appWithLines is newApp with its own telematics lines and routes.
func appWithLines(t *testing.T, clock func() time.Time, lines []telematics.Line, routes map[string][]telematics.Route) *App {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.Marshal(meta.Data{FetchedAt: clock().Unix(), Lines: lines, Routes: routes})
	if err := os.WriteFile(filepath.Join(dir, "telematics-meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(Config{StateDir: dir, RPS: 4, Matcher: match.Memory, Sched: sched.DefaultConfig()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.now = clock
	a.sched.SetClock(a.now)
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 5}.Feed(t), nil)
	return a
}

// A line code with an empty answer (no runs today, or OASA hiccup) is asked again later too.
func TestLineScheduleDailyEmptyCodeRetries(t *testing.T) {
	now := monday1020
	a := appWithLines(t, func() time.Time { return now }, []telematics.Line{{LineCode: "R1", LineID: "L"}, {LineCode: "R2", LineID: "L"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "R2": {{RouteCode: "SH2"}}})
	var calls atomic.Int32
	a.daily = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		calls.Add(1)
		if code == "R2" {
			return telematics.DailySchedule{}, nil
		}
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	get(t, a, "/v1/lines/L/schedule")
	now = now.Add(dailyNegativeTTL + time.Second)
	get(t, a, "/v1/lines/L/schedule")
	if calls.Load() != 4 {
		t.Fatalf("empty code not retried: %d calls", calls.Load())
	}
}

// A line code the metadata knows without routes yet still counts.
func TestLineScheduleDailyCodeWithoutRoutes(t *testing.T) {
	a := appWithLines(t, func() time.Time { return monday1020 }, []telematics.Line{{LineCode: "R1", LineID: "L"}, {LineCode: "R9", LineID: "L"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}})
	var asked []string
	var mu sync.Mutex // the codes are asked in parallel
	a.daily = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		mu.Lock()
		asked = append(asked, code)
		mu.Unlock()
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	get(t, a, "/v1/lines/L/schedule")
	sort.Strings(asked)
	if !reflect.DeepEqual(asked, []string{"R1", "R9"}) {
		t.Fatalf("asked %v", asked)
	}
}

// Requests that miss the cache at the same time share one fetch per line code.
func TestLineScheduleDailyConcurrentMisses(t *testing.T) {
	a := newApp(t, monday1020)
	var calls atomic.Int32
	release := make(chan struct{})
	a.daily = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		calls.Add(1)
		<-release
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	var wg sync.WaitGroup
	sources := make([]string, 5)
	for i := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/lines/L/schedule", nil))
			var sr ScheduleResponse
			json.Unmarshal(rec.Body.Bytes(), &sr)
			sources[i] = sr.Source
		}()
	}
	time.Sleep(50 * time.Millisecond) // let them all miss
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d fetches for 5 concurrent requests", calls.Load())
	}
	for i, s := range sources {
		if s != "oasa-daily" {
			t.Errorf("request %d: source %q", i, s)
		}
	}
}

// A client that leaves does not cut the fetch short: the full timetable is cached for the others.
func TestLineScheduleDailyClientCancel(t *testing.T) {
	a := newApp(t, monday1020)
	calls := 0
	a.daily = func(ctx context.Context, code string) (telematics.DailySchedule, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return telematics.DailySchedule{}, err
		}
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/lines/L/schedule", nil).WithContext(ctx))
	_, body := get(t, a, "/v1/lines/L/schedule")
	var sr ScheduleResponse
	json.Unmarshal(body, &sr)
	if sr.Source != "oasa-daily" || calls != 1 {
		t.Fatalf("after a cancelled client: %+v, %d calls", sr, calls)
	}
}

// The real fetch waits for a scheduler slot: nothing reaches OASA until the scheduler runs.
func TestDailyGoesThroughScheduler(t *testing.T) {
	var hits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "getDailySchedule" {
			hits.Add(1)
			w.Write([]byte(`{"go":[{"sde_start1":"1900-01-01 09:00:00","line_descr":"A - B"}],"come":[]}`))
			return
		}
		w.Write([]byte("[]"))
	}))
	defer fake.Close()
	a := newApp(t, monday1020)
	a.client.BaseURL = fake.URL + "/"
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.dailyQueued(short, "R1"); err == nil || hits.Load() != 0 {
		t.Fatalf("answered without the scheduler: err %v, %d hits", err, hits.Load())
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go a.sched.Run(ctx)
	wait, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	d, err := a.dailyQueued(wait, "R1")
	if err != nil || len(d.Go) != 1 || d.Go[0] != "09:00" {
		t.Fatalf("through the scheduler: %+v %v", d, err)
	}
}

// A request after midnight that finds yesterday's fetch in flight does not take its runs as today's.
func TestLineScheduleDailyWaiterAfterMidnight(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 5, 23, 59, 59, 0, gtfs.Athens).UnixNano())
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return time.Unix(0, clock.Load()) })
	a.daily = func(context.Context, string) (telematics.DailySchedule, error) {
		return telematics.DailySchedule{}, errors.New("unset")
	}
	started, release := make(chan struct{}), make(chan struct{})
	a.daily = func(context.Context, string) (telematics.DailySchedule, error) {
		close(started)
		<-release
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/lines/L/schedule", nil))
	}()
	<-started
	clock.Store(time.Date(2026, 10, 6, 0, 0, 1, 0, gtfs.Athens).UnixNano())
	got := make(chan ScheduleResponse)
	go func() {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/lines/L/schedule", nil))
		var sr ScheduleResponse
		json.Unmarshal(rec.Body.Bytes(), &sr)
		got <- sr
	}()
	time.Sleep(50 * time.Millisecond) // the second request is waiting on the first fetch
	close(release)
	sr := <-got
	<-done
	if sr.Date != "2026-10-06" || sr.Source != "gtfs" {
		t.Fatalf("after midnight: %+v", sr)
	}
}
