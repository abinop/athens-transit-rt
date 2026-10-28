package server

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// Every line's daily timetable is fetched ahead through the background queue, so a stop with 26
// lines (or an MCP question) finds it cached instead of queueing 40 OASA calls at once (QA Q11).
func TestPrefetchDailyFillsTheCache(t *testing.T) {
	a := appWithLines(t, func() time.Time { return monday1020 }, []telematics.Line{{LineCode: "R1", LineID: "608"}, {LineCode: "R2", LineID: "040"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "R2": {{RouteCode: "SH2"}}})
	var bg, urgent atomic.Int32
	a.dailyBg = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		bg.Add(1)
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00", "09:30"}}, nil
	}
	a.daily = func(context.Context, string) (telematics.DailySchedule, error) {
		urgent.Add(1)
		return telematics.DailySchedule{}, nil
	}
	if !a.prefetchDailyOnce(context.Background()) {
		t.Fatal("prefetch did not finish")
	}
	if bg.Load() != 2 {
		t.Fatalf("background calls %d, want 2 (one per line code)", bg.Load())
	}
	for _, l := range []string{"608", "040"} {
		_, body := get(t, a, "/v1/lines/"+l+"/schedule")
		var s ScheduleResponse
		if err := json.Unmarshal(body, &s); err != nil || s.Source != "oasa-daily" {
			t.Fatalf("%s: %s", l, body)
		}
	}
	if urgent.Load() != 0 {
		t.Fatalf("a request asked OASA %d times; the prefetch had it", urgent.Load())
	}
	a.prefetchDailyOnce(context.Background())
	if bg.Load() != 2 {
		t.Fatalf("a second prefetch the same day asked again: %d", bg.Load())
	}
}

// A shutdown stops the prefetch between lines.
func TestPrefetchDailyStopsWithContext(t *testing.T) {
	a := appWithLines(t, func() time.Time { return monday1020 }, []telematics.Line{{LineCode: "R1", LineID: "608"}, {LineCode: "R2", LineID: "040"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "R2": {{RouteCode: "SH2"}}})
	ctx, cancel := context.WithCancel(context.Background())
	var bg atomic.Int32
	a.dailyBg = func(context.Context, string) (telematics.DailySchedule, error) {
		bg.Add(1)
		cancel()
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	if a.prefetchDailyOnce(ctx) {
		t.Fatal("finished although stopped")
	}
	if bg.Load() != 1 {
		t.Fatalf("calls after the stop: %d", bg.Load())
	}
}

// A request that finds the prefetch fetching its line waits no longer than its own deadline
// (Codex): the shared fetch goes on, the request falls back to the GTFS.
func TestDailyJoinWaitsOnlyItsOwnDeadline(t *testing.T) {
	a := appWithLines(t, func() time.Time { return monday1020 }, []telematics.Line{{LineCode: "R1", LineID: "608"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}})
	release := make(chan struct{})
	started := make(chan struct{})
	slow := func(ctx context.Context, _ string) (telematics.DailySchedule, error) {
		close(started)
		<-release
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	go a.dailyVia(context.Background(), "608", monday1020, slow, time.Minute, false)
	<-started
	t0 := time.Now()
	if _, ok := a.dailyVia(context.Background(), "608", monday1020, slow, 50*time.Millisecond, true); ok {
		t.Fatal("a timetable before the fetch ended")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("the request waited %v", d)
	}
	close(release)
}

// A shutdown ends a prefetch fetch that waits for its slot (Codex).
func TestPrefetchDailyShutdownWhileWaiting(t *testing.T) {
	a := appWithLines(t, func() time.Time { return monday1020 }, []telematics.Line{{LineCode: "R1", LineID: "608"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}})
	ctx, cancel := context.WithCancel(context.Background())
	a.dailyBg = func(c context.Context, _ string) (telematics.DailySchedule, error) {
		cancel()
		<-c.Done()
		return telematics.DailySchedule{}, c.Err()
	}
	done := make(chan bool)
	go func() { done <- a.prefetchDailyOnce(ctx) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("finished although stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prefetch outlived the shutdown")
	}
}

// A round that runs past midnight stops: it must not write yesterday's timetables (Codex).
func TestPrefetchDailyStopsAtMidnight(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 10, 5, 23, 59, 50, 0, gtfs.Athens).UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()).In(gtfs.Athens) }
	a := appWithLines(t, clock, []telematics.Line{{LineCode: "R1", LineID: "608"}, {LineCode: "R2", LineID: "040"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "R2": {{RouteCode: "SH2"}}})
	var bg atomic.Int32
	a.dailyBg = func(context.Context, string) (telematics.DailySchedule, error) {
		bg.Add(1)
		now.Add(int64(20 * time.Second)) // the first line ends after midnight
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"23:00"}}, nil
	}
	if a.prefetchDailyOnce(context.Background()) {
		t.Fatal("the round went on past midnight")
	}
	if bg.Load() != 1 {
		t.Fatalf("lines fetched: %d, want 1", bg.Load())
	}
}
