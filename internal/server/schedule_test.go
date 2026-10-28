package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLineSchedule(t *testing.T) {
	a := newApp(t, monday1020)
	res, body := get(t, a, "/v1/lines/L/schedule")
	if res.StatusCode != 200 {
		t.Fatalf("status %d %s", res.StatusCode, body)
	}
	if ma := maxAge(t, res); ma != scheduleMaxAge {
		t.Fatalf("max-age %d", ma)
	}
	var sr ScheduleResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Line != "L" || sr.Date != "2026-10-05" || sr.Expired {
		t.Fatalf("response %+v", sr)
	}
	if len(sr.Directions) != 1 {
		t.Fatalf("directions %+v", sr.Directions)
	}
	d := sr.Directions[0]
	if d.Direction != 0 || d.Headsign != "EAST" || len(d.Departures) != 6 {
		t.Fatalf("direction %+v", d)
	}
	for i, dep := range d.Departures {
		want := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * 10 * time.Minute).Format("15:04")
		if dep.T != want || dep.Trip == "" {
			t.Errorf("departure %d: %+v, want %s", i, dep, want)
		}
		if dep.Seen == nil || *dep.Seen {
			t.Errorf("departure %d: seen %v before any poll, want false", i, dep.Seen)
		}
	}
}

func TestLineScheduleSeen(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020) // the first fixture vehicle is matched to T00
	_, body := get(t, a, "/v1/lines/L/schedule")
	var sr ScheduleResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, dep := range sr.Directions[0].Departures {
		if dep.Seen == nil {
			t.Fatalf("%s: no seen field today", dep.Trip)
		}
		seen[dep.Trip] = *dep.Seen
	}
	if !seen["T00"] || len(seen) != 6 {
		t.Fatalf("seen %v", seen)
	}
	// Every other trip of the fixture had no vehicle.
	for id, v := range seen {
		if id != "T00" && v {
			t.Errorf("%s seen without a vehicle", id)
		}
	}
}

// The seen flags of yesterday's poll must not show on today's timetable before the next poll.
func TestLineScheduleSeenIsTodayOnly(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020) // Monday 2026-10-05: T00 seen
	a.now = func() time.Time { return monday1020.AddDate(0, 0, 7) }
	_, body := get(t, a, "/v1/lines/L/schedule")
	var sr ScheduleResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatal(err)
	}
	deps := sr.Directions[0].Departures
	if len(deps) != 6 {
		t.Fatalf("directions %+v", sr.Directions)
	}
	for _, dep := range deps {
		if dep.Seen == nil || *dep.Seen {
			t.Fatalf("%s: seen %v from yesterday's poll", dep.Trip, dep.Seen)
		}
	}
}

func TestLineScheduleDate(t *testing.T) {
	a := newApp(t, monday1020)
	tests := []struct {
		name    string
		query   string
		status  int
		expired bool
		deps    int
	}{
		{name: "today", query: "", status: 200, deps: 6},
		{name: "a week ahead is outside the feed", query: "date=2026-10-12", status: 200, expired: true, deps: 6},
		{name: "yesterday", query: "date=2026-10-04", status: 400},
		{name: "eight days ahead", query: "date=2026-10-13", status: 400},
		{name: "not a date", query: "date=2026-02-31", status: 400},
		{name: "a day the service does not run", query: "date=2026-10-07", status: 200, expired: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/lines/L/schedule"
			if tc.query != "" {
				path += "?" + tc.query
			}
			res, body := get(t, a, path)
			if res.StatusCode != tc.status {
				t.Fatalf("status %d %s", res.StatusCode, body)
			}
			if tc.status != 200 {
				return
			}
			var sr ScheduleResponse
			if err := json.Unmarshal(body, &sr); err != nil {
				t.Fatal(err)
			}
			if sr.Expired != tc.expired {
				t.Errorf("expired %v", sr.Expired)
			}
			if sr.Directions == nil {
				t.Error("directions null, want []")
			}
			deps := 0
			for _, d := range sr.Directions {
				deps += len(d.Departures)
			}
			if deps != tc.deps {
				t.Errorf("%d departures, want %d", deps, tc.deps)
			}
			if tc.query != "" && strings.Contains(string(body), `"seen"`) {
				t.Error("seen field on a day that is not today")
			}
		})
	}
}

func TestLineScheduleUnknownLine(t *testing.T) {
	a := newApp(t, monday1020)
	res, body := get(t, a, "/v1/lines/ZZZ/schedule")
	if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_line"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}
