package server

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/history"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
)

// typicalTable: two vehicles drove shape SH at 5 m/s over its first 5 km a week ago, feed `feed`.
func typicalTable(t *testing.T, at time.Time, feed string) *segspeed.Table {
	t.Helper()
	dir := t.TempDir()
	start := at.Add(-7 * 24 * time.Hour).Truncate(time.Hour)
	var b strings.Builder
	b.WriteString(history.Header)
	for _, v := range []string{"h1", "h2"} {
		for i := 0; i <= 50; i++ {
			fmt.Fprintf(&b, "%d,L,SH,%s,37980000,23720000,%s,T,SH,%d,0\n", start.Unix()+int64(i*20), v, feed, i*100)
		}
	}
	f, err := os.Create(filepath.Join(dir, start.UTC().Format("2006-01-02T15")+".csv.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	zw.Write([]byte(b.String()))
	zw.Close()
	f.Close()
	tab, err := segspeed.Build(dir, gtfs.Athens, at.Add(-14*24*time.Hour), at, feed)
	if err != nil || tab.Shapes() != 1 {
		t.Fatalf("table: %v, %d shapes", err, tab.Shapes())
	}
	return tab
}

// TestLinePathSpeeds (contract rev 6): with a typical-speed table a matched vehicle gets
// path_speeds, m/s per 200 m step ahead, on /v1/lines and /v1/vehicles alike; without one, or
// with one of another GTFS feed, null.
func TestLinePathSpeeds(t *testing.T) {
	for _, c := range []string{"table", "none", "other feed"} {
		withTable := c == "table"
		now := monday1020
		a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
		switch c {
		case "table":
			a.segs.Store(typicalTable(t, now, a.gtfsVersion()))
		case "other feed":
			a.segs.Store(typicalTable(t, now, "1999-01-01"))
		}
		step := 300 / (geo.XY(37.98, 23.71)[0] - geo.XY(37.98, 23.70)[0]) * 0.01
		pollOne(a, now, "v", 23.72, now.Add(-20*time.Second))
		now = now.Add(30 * time.Second)
		pollOne(a, now, "v", 23.72+step, now.Add(-20*time.Second))
		_, body := get(t, a, "/v1/lines/L")
		var lr LineResponse
		if err := json.Unmarshal(body, &lr); err != nil || len(lr.Vehicles) != 1 {
			t.Fatalf("%v %s", err, body)
		}
		if !strings.Contains(string(body), `"path_speeds":`) {
			t.Fatalf("no path_speeds key: %s", body)
		}
		v := lr.Vehicles[0]
		if !withTable {
			if v.PathSpeeds != nil {
				t.Fatalf("%s: path_speeds %v", c, v.PathSpeeds)
			}
			continue
		}
		if len(v.PathSpeeds) == 0 || len(v.PathSpeeds) > segspeed.NSteps {
			t.Fatalf("path_speeds %v", v.PathSpeeds)
		}
		for i, s := range v.PathSpeeds {
			if s == nil || math.Abs(*s-5) > 0.05 {
				t.Fatalf("step %d: %v", i, v.PathSpeeds)
			}
		}
		_, body = get(t, a, "/v1/vehicles")
		c := decodeVehicles(t, body).Vehicles[0]
		if !slices.EqualFunc(c.PathSpeeds, v.PathSpeeds, func(x, y *float64) bool { return *x == *y }) {
			t.Fatalf("/v1/vehicles path_speeds %s", body)
		}
	}
}

// TestPathSpeedsFollowLiveTraffic (R3): two buses that just crossed the road ahead at 15 m/s,
// where the usual speed at this hour is 5 m/s, bring the steps ahead to 0.7×15 + 0.3×5 = 12.
func TestPathSpeedsFollowLiveTraffic(t *testing.T) {
	now := monday1020
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
	a.segs.Store(typicalTable(t, now, a.gtfsVersion()))
	step := 300 / (geo.XY(37.98, 23.71)[0] - geo.XY(37.98, 23.70)[0]) * 0.01 // degrees for 300 m
	for _, id := range []string{"w1", "w2"} {
		pollOne(a, now, id, 23.72+step, now.Add(-60*time.Second))
		pollOne(a, now, id, 23.72+3*step, now.Add(-20*time.Second)) // 600 m in 40 s
	}
	pollOne(a, now, "v", 23.72, now.Add(-20*time.Second))
	now = now.Add(30 * time.Second)
	pollOne(a, now, "v", 23.72+step, now.Add(-20*time.Second))
	_, body := get(t, a, "/v1/lines/L")
	var lr LineResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatal(err)
	}
	for _, v := range lr.Vehicles {
		if v.ID != "v" {
			continue
		}
		// Step 0 starts a little before the road the others crossed: partly live.
		if len(v.PathSpeeds) < 3 || v.PathSpeeds[0] == nil || *v.PathSpeeds[0] <= 5 || v.PathSpeeds[1] == nil || math.Abs(*v.PathSpeeds[1]-12) > 0.15 {
			t.Fatalf("path_speeds %s", body)
		}
		if last := v.PathSpeeds[len(v.PathSpeeds)-1]; last == nil || math.Abs(*last-5) > 0.05 {
			t.Fatalf("past the traffic the usual speed: %v", deref(last))
		}
		return
	}
	t.Fatalf("no vehicle v: %s", body)
}
