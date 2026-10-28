package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/meta"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

var monday1020 = time.Date(2026, 10, 5, 10, 20, 0, 0, gtfs.Athens)

func newApp(t *testing.T, now time.Time) *App {
	t.Helper()
	return newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
}

func newAppWith(t *testing.T, cfg sched.Config, clock func() time.Time) *App {
	t.Helper()
	now := clock()
	dir := t.TempDir()
	m := meta.Data{FetchedAt: now.Unix(), Lines: []telematics.Line{{LineCode: "R1", LineID: "L"}, {LineCode: "77", LineID: "Α1"}},
		Routes: map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "77": {{RouteCode: "7701"}}}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "telematics-meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(Config{StateDir: dir, RPS: 4, Matcher: match.Memory, Sched: cfg},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.now = clock
	a.sched.SetClock(a.now)
	// Tests never reach OASA: the daily schedule is down unless a test sets it (GTFS fallback).
	a.daily = func(context.Context, string) (telematics.DailySchedule, error) {
		return telematics.DailySchedule{}, errors.New("no daily schedule in tests")
	}
	a.stopArrivals = func(context.Context, string) ([]telematics.Arrival, error) {
		return nil, errors.New("no stop arrivals in tests")
	}
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 5}.Feed(t), nil)
	return a
}

func get(t *testing.T, a *App, path string) (*http.Response, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("%s: no CORS header", path)
	}
	return res, body
}

func maxAge(t *testing.T, res *http.Response) int {
	cc := res.Header.Get("Cache-Control")
	v, err := strconv.Atoi(strings.TrimPrefix(cc, "public, max-age="))
	if err != nil {
		t.Fatalf("Cache-Control %q", cc)
	}
	return v
}

func poll(a *App, at time.Time) {
	a.onLine(sched.LinePoll{Line: "L", Started: at, Done: at, Routes: 1, Obs: []match.Obs{
		{Vehicle: telematics.Vehicle{VehNo: "44548", RouteCode: "SH", Lat: 37.98, Lon: 23.725, Heading: 90, Time: at.Add(-10 * time.Second)}, LineCode: "R1"},
		{Vehicle: telematics.Vehicle{VehNo: "9", RouteCode: "SH", Lat: 38.5, Lon: 23.0, Time: at.Add(-5 * time.Second)}, LineCode: "R1"},
	}})
}

func TestLineLifecycle(t *testing.T) {
	a := newApp(t, monday1020)
	res, body := get(t, a, "/v1/lines/L")
	if res.StatusCode != 503 || !strings.Contains(string(body), `"warming_up"`) || maxAge(t, res) != 5 {
		t.Fatalf("before first poll: %d %s", res.StatusCode, body)
	}
	if !a.sched.IsWatched("L") {
		t.Fatal("request did not mark the line watched")
	}
	poll(a, monday1020)
	res, body = get(t, a, "/v1/lines/L")
	if res.StatusCode != 200 {
		t.Fatalf("status %d %s", res.StatusCode, body)
	}
	if ma := maxAge(t, res); ma < 5 || ma > 300 {
		t.Fatalf("max-age %d", ma)
	}
	var lr LineResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatal(err)
	}
	if lr.Line != "L" || lr.UpdatedAt != monday1020.Unix() || lr.NextUpdateAt < monday1020.Unix()+5 || len(lr.Vehicles) != 2 {
		t.Fatalf("response %+v", lr)
	}
	v := lr.Vehicles[0]
	// 2.5 stops along at 10:19:50: T00 is 440 s late (cost 440), T01 160 s early (cost 3x160).
	if v.TripID == nil || *v.TripID != "T00" || v.Variant == nil || *v.Variant != "SH" || v.NextStopID == nil ||
		*v.NextStopID != "S3" || v.TripLabel == nil || *v.TripLabel != "10:00 STOP 0 → STOP 4" || v.DelayS == nil || *v.DelayS != 440 ||
		v.Bearing == nil || *v.Bearing != 90 || v.PositionAt != monday1020.Unix()-10 {
		b, _ := json.Marshal(v)
		t.Fatalf("matched vehicle %s", b)
	}
	u := lr.Vehicles[1]
	if u.TripID != nil || u.DelayS != nil || u.TripLabel != nil || u.NextStopID != nil || u.Bearing != nil ||
		u.Variant == nil || *u.Variant != "SH" {
		b, _ := json.Marshal(u)
		t.Fatalf("unmatched vehicle %s", b)
	}
}

// The JSON field names must be exactly those of the contract fixture.
func TestLineJSONMatchesFixture(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)
	_, body := get(t, a, "/v1/lines/L")
	fix, err := os.ReadFile("../../docs/fixtures/line-040.json")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keys(t, body), keys(t, fix); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys %v, fixture %v", got, want)
	}
	_, body = get(t, a, "/v1/vehicles")
	fix, _ = os.ReadFile("../../docs/fixtures/vehicles.json")
	if got, want := keys(t, body), keys(t, fix); !reflect.DeepEqual(got, want) {
		t.Fatalf("vehicles keys %v, fixture %v", got, want)
	}
	_, body = get(t, a, "/v1/status")
	fix, _ = os.ReadFile("../../docs/fixtures/status.json")
	if got, want := keys(t, body), keys(t, fix); !reflect.DeepEqual(got, want) {
		t.Fatalf("status keys %v, fixture %v", got, want)
	}
}

func keys(t *testing.T, b []byte) []string {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k, v := range m {
		out = append(out, k)
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			for k2 := range arr[0].(map[string]any) {
				out = append(out, k+"."+k2)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestUnknownAndLatinLines(t *testing.T) {
	a := newApp(t, monday1020)
	res, body := get(t, a, "/v1/lines/999")
	if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_line"`) {
		t.Fatalf("unknown: %d %s", res.StatusCode, body)
	}
	// "A1" (Latin) and the percent-encoded Greek id both reach line Α1 (known, no GTFS trips).
	for _, p := range []string{"/v1/lines/A1", "/v1/lines/a1", "/v1/lines/%CE%911"} {
		res, body = get(t, a, p)
		if res.StatusCode != 200 || !strings.Contains(string(body), `"line":"Α1"`) || maxAge(t, res) != 120 {
			t.Fatalf("%s: %d %s", p, res.StatusCode, body)
		}
	}
}

func TestWarmingWhileRoutesUnknown(t *testing.T) {
	a := newApp(t, monday1020)
	// First boot: no metadata at all yet.
	a.meta = meta.Open(filepath.Join(t.TempDir(), "none.json"), a.client, func(func(context.Context) error) {}, nil)
	a.refreshLines()
	a.mu.Lock()
	a.known["L"] = true // known from the GTFS, routes unknown
	a.mu.Unlock()
	res, body := get(t, a, "/v1/lines/L")
	if res.StatusCode != 503 || !strings.Contains(string(body), "warming_up") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestInactiveLineAtNight(t *testing.T) {
	a := newApp(t, time.Date(2026, 10, 5, 3, 0, 0, 0, gtfs.Athens))
	res, body := get(t, a, "/v1/lines/L")
	if res.StatusCode != 200 || !strings.Contains(string(body), `"vehicles":[]`) || maxAge(t, res) != 120 {
		t.Fatalf("inactive: %d %s", res.StatusCode, body)
	}
}

func TestStatusAndFeeds(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)
	res, body := get(t, a, "/v1/status")
	var st StatusResponse
	json.Unmarshal(body, &st)
	if maxAge(t, res) != 10 || !st.OK || st.GTFSExpires != "2026-10-06" || st.GTFSVersion != "2026-07-06" ||
		st.LinesActive != 1 || st.BudgetRPS != 4 || st.UpdatedAt != monday1020.Unix() {
		t.Fatalf("status %s", body)
	}
	res, body = get(t, a, "/v1/gtfs-rt/vehicle_positions.pb")
	var msg rt.FeedMessage
	if err := proto.Unmarshal(body, &msg); err != nil || len(msg.Entity) != 2 || maxAge(t, res) != 15 {
		t.Fatalf("vehicle positions: %v %d", err, len(msg.Entity))
	}
	_, body = get(t, a, "/v1/gtfs-rt/trip_updates.json")
	if !strings.Contains(string(body), `"tripId": "T00"`) {
		t.Fatalf("trip updates json %s", body)
	}
	if res, _ := get(t, a, "/v1/gtfs-rt/other.pb"); res.StatusCode != 404 {
		t.Fatal("unknown feed name served")
	}
}

func TestCanonicalLine(t *testing.T) {
	for in, want := range map[string]string{" a1 ": "Α1", "x96": "Χ96", "040": "040", "Α1": "Α1", "e14": "Ε14"} {
		if got := CanonicalLine(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestHostileLineIDs(t *testing.T) {
	a := newApp(t, monday1020)
	for _, p := range []string{"/v1/lines/..%2Fetc%2Fpasswd", "/v1/lines/../etc/passwd", "/v1/lines/./040", "/v1//lines/040", "/v1/lines/%00", "/v1/lines/" + strings.Repeat("x", 2048),
		"/v1/lines/%FF%FE", "/v1/nothing"} {
		res, body := get(t, a, p)
		if res.StatusCode != 404 || !json.Valid(body) {
			t.Fatalf("%.40s: %d %s", p, res.StatusCode, body)
		}
		if ma := maxAge(t, res); ma > 60 {
			t.Fatalf("%.40s: 404 cached %d s", p, ma)
		}
	}
	if a.sched.IsWatched("..%2FETC%2FPASSWD") || a.sched.IsWatched("../ETC/PASSWD") || a.sched.IsWatched("\x00") {
		t.Fatal("unknown id entered the watched set")
	}
}

func TestGzipWhenAsked(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)
	for _, p := range []string{"/v1/lines/L", "/v1/status", "/v1/gtfs-rt/vehicle_positions.pb", "/v1/lines/999"} {
		plainRes, plain := get(t, a, p)
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Accept-Encoding", "br, gzip")
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		res := rec.Result()
		if res.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") ||
			res.Header.Get("Access-Control-Allow-Origin") != "*" || res.StatusCode != plainRes.StatusCode {
			t.Fatalf("%s: %d %v", p, res.StatusCode, res.Header)
		}
		zr, err := gzip.NewReader(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(zr)
		if string(got) != string(plain) {
			t.Fatalf("%s: gzip body differs", p)
		}
	}
}

func TestMetricsLocalOnly(t *testing.T) {
	a := newApp(t, monday1020)
	get(t, a, "/v1/lines/L") // watch it
	a.client.Log.Add(monday1020)
	a.client.Log.Add(monday1020)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	a.MetricsHandler().ServeHTTP(rec, req)
	var m Metrics
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body)
	}
	if len(m.RequestsPerSecond) != telematics.RequestLogSeconds || m.Max1s != 2 || m.Max10s != 2 ||
		m.BudgetRPS != 4 || m.LinesByTier["watched"] != 1 || len(m.Lines) == 0 {
		t.Fatalf("metrics max %d/%d budget %v tiers %v lines %d", m.Max1s, m.Max10s, m.BudgetRPS, m.LinesByTier, len(m.Lines))
	}
	var l *MetricsLine
	for i := range m.Lines {
		if m.Lines[i].ID == "L" {
			l = &m.Lines[i]
		}
	}
	if l == nil || l.Tier != "watched" || l.IntervalS != 30 {
		t.Fatalf("line L %+v", l)
	}
	rec = httptest.NewRecorder()
	req.RemoteAddr = "203.0.113.9:5555"
	a.MetricsHandler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("remote metrics: %d", rec.Code)
	}
}

// B7: OASA answers garbage for a while. The request rate drops, the last good data stays
// served, /v1/status turns ok=false after 5 minutes without a good poll, and everything
// recovers once OASA does.
func TestOASAOutageBackoffAndRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("runs ~15 s of real pacing")
	}
	var offset atomic.Int64
	start := time.Now()
	clock := func() time.Time { return monday1020.Add(time.Since(start) + time.Duration(offset.Load())) }
	var down atomic.Bool
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
			return
		}
		if r.URL.Query().Get("act") != "getBusLocation" {
			w.Write([]byte("[]"))
			return
		}
		fmt.Fprintf(w, `[{"VEH_NO":"44548","CS_DATE":%q,"CS_LAT":"37.98","CS_LNG":"23.725","ROUTE_CODE":"SH","VEH_HEADING":"90"}]`,
			telematics.FormatCSDate(clock().Add(-10*time.Second)))
	}))
	defer fake.Close()
	cfg := sched.DefaultConfig()
	fast := sched.TierConfig{Base: 300 * time.Millisecond, Min: 300 * time.Millisecond}
	cfg.Watched, cfg.Dense, cfg.Other = fast, fast, fast
	a := newAppWith(t, cfg, clock)
	a.client.BaseURL = fake.URL + "/"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.sched.Run(ctx)

	waitFor := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(limit); !cond(); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s (rps %.2f)", what, a.pacer.RPS())
			}
		}
	}
	lineHasVehicle := func() (bool, int64) {
		res, body := get(t, a, "/v1/lines/L")
		var lr LineResponse
		json.Unmarshal(body, &lr)
		return res.StatusCode == 200 && len(lr.Vehicles) == 1, lr.UpdatedAt
	}
	status := func() bool {
		_, body := get(t, a, "/v1/status")
		var st StatusResponse
		json.Unmarshal(body, &st)
		return st.OK
	}
	waitFor("first data", 5*time.Second, func() bool { ok, _ := lineHasVehicle(); return ok })

	down.Store(true)
	waitFor("backoff", 10*time.Second, func() bool { return a.pacer.RPS() <= 2 })
	ok, before := lineHasVehicle()
	time.Sleep(time.Second)
	if ok2, after := lineHasVehicle(); !ok || !ok2 || after != before {
		t.Fatalf("last good data not kept during the outage: %v %v %d %d", ok, ok2, before, after)
	}
	if !status() {
		t.Fatal("status not ok right after the outage started")
	}
	offset.Store(int64(6 * time.Minute))
	if status() {
		t.Fatal("status still ok 6 minutes into the outage")
	}

	down.Store(false)
	waitFor("recovery", 30*time.Second, func() bool { return a.pacer.RPS() >= 3.9 && status() })
	if _, after := lineHasVehicle(); after <= before {
		t.Fatal("no fresh data after recovery")
	}
}

// D3: after the static GTFS expires, lines are still polled (on the weekday pattern of the
// feed's last valid week) and vehicles are served with position and route, unmatched.
func TestExpiredGTFSStillServesVehicles(t *testing.T) {
	after := time.Date(2026, 10, 12, 10, 20, 0, 0, gtfs.Athens) // Monday; feed ended 2026-10-06
	a := newApp(t, after)
	var logs strings.Builder
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.warnExpiry(true)
	if !strings.Contains(logs.String(), "static GTFS expired") {
		t.Fatalf("no expiry warning: %s", logs.String())
	}
	if _, polled := a.sched.NextUpdate("L"); !polled {
		t.Fatal("line not polled after the GTFS expired")
	}
	poll(a, after)
	res, body := get(t, a, "/v1/lines/L")
	var lr LineResponse
	json.Unmarshal(body, &lr)
	if res.StatusCode != 200 || len(lr.Vehicles) == 0 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	matched := 0
	for _, v := range lr.Vehicles {
		if v.RouteCode != "SH" || v.Lat == 0 {
			t.Fatalf("vehicle %+v", v)
		}
		if v.TripID != nil && v.DelayS != nil {
			matched++
		}
	}
	if matched == 0 { // trips still match by the same weekday inside the feed's validity
		t.Fatalf("no vehicle matched to a trip after the GTFS expired: %s", body)
	}
	_, body = get(t, a, "/v1/status")
	var st StatusResponse
	json.Unmarshal(body, &st)
	if st.GTFSExpires != "2026-10-06" || !st.OK {
		t.Fatalf("status %s", body)
	}
}

// A vehicle that moved to another line within feedMaxAge appears once in the feeds (the
// newest fix wins): duplicate entity ids are invalid GTFS-RT.
func TestFeedHasEachVehicleOnce(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020.Add(-time.Minute))
	a.onLine(sched.LinePoll{Line: "Α1", Started: monday1020, Done: monday1020, Routes: 1, Obs: []match.Obs{
		{Vehicle: telematics.Vehicle{VehNo: "44548", RouteCode: "7701", Lat: 37.99, Lon: 23.73, Time: monday1020.Add(-5 * time.Second)}, LineCode: "77"},
	}})
	_, body := get(t, a, "/v1/gtfs-rt/vehicle_positions.pb")
	var msg rt.FeedMessage
	if err := proto.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range msg.Entity {
		seen[e.GetId()]++
		if e.GetId() == "vehicle-44548" && e.GetVehicle().GetPosition().GetLatitude() != 37.99 {
			t.Fatalf("older fix kept: %v", e.GetVehicle().GetPosition())
		}
	}
	if seen["vehicle-44548"] != 1 {
		t.Fatalf("entities %v", seen)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for h, want := range map[string]bool{"gzip": true, "br, gzip": true, "gzip;q=0.5": true, "GZIP": true,
		"gzip;q=0": false, "gzip; q=0.0": false, "x-gzip2": false, "": false, "identity": false, "*": true, "*;q=0": false} {
		if got := acceptsGzip(h); got != want {
			t.Errorf("%q: %v, want %v", h, got, want)
		}
	}
}

// --proxy: every OASA request goes through the proxy (OASA blocks datacenter addresses).
func TestTelematicsThroughProxy(t *testing.T) {
	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "oasa.invalid" {
			proxied.Add(1)
		}
		w.Write([]byte("[]"))
	}))
	defer proxy.Close()
	dir := t.TempDir()
	a := New(Config{StateDir: dir, RPS: 4, Matcher: match.Memory, Sched: sched.DefaultConfig(),
		TelematicsURL: "http://oasa.invalid/api/", Proxy: proxy.URL}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := a.client.BusLocations(context.Background(), "1"); err != nil || proxied.Load() != 1 {
		t.Fatalf("err %v proxied %d", err, proxied.Load())
	}
}
