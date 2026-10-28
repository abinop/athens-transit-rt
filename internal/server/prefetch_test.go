package server

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

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
