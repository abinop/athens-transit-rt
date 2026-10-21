package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

func TestTileOf(t *testing.T) {
	for _, c := range []struct {
		lat, lon float64
		z, x, y  int
	}{
		{37.9755, 23.7348, 13, 4636, 3160}, // Syntagma
		{37.9755, 23.7348, 9, 289, 197},
		{0, 0, 10, 512, 512},
		{0, -180, 13, 0, 4096},
		{38.5, 22.9, 13, 4617, 3145}, // tileBox corners
		{37.5, 24.5, 13, 4653, 3174},
	} {
		if x, y := tileOf(c.lat, c.lon, c.z); x != c.x || y != c.y {
			t.Errorf("tileOf(%v, %v, %d) = %d/%d, want %d/%d", c.lat, c.lon, c.z, x, y, c.x, c.y)
		}
	}
	for z, want := range map[int][4]int{9: {288, 290, 196, 198}, 13: {4617, 4653, 3145, 3174}} {
		if x0, x1, y0, y1 := tileRange(z); [4]int{x0, x1, y0, y1} != want {
			t.Errorf("tileRange(%d) = %d..%d, %d..%d", z, x0, x1, y0, y1)
		}
	}
}

func tilePath(z int, lat, lon float64) string {
	x, y := tileOf(lat, lon, z)
	return fmt.Sprintf("/v1/vehicles/tiles/%d/%d/%d", z, x, y)
}

// rawVehicles decodes a vehicles body keeping each vehicle as its JSON object.
func rawVehicles(t *testing.T, b []byte) (int64, int64, map[string]map[string]json.RawMessage) {
	t.Helper()
	var r struct {
		UpdatedAt    int64                        `json:"updated_at"`
		NextUpdateAt int64                        `json:"next_update_at"`
		Vehicles     []map[string]json.RawMessage `json:"vehicles"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Vehicles == nil {
		t.Fatalf("%v: %s", err, b)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, v := range r.Vehicles {
		var id string
		json.Unmarshal(v["id"], &id)
		out[id] = v
	}
	return r.UpdatedAt, r.NextUpdateAt, out
}

// TestVehicleTiles: every vehicle of /v1/vehicles is in exactly one tile per zoom, z13 with the
// same entry, z10 without the path fields, all from the same build.
func TestVehicleTiles(t *testing.T) {
	now := monday1020
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
	step := 300 / (geo.XY(37.98, 23.71)[0] - geo.XY(37.98, 23.70)[0]) * 0.01
	pollOne(a, now, "v", 23.72, now.Add(-20*time.Second))
	now = now.Add(30 * time.Second)
	pollOne(a, now, "v", 23.72+step, now.Add(-20*time.Second)) // moving: has path, path_beyond, path_stops
	a.onLine(sched.LinePoll{Line: "Α1", Started: now, Done: now, Routes: 1, Obs: []match.Obs{
		{Vehicle: telematics.Vehicle{VehNo: "far", RouteCode: "7701", Lat: 38.5, Lon: 22.9, Time: now}, LineCode: "77"},
		{Vehicle: telematics.Vehicle{VehNo: "out", RouteCode: "7701", Lat: 38.6, Lon: 23.7, Time: now}, LineCode: "77"},
		// North of the box but still in the top row of z13 tiles: in the tiles.
		{Vehicle: telematics.Vehicle{VehNo: "edge", RouteCode: "7701", Lat: 38.501, Lon: 23.7, Time: now}, LineCode: "77"},
	}})
	_, body := get(t, a, "/v1/vehicles")
	upd, next, all := rawVehicles(t, body)
	if len(all) != 4 || all["v"]["path"] == nil || string(all["v"]["path"]) == "null" {
		t.Fatalf("setup: %s", body)
	}
	for _, z := range []int{9, 13} {
		seen := map[string]bool{}
		for _, id := range []string{"v", "far", "edge"} {
			var lat, lon float64
			json.Unmarshal(all[id]["lat"], &lat)
			json.Unmarshal(all[id]["lon"], &lon)
			p := tilePath(z, lat, lon)
			res, tb := get(t, a, p)
			if res.StatusCode != 200 {
				t.Fatalf("%s: %d %s", p, res.StatusCode, tb)
			}
			tu, tn, tv := rawVehicles(t, tb)
			if tu != upd || tn != next {
				t.Fatalf("%s: other build %d/%d vs %d/%d", p, tu, tn, upd, next)
			}
			for vid, got := range tv {
				if seen[vid] {
					t.Fatalf("z%d: %s in two tiles", z, vid)
				}
				seen[vid] = true
				want := all[vid]
				if z == 13 && !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: %s differs from /v1/vehicles", p, vid)
				}
				if z == 9 {
					for k, v := range want {
						_, has := got[k]
						switch k {
						case "path", "path_beyond", "path_stops", "path_speeds":
							if has {
								t.Fatalf("%s: %s has %s", p, vid, k)
							}
						default:
							if !bytes.Equal(got[k], v) {
								t.Fatalf("%s: %s.%s %s, want %s", p, vid, k, got[k], v)
							}
						}
					}
					if len(got) != len(want)-4 {
						t.Fatalf("%s: keys %v", p, got)
					}
				}
			}
		}
		if !seen["v"] || !seen["far"] || !seen["edge"] || seen["out"] {
			t.Fatalf("z%d: vehicles %v (out of the tile domain must be in none)", z, seen)
		}
	}
	// An empty tile in the domain: the shared empty body of this build.
	res, tb := get(t, a, "/v1/vehicles/tiles/13/4650/3150")
	if tu, _, tv := rawVehicles(t, tb); res.StatusCode != 200 || len(tv) != 0 || tu != upd || maxAge(t, res) != 30 {
		t.Fatalf("empty tile: %d %s", res.StatusCode, tb)
	}
	if a.sched.IsWatched("L") || a.sched.IsWatched("Α1") {
		t.Fatal("a tile marked a line watched")
	}
	// The next build moves "v" to another tile.
	pollOne(a, now, "v", 23.80, now)
	now = now.Add(vehiclesEvery)
	_, tb = get(t, a, tilePath(13, 37.98, 23.72+step))
	if _, _, tv := rawVehicles(t, tb); len(tv) != 0 {
		t.Fatalf("old tile still has v: %s", tb)
	}
	_, tb = get(t, a, tilePath(13, 37.98, 23.80))
	if _, _, tv := rawVehicles(t, tb); tv["v"] == nil {
		t.Fatalf("new tile lacks v: %s", tb)
	}
}

func TestTileHTTP(t *testing.T) {
	now := monday1020
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
	poll(a, now)
	p := tilePath(13, 37.98, 23.725)
	res, raw := do(a, "GET", p, nil)
	resZ, gz := do(a, "GET", p, map[string]string{"Accept-Encoding": "gzip"})
	if res.StatusCode != 200 || res.Header.Get("Content-Encoding") != "" || resZ.Header.Get("Content-Encoding") != "gzip" ||
		res.Header.Get("Vary") != "Accept-Encoding" || resZ.Header.Get("Access-Control-Allow-Origin") != "*" ||
		res.Header.Get("Content-Type") != "application/json; charset=utf-8" || res.Header.Get("Cache-Control") != "public, max-age=30" {
		t.Fatalf("headers %d %v / %v", res.StatusCode, res.Header, resZ.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	unz, _ := io.ReadAll(zr)
	if !bytes.Equal(unz, raw) || res.Header.Get("Content-Length") == "" || !bytes.Contains(raw, []byte(`"44548"`)) {
		t.Fatalf("gzip body differs from raw: %s", raw)
	}
	etag, gzETag := res.Header.Get("ETag"), resZ.Header.Get("ETag")
	_, all := do(a, "GET", "/v1/vehicles", nil)
	_, allZ := do(a, "GET", "/v1/vehicles", map[string]string{"Accept-Encoding": "gzip"})
	if etag == "" || etag == gzETag || bytes.Equal(raw, all) || bytes.Equal(gz, allZ) {
		t.Fatalf("etags %q %q", etag, gzETag)
	}
	for _, inm := range []string{etag, "W/" + etag, "*"} {
		r, b := do(a, "GET", p, map[string]string{"If-None-Match": inm})
		if r.StatusCode != 304 || len(b) != 0 || r.Header.Get("ETag") != etag || r.Header.Get("Cache-Control") == "" {
			t.Fatalf("If-None-Match %s: %d %q", inm, r.StatusCode, b)
		}
	}
	if r, _ := do(a, "GET", p, map[string]string{"If-None-Match": gzETag, "Accept-Encoding": "gzip"}); r.StatusCode != 304 {
		t.Fatalf("gzip If-None-Match: %d", r.StatusCode)
	}
	if r, b := do(a, "HEAD", p, nil); r.StatusCode != 200 || len(b) != 0 || r.Header.Get("Content-Length") != res.Header.Get("Content-Length") {
		t.Fatalf("HEAD: %d %d bytes", r.StatusCode, len(b))
	}
	// Empty tiles are gzipped too, with their own ETags.
	re, _ := do(a, "GET", "/v1/vehicles/tiles/9/288/196", nil)
	reZ, _ := do(a, "GET", "/v1/vehicles/tiles/9/288/196", map[string]string{"Accept-Encoding": "gzip"})
	if re.StatusCode != 200 || reZ.Header.Get("Content-Encoding") != "gzip" || re.Header.Get("ETag") == reZ.Header.Get("ETag") {
		t.Fatalf("empty tile: %d %v", re.StatusCode, reZ.Header)
	}
	// Max-age counts down to the next build, like /v1/vehicles.
	now = now.Add(26 * time.Second)
	if r, b := do(a, "GET", p, nil); maxAge(t, r) != 5 || !bytes.Equal(b, raw) {
		t.Fatalf("later: max-age %d", maxAge(t, r))
	}
	now = now.Add(4 * time.Second)
	if r, b := do(a, "GET", p, nil); maxAge(t, r) != 30 || bytes.Equal(b, raw) {
		t.Fatalf("not rebuilt: max-age %d %s", maxAge(t, r), b)
	}

	for path, want := range map[string]int{
		"/v1/vehicles/tiles/12/2318/1580":     404, // only z 9 and 13
		"/v1/vehicles/tiles/10/579/395":       404,
		"/v1/vehicles/tiles/13/4616/3160":     404, // west of the domain
		"/v1/vehicles/tiles/13/4654/3160":     404,
		"/v1/vehicles/tiles/13/4636/3144":     404,
		"/v1/vehicles/tiles/13/4636/3175":     404,
		"/v1/vehicles/tiles/9/287/197":        404,
		"/v1/vehicles/tiles/13/04636/3160":    404, // not canonical: another cache key
		"/v1/vehicles/tiles/13/+4636/3160":    404,
		"/v1/vehicles/tiles/13/%34636/3160":   404, // escaped alias
		"/v1/vehicles/tiles/13/4636/3160/":    404,
		"/v1/vehicles/tiles/13/4636":          404,
		"/v1/vehicles/tiles/013/4636/3160":    404,
		"/v1/vehicles/tiles/13/4636/x":        404,
		"/v1/vehicles/tiles/13/4636/3160?t=1": 400,
		"/v1/vehicles/tiles/13/4636/3160?":    400,
		"/v1/vehicles/tiles/13/4636/3160":     200,
	} {
		r, b := do(a, "GET", path, map[string]string{"Accept-Encoding": "gzip"})
		if r.StatusCode != want {
			t.Errorf("%s: %d %s, want %d", path, r.StatusCode, b, want)
			continue
		}
		switch want {
		case 400:
			if r.Header.Get("Cache-Control") != "no-store" || !bytes.Contains(b, []byte(`"query_not_allowed"`)) {
				t.Errorf("%s: %v %s", path, r.Header, b)
			}
		case 404:
			if r.Header.Get("Cache-Control") != "public, max-age=60" {
				t.Errorf("%s: %v", path, r.Header)
			}
		}
	}
	if r, _ := do(a, "POST", p, nil); r.StatusCode != 404 {
		t.Fatalf("POST: %d", r.StatusCode)
	}
}

// The JSON field names of both tile zooms must be exactly those of the contract fixtures.
func TestTileJSONMatchesFixture(t *testing.T) {
	a := newApp(t, monday1020)
	pollOne(a, monday1020, "v", 23.72, monday1020)
	for _, c := range []struct {
		z       int
		fixture string
	}{
		{13, "vehicles-tile-13-4636-3160.json"}, {9, "vehicles-tile-9-289-197.json"},
	} {
		_, body := get(t, a, tilePath(c.z, 37.98, 23.72))
		fix, err := os.ReadFile("../../docs/fixtures/" + c.fixture)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := keys(t, body), keys(t, fix); !reflect.DeepEqual(got, want) {
			t.Fatalf("z%d keys %v, fixture %v", c.z, got, want)
		}
	}
}
