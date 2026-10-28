package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// arrivalsApp is an app whose clock the test moves and whose OASA stop predictions come from
// answer, counting the calls.
func arrivalsApp(t *testing.T, answer func() ([]telematics.Arrival, error)) (*App, *time.Time, *int) {
	t.Helper()
	now := monday1020
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
	calls := 0
	a.stopArrivals = func(_ context.Context, code string) ([]telematics.Arrival, error) {
		if code != "S2" {
			t.Errorf("asked stop %q", code)
		}
		calls++
		return answer()
	}
	return a, &now, &calls
}

func decodeArrivals(t *testing.T, body []byte) StopArrivalsResponse {
	t.Helper()
	var r StopArrivalsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r
}

// OASA's predictions, soonest first, with the line number from the route code ("" if unknown).
func TestOASAStopArrivals(t *testing.T) {
	a, _, calls := arrivalsApp(t, func() ([]telematics.Arrival, error) {
		return []telematics.Arrival{{RouteCode: "XX", VehCode: "V2", Minutes: 9}, {RouteCode: "SH", VehCode: "V1", Minutes: 4}}, nil
	})
	res, body := get(t, a, "/v1/stops/S2/arrivals")
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	r := decodeArrivals(t, body)
	if r.StopID != "S2" || r.StopName != "STOP 2" || r.UpdatedAt != monday1020.Unix() || len(r.Arrivals) != 2 {
		t.Fatalf("%+v", r)
	}
	if got := r.Arrivals[0]; got != (PredictedArrival{Line: "L", RouteCode: "SH", Vehicle: "V1", Minutes: 4}) {
		t.Fatalf("first %+v", got)
	}
	if got := r.Arrivals[1]; got.Line != "" || got.RouteCode != "XX" || got.Minutes != 9 {
		t.Fatalf("second %+v", got)
	}
	if maxAge(t, res) != 30 || *calls != 1 {
		t.Fatalf("max-age %d, %d calls", maxAge(t, res), *calls)
	}
}

// No predictions is an empty list, not null.
func TestOASAStopArrivalsEmpty(t *testing.T) {
	a, _, _ := arrivalsApp(t, func() ([]telematics.Arrival, error) { return nil, nil })
	res, body := get(t, a, "/v1/stops/S2/arrivals")
	if res.StatusCode != 200 || string(body) == "" || decodeArrivals(t, body).Arrivals == nil {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

// One OASA call per stop per stopArrivalsTTL; Cache-Control runs out with that cache.
func TestOASAStopArrivalsCached(t *testing.T) {
	a, now, calls := arrivalsApp(t, func() ([]telematics.Arrival, error) {
		return []telematics.Arrival{{RouteCode: "SH", Minutes: 3}}, nil
	})
	get(t, a, "/v1/stops/S2/arrivals")
	*now = now.Add(20 * time.Second)
	res, body := get(t, a, "/v1/stops/S2/arrivals")
	if *calls != 1 || maxAge(t, res) != 10 || decodeArrivals(t, body).UpdatedAt != monday1020.Unix() {
		t.Fatalf("%d calls, max-age %d, %s", *calls, maxAge(t, res), body)
	}
	*now = now.Add(15 * time.Second)
	get(t, a, "/v1/stops/S2/arrivals")
	if *calls != 2 {
		t.Fatalf("%d calls after the TTL", *calls)
	}
}

// Ids not in the GTFS never reach OASA.
func TestOASAStopArrivalsUnknownStop(t *testing.T) {
	a, _, calls := arrivalsApp(t, func() ([]telematics.Arrival, error) { return nil, nil })
	res, body := get(t, a, "/v1/stops/NOPE/arrivals")
	if res.StatusCode != 404 || string(body) != `{"error":"unknown_stop"}` || *calls != 0 {
		t.Fatalf("%d %s, %d calls", res.StatusCode, body, *calls)
	}
}

func TestOASAStopArrivalsWarmingUp(t *testing.T) {
	a, _, calls := arrivalsApp(t, func() ([]telematics.Arrival, error) { return nil, nil })
	a.mu.Lock()
	a.w = nil
	a.mu.Unlock()
	res, body := get(t, a, "/v1/stops/S2/arrivals")
	if res.StatusCode != 503 || string(body) != `{"error":"warming_up"}` || *calls != 0 {
		t.Fatalf("%d %s, %d calls", res.StatusCode, body, *calls)
	}
}

// Without a recent answer: 502 when OASA fails, 503 busy when the request queue is full. Errors
// are not cached: the next request asks again.
func TestOASAStopArrivalsErrors(t *testing.T) {
	var err error
	a, _, calls := arrivalsApp(t, func() ([]telematics.Arrival, error) { return nil, err })
	err = errors.New("oasa down")
	res, body := get(t, a, "/v1/stops/S2/arrivals")
	if res.StatusCode != 502 || string(body) != `{"error":"upstream_unavailable"}` || maxAge(t, res) != minMaxAge {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	err = errQueueFull
	res, body = get(t, a, "/v1/stops/S2/arrivals")
	if res.StatusCode != 503 || string(body) != `{"error":"busy"}` || *calls != 2 {
		t.Fatalf("%d %s, %d calls", res.StatusCode, body, *calls)
	}
}

// When OASA or the queue cannot answer, the last good answer is served up to staleArrivalsMax
// old, with its own updated_at and a short max-age; older than that, the error.
func TestOASAStopArrivalsStale(t *testing.T) {
	var err error
	a, now, _ := arrivalsApp(t, func() ([]telematics.Arrival, error) {
		return []telematics.Arrival{{RouteCode: "SH", Minutes: 6}}, err
	})
	get(t, a, "/v1/stops/S2/arrivals")
	err = errQueueFull
	*now = now.Add(time.Minute)
	res, body := get(t, a, "/v1/stops/S2/arrivals")
	r := decodeArrivals(t, body)
	if res.StatusCode != 200 || r.UpdatedAt != monday1020.Unix() || len(r.Arrivals) != 1 || maxAge(t, res) != minMaxAge {
		t.Fatalf("%d %s max-age %d", res.StatusCode, body, maxAge(t, res))
	}
	*now = monday1020.Add(staleArrivalsMax + time.Second)
	if res, body = get(t, a, "/v1/stops/S2/arrivals"); res.StatusCode != 503 {
		t.Fatalf("too old served: %d %s", res.StatusCode, body)
	}
}
