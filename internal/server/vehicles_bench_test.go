package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/release"
)

// benchApp publishes n vehicles spread over the lines of a feed, each somewhere along one of its
// line's trips, with the speed and path a moving matched vehicle gets. ATRT_BENCH_STATE=<state
// dir> uses the real snapshot (real shapes, real path sizes); otherwise a synthetic line.
func benchApp(b *testing.B, n int) *App {
	b.Helper()
	var f *gtfs.Feed
	if dir := os.Getenv("ATRT_BENCH_STATE"); dir != "" {
		var err error
		if f, _, err = (&release.Fetcher{Dir: dir}).LoadLocal(); err != nil || f == nil {
			b.Fatalf("snapshot in %s: %v", dir, err)
		}
	} else {
		f = gtfstest.StraightLine{Stops: 30, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 2}.Feed(b)
	}
	now := monday1020
	a := &App{log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now },
		lines: map[string]*lineData{}, motion: newMotion(nil)}
	m := match.New(f, nil, match.Hungarian)
	a.w = &world{feed: f, matcher: m}
	rng := rand.New(rand.NewPCG(1, 2))
	lines := f.Lines()
	start := time.Now()
	paths, points, tries := 0, 0, 0
	for i := 0; i < n; i++ {
		line := lines[rng.IntN(len(lines))]
		trips := f.TripsForLine(line)
		t := f.Trip(trips[rng.IntN(len(trips))])
		g := m.Geometry(t)
		if g == nil || !g.FromShape {
			if tries++; tries > 100*n {
				b.Fatal("no trips with shape geometry")
			}
			i--
			continue
		}
		next := 1 + rng.IntN(len(g.StopAlong)-1)
		s := g.StopAlong[next-1] + rng.Float64()*(g.StopAlong[next]-g.StopAlong[next-1])
		at := g.Line.At(s)
		lat, lon := latLon(at)
		v := Vehicle{ID: strconv.Itoa(40000 + i), Lat: lat, Lon: lon,
			Bearing: ptr(90.0), PositionAt: now.Unix() - 30, Variant: ptr(f.Shapes[t.Shape].ID), DelayS: ptr(120),
			Speed: ptr(float64(rng.IntN(120)) / 10)}
		v.Path, v.PathBeyond, v.PathStops = pathAhead(g.Line, s, v.Speed, g.StopAlong[next:])
		if v.Path != nil {
			paths++
			points += len(v.Path)
		}
		d := a.lines[line]
		if d == nil {
			d = &lineData{updated: now, feed: f}
			a.lines[line] = d
		}
		d.vehicles = append(d.vehicles, v)
	}
	b.Logf("%d vehicles on %d lines, %d paths, %.1f points/path, motion+path %.1f µs/vehicle",
		n, len(a.lines), paths, float64(points)/float64(max(paths, 1)), float64(time.Since(start).Microseconds())/float64(n))
	return a
}

func latLon(p [2]float64) (float64, float64) {
	lat, lon := geo.LatLon(p)
	return round5(lat), round5(lon)
}

// BenchmarkBuildVehicles: go test ./internal/server -bench BuildVehicles -benchmem
func BenchmarkBuildVehicles(b *testing.B) {
	a := benchApp(b, 1500)
	s, err := a.buildVehicles(monday1020)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := a.buildVehicles(monday1020); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(s.raw))/1024, "raw-KB")
	b.ReportMetric(float64(len(s.gz))/1024, "gzip-KB")
}

// prodApp publishes a recorded /v1/vehicles body (ATRT_BENCH_VEHICLES=<file>) as it was, at
// its updated_at.
func prodApp(b *testing.B) (*App, time.Time) {
	b.Helper()
	file := os.Getenv("ATRT_BENCH_VEHICLES")
	if file == "" {
		b.Skip("ATRT_BENCH_VEHICLES=<recorded /v1/vehicles body> not set")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		b.Fatal(err)
	}
	var r VehiclesResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		b.Fatal(err)
	}
	f := gtfstest.StraightLine{Stops: 2, Trips: 1, First: 10 * 3600, Headway: 10, Leg: 2}.Feed(b)
	now := time.Unix(r.UpdatedAt, 0)
	a := &App{log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now },
		lines: map[string]*lineData{}, motion: newMotion(nil), w: &world{feed: f}}
	for _, c := range r.Vehicles {
		d := a.lines[c.Line]
		if d == nil {
			d = &lineData{updated: now, feed: f}
			a.lines[c.Line] = d
		}
		d.vehicles = append(d.vehicles, Vehicle{ID: c.ID, Lat: c.Lat, Lon: c.Lon, Bearing: c.Bearing,
			PositionAt: c.PositionAt, Variant: c.Variant, DelayS: c.DelayS, Speed: c.Speed, Path: c.Path,
			PathBeyond: c.PathBeyond, PathStops: c.PathStops})
	}
	return a, now
}

// BenchmarkBuildProd: ATRT_BENCH_VEHICLES=v.json go test ./internal/server -bench BuildProd -benchmem
// Reports the snapshot (/v1/vehicles and all tiles) and logs what a map view around Syntagma
// fetches at each MapLibre zoom.
func BenchmarkBuildProd(b *testing.B) {
	a, now := prodApp(b)
	s, err := a.buildVehicles(now)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := a.buildVehicles(now); err != nil {
			b.Fatal(err)
		}
	}
	held, tileGz := len(s.raw)+len(s.gz)+len(s.empty.raw)+len(s.empty.gz), 0
	for _, e := range s.tiles {
		held += len(e.raw) + len(e.gz)
		tileGz += len(e.gz)
	}
	b.ReportMetric(float64(len(s.raw))/1024, "raw-KB")
	b.ReportMetric(float64(len(s.gz))/1024, "gzip-KB")
	b.ReportMetric(float64(len(s.tiles)), "tiles")
	b.ReportMetric(float64(tileGz)/1024, "tiles-gzip-KB")
	b.ReportMetric(float64(held)/1024, "held-KB")
	for _, screen := range []struct {
		name string
		w, h float64
	}{{"phone", 400, 800}, {"desktop", 1920, 1080}} {
		for zoom := 10; zoom <= 15; zoom++ {
			n, vehicles, raw, gz := viewTiles(s, 37.9755, 23.7348, screen.w, screen.h, zoom)
			b.Logf("%-7s zoom %d: %3d tiles, %3d vehicles, %6.1f KB raw, %5.1f KB gzip", screen.name, zoom, n, vehicles,
				float64(raw)/1024, float64(gz)/1024)
		}
	}
}

// viewTiles is what a w×h px MapLibre view (512 px tiles) at zoom centred on lat/lon fetches:
// the tiles of its LOD that touch the view padded by 300 m and clamped to tileBox.
func viewTiles(s *vehiclesSnapshot, lat, lon, w, h float64, zoom int) (n, vehicles, raw, gz int) {
	mpp := 40075016.7 / 512 * math.Cos(lat*math.Pi/180) / math.Exp2(float64(zoom))
	dLat := (h/2*mpp + 300) / 111320
	dLon := (w/2*mpp + 300) / (111320 * math.Cos(lat*math.Pi/180))
	z := tileOverviewZ
	if zoom >= 13 {
		z = tileDetailZ
	}
	x0, y0 := tileOf(min(lat+dLat, tileBox.north), max(lon-dLon, tileBox.west), z)
	x1, y1 := tileOf(max(lat-dLat, tileBox.south), min(lon+dLon, tileBox.east), z)
	for x := x0; x <= x1; x++ {
		for y := y0; y <= y1; y++ {
			e := s.tiles[tileKey{z, x, y}]
			if e == nil {
				e = s.empty
			}
			var r struct{ Vehicles []json.RawMessage }
			json.Unmarshal(e.raw, &r)
			n, vehicles, raw, gz = n+1, vehicles+len(r.Vehicles), raw+len(e.raw), gz+len(e.gz)
		}
	}
	return n, vehicles, raw, gz
}
