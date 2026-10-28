package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/match"
)

func TestStopArrivals(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)

	t.Run("the buses on their way, with the time the timetable plus their delay gives", func(t *testing.T) {
		res, body := get(t, a, "/v1/stops/S3")
		if res.StatusCode != 200 || maxAge(t, res) != stopMaxAge {
			t.Fatalf("%d %s", res.StatusCode, body)
		}
		var sr StopResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			t.Fatal(err)
		}
		// T00 is due at S3 at 10:15 and runs 440 s late: 10:22:20, 140 s after 10:20.
		want := StopArrival{Line: "L", Headsign: "EAST", Variant: "SH", Vehicle: "44548", EtaS: 140, DelayS: 440,
			At: monday1020.Add(140 * time.Second).Unix(), PositionAt: monday1020.Unix() - 10}
		if sr.Stop.ID != "S3" || sr.Stop.Name != "STOP 3" || strings.Join(sr.Lines, ",") != "L" ||
			len(sr.Arrivals) != 1 || sr.Arrivals[0] != want || sr.UpdatedAt != monday1020.Unix() {
			t.Fatalf("got %s", body)
		}
		if !a.sched.IsWatched("L") {
			t.Error("a stop request must mark its lines watched")
		}
	})

	t.Run("a stop the bus has passed: its lines, no arrivals", func(t *testing.T) {
		_, body := get(t, a, "/v1/stops/S1")
		var sr StopResponse
		json.Unmarshal(body, &sr)
		if strings.Join(sr.Lines, ",") != "L" || sr.Arrivals == nil || len(sr.Arrivals) != 0 {
			t.Fatalf("got %s", body)
		}
	})

	t.Run("an unknown stop", func(t *testing.T) {
		res, body := get(t, a, "/v1/stops/NOPE")
		if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_stop"`) {
			t.Fatalf("%d %s", res.StatusCode, body)
		}
	})
}

func TestStopArrivalsLateBusIsDueNow(t *testing.T) {
	// 4 minutes on, the bus is still before S3 by its last fix (due 10:22:20): due now, never a negative wait.
	a := newApp(t, monday1020)
	poll(a, monday1020)
	a.now = func() time.Time { return monday1020.Add(4 * time.Minute) }
	_, body := get(t, a, "/v1/stops/S3")
	var sr StopResponse
	json.Unmarshal(body, &sr)
	if len(sr.Arrivals) != 1 || sr.Arrivals[0].EtaS != 0 {
		t.Fatalf("got %s", body)
	}
}

func TestStopArrivalsDropOldFixes(t *testing.T) {
	// A bus not heard of for longer than staleFix may be gone: not promised.
	a := newApp(t, monday1020)
	poll(a, monday1020)
	a.now = func() time.Time { return monday1020.Add(staleFix) }
	_, body := get(t, a, "/v1/stops/S4")
	var sr StopResponse
	json.Unmarshal(body, &sr)
	if len(sr.Arrivals) != 0 {
		t.Fatalf("got %s", body)
	}
}

func TestStopArrivalsBusThatChangedLines(t *testing.T) {
	// 44548 reports on another line after its fix on L: its L result is old news.
	a := newApp(t, monday1020)
	poll(a, monday1020)
	a.mu.Lock()
	a.lines["Α1"] = &lineData{updated: monday1020, feed: a.w.feed, results: []match.Result{{VehicleID: "44548", Line: "Α1", Trip: -1, Time: monday1020}}}
	a.mu.Unlock()
	_, body := get(t, a, "/v1/stops/S3")
	var sr StopResponse
	json.Unmarshal(body, &sr)
	if len(sr.Arrivals) != 0 {
		t.Fatalf("got %s", body)
	}
}
