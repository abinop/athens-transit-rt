package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatsEndpoints(t *testing.T) {
	a := newApp(t, time.Unix(1_800_000_000, 0))
	if res, _ := get(t, a, "/v1/stats"); res.StatusCode != 404 {
		t.Fatalf("no index: %d", res.StatusCode)
	}
	days := filepath.Join(a.statsDir(), "days")
	os.MkdirAll(days, 0o755)
	os.WriteFile(filepath.Join(a.statsDir(), "index.json"), []byte(`{"days":["2026-10-06"]}`), 0o644)
	os.WriteFile(filepath.Join(days, "2026-10-06.json"), []byte(`{"date":"2026-10-06"}`), 0o644)

	res, body := get(t, a, "/v1/stats")
	if res.StatusCode != 200 || string(body) != `{"days":["2026-10-06"]}` || maxAge(t, res) != statsIndexMaxAge {
		t.Fatalf("index: %d %s", res.StatusCode, body)
	}
	res, body = get(t, a, "/v1/stats/days/2026-10-06")
	if res.StatusCode != 200 || string(body) != `{"date":"2026-10-06"}` || maxAge(t, res) != statsDayMaxAge ||
		res.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("day: %d %s", res.StatusCode, body)
	}
	if res, _ := get(t, a, "/v1/stats/days/2026-10-07"); res.StatusCode != 404 {
		t.Fatalf("missing day: %d", res.StatusCode)
	}
	for _, p := range []string{"/v1/stats/days/2026-1-07", "/v1/stats/days/index", "/v1/stats/days/2026-10-06.json"} {
		if res, _ := get(t, a, p); res.StatusCode != 400 {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
}

func TestStatsPlanUsesTheFeedInUse(t *testing.T) {
	a := newApp(t, time.Unix(1_800_000_000, 0))
	p, err := a.statsPlan("2026-09-28") // a Monday of the test feed
	if err != nil || p.GTFS == "" || p.GTFS != worldVersion(a.world()) || len(p.Trips) != 6 {
		t.Fatalf("plan = %+v, err %v", p, err)
	}
}
