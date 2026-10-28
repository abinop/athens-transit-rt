package rail

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

// fakeRail is the upstream: railway.gov.gr and ose.mpass.ltd pages, served from constants, with
// a hit counter and a switch that makes every request fail.
type fakeRail struct {
	mu   sync.Mutex
	hits map[string]int
	fail bool
}

func (f *fakeRail) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func (f *fakeRail) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeRail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.URL.Path]++
	fail := f.fail
	f.mu.Unlock()
	if fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	switch r.URL.Path {
	case "/api/public/corridor-stations":
		io.WriteString(w, stationsJSON)
	case "/api/public/schedule-board":
		io.WriteString(w, boardJSON)
	default:
		if strings.HasPrefix(r.URL.Path, "/screens/") {
			io.WriteString(w, mpassJSON)
			return
		}
		http.NotFound(w, r)
	}
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

const stationsJSON = `{"corridors":[
 {"id":"KIATO","name":"Πειραιάς – Κιάτο","stations":[
   {"stationId":"ST-PIR","stationIds":["ST-PIR"],"nameGreek":"Πειραιάς","name":"Πειραιάς"},
   {"stationId":"ST-AIR","stationIds":["ST-AIR","ST-AIR2"],"nameGreek":"Αεροδρόμιο","name":"AIRPORT"}]},
 {"id":"PIRAIR","name":"Πειραιάς – Αεροδρόμιο","stations":[
   {"stationId":"ST-PIR","stationIds":["ST-PIR"],"nameGreek":"Πειραιάς"}]}]}`

// 2026-10-28 is EET (UTC+2): 03:20Z is 05:20 in Athens. The clock of the tests is 05:00.
const boardJSON = `{"schedules":[
 {"trainNumber":"2539","status":"scheduled","delay":2,"destination":{"nameGreek":"Κιάτο"},
  "routeStations":[
    {"stationId":"ST-PIR","nameGreek":"Πειραιάς","role":"origin","index":0,"scheduledDeparture":"2026-10-28T03:20:00.000Z"},
    {"stationId":"ST-REN","nameGreek":"Ρέντης","role":"stop","index":1,"scheduledDeparture":"2026-10-28T03:26:00.000Z","platform":"3","delay":4},
    {"stationId":"ST-KIA","nameGreek":"Κιάτο","role":"destination","index":2,"scheduledArrival":"2026-10-28T04:30:00.000Z"}]},
 {"trainNumber":"1200","status":"scheduled","delay":0,"destination":{"nameGreek":"Κιάτο"},
  "routeStations":[{"stationId":"ST-PIR","nameGreek":"Πειραιάς","role":"origin","index":0,"scheduledDeparture":"2026-10-28T02:00:00.000Z"}]},
 {"trainNumber":"1203","status":"cancelled","delay":0,"destination":{"nameGreek":"Αεροδρόμιο"},
  "routeStations":[{"stationId":"ST-PIR","nameGreek":"Πειραιάς","role":"origin","index":0,"scheduledDeparture":"2026-10-28T03:30:00.000Z"}]}]}`

const mpassJSON = `{"screen":{"id":15,"identifier":28,"area":{"name":"Αεροδρόμιο","nameEl":"Αεροδρόμιο","areaCode":28}},
 "routes":[
  {"routesEl":{"TRENO":"1203 ","FROMTO":"Αεροδρόμιο - Πειραιάς","DR_TAFIX":null,"DR_TANA":" 5:32",
   "DR_FORECASTED_DATE":"2026-10-28T03:32:00Z","DR_PLATFORM":"4","DR_DELAY_TANA":null}},
  {"routesEl":{"TRENO":"1205 ","FROMTO":"Αεροδρόμιο - Πειραιάς","DR_TAFIX":null,"DR_TANA":" 4:10",
   "DR_FORECASTED_DATE":"2026-10-28T02:10:00Z","DR_PLATFORM":"4","DR_DELAY_TANA":null}}]}`

var suburbanNow = time.Date(2026, 10, 28, 5, 0, 0, 0, gtfs.Athens)

func newTestSuburban(t *testing.T) (*Suburban, *testClock, *fakeRail) {
	t.Helper()
	f := &fakeRail{hits: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	clk := &testClock{t: suburbanNow}
	return &Suburban{BaseURL: srv.URL, MPassURL: srv.URL, HTTP: srv.Client(), Now: clk.now}, clk, f
}

func TestSuburbanStations(t *testing.T) {
	s, _, f := newTestSuburban(t)
	list, ok := s.Stations(context.Background())
	if !ok {
		t.Fatal("no stations")
	}
	want := []Station{
		{ID: "ST-PIR", Name: "Πειραιάς", Corridors: []string{"KIATO", "PIRAIR"}},
		{ID: "ST-AIR", Name: "Αεροδρόμιο", Corridors: []string{"KIATO"}},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("stations %+v", list)
	}
	if _, ok := s.Stations(context.Background()); !ok {
		t.Fatal("second call failed")
	}
	if n := f.count("/api/public/corridor-stations"); n != 1 {
		t.Fatalf("fetched %d times, want the 24 h cache to serve the second", n)
	}
}

func TestSuburbanDepartures(t *testing.T) {
	s, _, _ := newTestSuburban(t)
	delay := 2
	tests := []struct {
		name    string
		station string
		want    string
		deps    []SuburbanDeparture
	}{
		{
			name: "origin: no from, schedule delay, past and cancelled left out", station: "ST-PIR", want: "Πειραιάς",
			deps: []SuburbanDeparture{{Train: "2539", From: "", To: "Κιάτο", T: "05:20",
				DelayMin: &delay, Platform: nil, Status: "scheduled"}},
		},
		{
			name: "middle stop: from is the stop before, platform and stop delay", station: "ST-REN", want: "ST-REN",
			deps: []SuburbanDeparture{{Train: "2539", From: "Πειραιάς", To: "Κιάτο", T: "05:26",
				DelayMin: intPtr(4), Platform: strPtr("3"), Status: "scheduled"}},
		},
		{
			name: "alias id of the same station", station: "ST-AIR2", want: "Αεροδρόμιο",
			deps: []SuburbanDeparture{},
		},
		{
			// The last row of a run is its arrival, not a departure of that station.
			name: "terminus has no departure", station: "ST-KIA", want: "ST-KIA",
			deps: []SuburbanDeparture{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.Departures(context.Background(), tc.station, 10, suburbanNow)
			if !ok {
				t.Fatal("no departures")
			}
			if got.Station != tc.want {
				t.Errorf("station %q, want %q", got.Station, tc.want)
			}
			if !reflect.DeepEqual(got.Departures, tc.deps) {
				t.Errorf("departures\n got %+v\nwant %+v", got.Departures, tc.deps)
			}
		})
	}
}

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

func TestSuburbanBoardCache(t *testing.T) {
	s, clk, f := newTestSuburban(t)
	for i := 0; i < 3; i++ {
		if _, ok := s.Departures(context.Background(), "ST-PIR", 10, clk.now()); !ok {
			t.Fatal("no departures")
		}
	}
	if n := f.count("/api/public/schedule-board"); n != 1 {
		t.Fatalf("board fetched %d times in the 10 min cache", n)
	}
	// Past the cache: one new fetch.
	clk.t = clk.t.Add(boardTTL + time.Minute)
	if _, ok := s.Departures(context.Background(), "ST-PIR", 10, clk.now()); !ok {
		t.Fatal("no departures after the cache expired")
	}
	if n := f.count("/api/public/schedule-board"); n != 2 {
		t.Fatalf("board fetched %d times, want 2", n)
	}
}

func TestSuburbanServesStaleOnError(t *testing.T) {
	s, clk, f := newTestSuburban(t)
	if _, ok := s.Stations(context.Background()); !ok {
		t.Fatal("no stations")
	}
	first, ok := s.Departures(context.Background(), "ST-REN", 10, clk.now())
	if !ok {
		t.Fatal("no departures")
	}
	f.setFail(true)
	clk.t = clk.t.Add(boardTTL + time.Minute)
	got, ok := s.Departures(context.Background(), "ST-REN", 10, clk.now())
	if !ok || !reflect.DeepEqual(got, first) {
		t.Fatalf("stale board not served: ok=%v %+v", ok, got)
	}
	// The failure is remembered: the next request does not start another 743 KB download.
	hits := f.count("/api/public/schedule-board")
	if _, ok := s.Departures(context.Background(), "ST-REN", 10, clk.now()); !ok {
		t.Fatal("no departures in the backoff window")
	}
	if n := f.count("/api/public/schedule-board"); n != hits {
		t.Fatalf("retried %d times inside the backoff window", n-hits)
	}
	clk.t = clk.t.Add(stationsTTL + time.Minute)
	if list, ok := s.Stations(context.Background()); !ok || len(list) != 2 {
		t.Fatalf("stale stations not served: %d", len(list))
	}
}

func TestSuburbanMPassFallback(t *testing.T) {
	// railway.gov.gr is down, the mpass screens answer.
	down := &fakeRail{hits: map[string]int{}, fail: true}
	railSrv := httptest.NewServer(down)
	defer railSrv.Close()
	up := &fakeRail{hits: map[string]int{}}
	mpassSrv := httptest.NewServer(up)
	defer mpassSrv.Close()
	s := &Suburban{BaseURL: railSrv.URL, MPassURL: mpassSrv.URL, HTTP: railSrv.Client(),
		Now: func() time.Time { return suburbanNow }}

	got, ok := s.Departures(context.Background(), "6928c3c150ab5bc72b39cec7", 10, suburbanNow)
	if !ok {
		t.Fatal("no fallback departures")
	}
	if got.Station != "Αεροδρόμιο" {
		t.Errorf("station %q, want the screen name", got.Station)
	}
	want := []SuburbanDeparture{{Train: "1203", From: "Αεροδρόμιο", To: "Πειραιάς", T: "05:32",
		DelayMin: nil, Platform: strPtr("4"), Status: "scheduled"}}
	if !reflect.DeepEqual(got.Departures, want) {
		t.Errorf("departures\n got %+v\nwant %+v", got.Departures, want)
	}
	// A station without an mpass screen has no fallback.
	if _, ok := s.Departures(context.Background(), "ST-UNKNOWN", 10, suburbanNow); ok {
		t.Fatal("fallback used for a station with no screen number")
	}
}

func TestSuburbanMPassNotUsedWhenBoardWorks(t *testing.T) {
	s, _, f := newTestSuburban(t)
	if _, ok := s.Departures(context.Background(), "ST-PIR", 10, suburbanNow); !ok {
		t.Fatal("no departures")
	}
	if n := f.count("/screens/2/data/multilingual"); n != 0 {
		t.Fatalf("mpass called %d times", n)
	}
}

// A cold cache at a dead upstream must not fetch on every request.
func TestSuburbanColdOutageBacksOff(t *testing.T) {
	s, clk, f := newTestSuburban(t)
	f.setFail(true)
	for i := 0; i < 3; i++ {
		if _, ok := s.Stations(context.Background()); ok {
			t.Fatal("stations served by a dead upstream")
		}
	}
	if n := f.count("/api/public/corridor-stations"); n != 1 {
		t.Fatalf("stations fetched %d times in the failure window", n)
	}
	for i := 0; i < 3; i++ {
		if _, ok := s.Departures(context.Background(), "ST-PIR", 10, clk.now()); ok {
			t.Fatal("departures served by a dead upstream")
		}
	}
	if n := f.count("/api/public/schedule-board"); n != 1 {
		t.Fatalf("board fetched %d times in the failure window", n)
	}
	// After the window a new attempt is made and succeeds.
	clk.t = clk.t.Add(upstreamRetry + time.Second)
	f.setFail(false)
	if list, ok := s.Stations(context.Background()); !ok || len(list) != 2 {
		t.Fatalf("stations after the window: %d", len(list))
	}
	if _, ok := s.Departures(context.Background(), "ST-PIR", 10, clk.now()); !ok {
		t.Fatal("no departures after the window")
	}
}

const mpassManyJSON = `{"screen":{"area":{"nameEl":"Πειραιάς"}},"routes":[
 {"routesEl":{"TRENO":"1","FROMTO":"Πειραιάς - Κιάτο","DR_TANA":" 5:01"}},
 {"routesEl":{"TRENO":"2","FROMTO":"Πειραιάς - Κιάτο","DR_TANA":" 5:20"}},
 {"routesEl":{"TRENO":"3","FROMTO":"Πειραιάς - Κιάτο","DR_TANA":" 5:40"}}]}`

// countServer is an upstream screen that counts its requests.
func countServer(t *testing.T, body string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond) // a real download takes time: the burst is not serialised by luck
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// The cache keeps the whole screen: every caller gets its own n and its own now.
func TestMPassCacheIsPerRequest(t *testing.T) {
	srv, hits := countServer(t, mpassManyJSON)
	clk := &testClock{t: time.Date(2026, 10, 28, 5, 0, 30, 0, gtfs.Athens)}
	s := &Suburban{MPassURL: srv.URL, HTTP: srv.Client(), Now: clk.now}
	deps, name, ok := s.mpassDepartures(context.Background(), "ST", 1, clk.now(), 1)
	if !ok || len(deps) != 1 || deps[0].Train != "1" || name != "Πειραιάς" {
		t.Fatalf("first call: %+v %q %v", deps, name, ok)
	}
	if deps, _, ok := s.mpassDepartures(context.Background(), "ST", 1, clk.now(), 5); !ok || len(deps) != 3 {
		t.Fatalf("a caller asking for more got %+v", deps)
	}
	// 50 s later the cache is still fresh and the 05:01 train has left.
	clk.t = clk.t.Add(50 * time.Second)
	deps, _, ok = s.mpassDepartures(context.Background(), "ST", 1, clk.now(), 5)
	if !ok || len(deps) != 2 || deps[0].Train != "2" {
		t.Fatalf("after the 05:01 departure: %+v", deps)
	}
	if n := hits(); n != 1 {
		t.Fatalf("screen fetched %d times inside the 1 min cache", n)
	}
}

// Concurrent fallbacks download the screen once, not once per request.
func TestMPassOneFetchPerBurst(t *testing.T) {
	srv, hits := countServer(t, mpassManyJSON)
	clk := &testClock{t: time.Date(2026, 10, 28, 5, 0, 30, 0, gtfs.Athens)}
	s := &Suburban{MPassURL: srv.URL, HTTP: srv.Client(), Now: clk.now}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, ok := s.mpassDepartures(context.Background(), "ST", 1, clk.now(), 5); !ok {
				t.Error("no departures")
			}
		}()
	}
	wg.Wait()
	if n := hits(); n != 1 {
		t.Fatalf("screen fetched %d times for one burst", n)
	}
}

func TestClockTimeDST(t *testing.T) {
	day := gtfs.Midnight(time.Date(2026, 3, 29, 1, 0, 0, 0, gtfs.Athens))
	// Athens moves to summer time on 2026-03-29 at 03:00. The wall clock " 4:10" is 01:10Z;
	// midnight plus 4 h 10 min of elapsed time would be an hour later.
	got, ok := clockTime(day, " 4:10")
	if want := time.Date(2026, 3, 29, 4, 10, 0, 0, gtfs.Athens); !ok || !got.Equal(want) {
		t.Fatalf("clockTime = %v, want %v", got, want)
	}
	if _, off := got.Zone(); off != 3*3600 {
		t.Fatalf("offset %d, want summer time", off)
	}
	// Past midnight: 24:30 rolls into the next day, at the same wall clock.
	if late, ok := clockTime(day, "24:30"); !ok || !late.Equal(time.Date(2026, 3, 30, 0, 30, 0, 0, gtfs.Athens)) {
		t.Fatalf("24:30 = %v", late)
	}
}

func TestGetJSONBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat(" ", maxJSONBody))
		io.WriteString(w, "{}")
	}))
	defer srv.Close()
	s := &Suburban{HTTP: srv.Client()}
	var v map[string]any
	if err := s.getJSON(context.Background(), srv.URL, &v); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("oversized document: %v", err)
	}
}

func TestSuburbanTrain(t *testing.T) {
	s, _, f := newTestSuburban(t)
	at := func(hh, mm int) int64 { return time.Date(2026, 10, 28, hh, mm, 0, 0, gtfs.Athens).Unix() }
	got, found, ok := s.Train(context.Background(), "2539", suburbanNow)
	if !ok || !found {
		t.Fatalf("found %v ok %v", found, ok)
	}
	want := &SuburbanRun{Train: "2539", From: "Πειραιάς", To: "Κιάτο", Status: "scheduled", DelayMin: intPtr(2), Stops: []SuburbanStop{
		{ID: "ST-PIR", Name: "Πειραιάς", T: "05:20", At: at(5, 20)},
		{ID: "ST-REN", Name: "Ρέντης", T: "05:26", At: at(5, 26), Platform: strPtr("3"), DelayMin: intPtr(4)},
		{ID: "ST-KIA", Name: "Κιάτο", T: "06:30", At: at(6, 30)},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	for _, no := range []string{"1203", "9999"} { // cancelled, unknown
		if _, found, ok := s.Train(context.Background(), no, suburbanNow); !ok || found {
			t.Errorf("%s: found %v ok %v", no, found, ok)
		}
	}
	if n := f.count("/api/public/schedule-board"); n != 1 {
		t.Errorf("board fetched %d times", n)
	}
	f.setFail(true)
	s2, _, f2 := newTestSuburban(t)
	f2.setFail(true)
	if _, _, ok := s2.Train(context.Background(), "2539", suburbanNow); ok {
		t.Error("ok without a board")
	}
}

func TestSuburbanTrainPicksTheRunOfNow(t *testing.T) {
	b := &scheduleBoard{Schedules: []boardSchedule{
		{TrainNumber: "7", Status: "completed", RouteStops: []boardStop{
			{StationID: "A", ScheduledDeparture: "2026-10-28T03:00:00Z"}, {StationID: "B", ScheduledArrival: "2026-10-28T03:30:00Z"}}},
		{TrainNumber: "7 ", Status: "in_progress", RouteStops: []boardStop{
			{StationID: "B", ScheduledDeparture: "2026-10-28T04:00:00Z"}, {StationID: "A", ScheduledArrival: "2026-10-28T04:30:00Z"}}},
		{TrainNumber: "7", Status: "scheduled", RouteStops: []boardStop{
			{StationID: "A", ScheduledDeparture: "2026-10-28T05:00:00Z"}, {StationID: "B", ScheduledArrival: "2026-10-28T05:30:00Z"}}},
	}}
	pick := func(h, m int) string {
		sc := b.run("7", time.Date(2026, 10, 28, h, m, 0, 0, time.UTC))
		if sc == nil {
			return ""
		}
		return sc.Status
	}
	if got := pick(4, 10); got != "in_progress" {
		t.Errorf("04:10 → %s", got)
	}
	if got := pick(4, 45); got != "scheduled" { // between runs: the next one
		t.Errorf("04:45 → %s", got)
	}
	if got := pick(6, 0); got != "scheduled" { // after the last: the last one
		t.Errorf("06:00 → %s", got)
	}
}
