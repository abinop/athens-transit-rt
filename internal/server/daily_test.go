package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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
	dir := t.TempDir()
	m := meta.Data{FetchedAt: now.Unix(), Lines: []telematics.Line{{LineCode: "R1", LineID: "L"}, {LineCode: "R2", LineID: "L"}},
		Routes: map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "R2": {{RouteCode: "SH2"}}}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "telematics-meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(Config{StateDir: dir, RPS: 4, Matcher: match.Memory, Sched: sched.DefaultConfig()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.now = func() time.Time { return now }
	a.sched.SetClock(a.now)
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 5}.Feed(t), nil)
	calls := 0
	a.daily = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		calls++
		if code == "R2" {
			return telematics.DailySchedule{}, errors.New("down")
		}
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00"}}, nil
	}
	_, body := get(t, a, "/v1/lines/L/schedule")
	var sr ScheduleResponse
	json.Unmarshal(body, &sr)
	if sr.Source != "oasa-daily" || calls != 2 {
		t.Fatalf("first %+v calls %d", sr, calls)
	}
	get(t, a, "/v1/lines/L/schedule")
	if calls != 2 {
		t.Fatalf("asked again within the TTL: %d", calls)
	}
	now = now.Add(dailyNegativeTTL + time.Second)
	get(t, a, "/v1/lines/L/schedule")
	if calls != 4 {
		t.Fatalf("partial result kept past the TTL: %d calls", calls)
	}
}
