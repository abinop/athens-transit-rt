package rail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

func testSTASY(t *testing.T) *STASY {
	t.Helper()
	f, err := ParseSTASY(filepath.Join("testdata", "stasy"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The fixture runs on Wed 2026-10-28 (service "wd") and Sat 2026-10-31 (service "we").
var (
	stasyNow = time.Date(2026, 10, 28, 10, 0, 0, 0, gtfs.Athens)
	stasyWed = time.Date(2026, 11, 4, 10, 0, 0, 0, gtfs.Athens) // a week after the feed ends
)

func TestFold(t *testing.T) {
	for _, pair := range [][2]string{
		{"ΣΥΝΤΑΓΜΑ", "Σύνταγμα"},
		{"Σύνταγμα", "συνταγμα"},
		{"  ΑΓΙΟΣ   ΝΙΚΟΛΑΟΣ ", "άγιος νικολαος"},
		{"ΠΕΙΡΑΙΑΣ", "πειραιασ"}, // final sigma
	} {
		if a, b := Fold(pair[0]), Fold(pair[1]); a != b {
			t.Errorf("Fold(%q) = %q, Fold(%q) = %q", pair[0], a, pair[1], b)
		}
	}
}

func TestHHMM(t *testing.T) {
	for secs, want := range map[int32]string{0: "00:00", 5*3600 + 30*60: "05:30", 24*3600 + 10*60: "24:10"} {
		if got := HHMM(secs); got != want {
			t.Errorf("HHMM(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestSTASYDepartures(t *testing.T) {
	f := testSTASY(t)
	if n := f.NumTrips(); n != 8 {
		t.Fatalf("trips = %d", n)
	}
	tests := []struct {
		name       string
		stop       string
		n          int
		now        time.Time
		indicative bool
		want       []STASYDeparture
	}{
		{
			name: "next per line and headsign", stop: "ΣΥΝΤΑΓΜΑ", n: 3, now: stasyNow,
			want: []STASYDeparture{
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "10:01", InS: 60},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΑΝΘΟΥΠΟΛΗ", T: "10:02", InS: 120},
				{Line: "M1", RouteColor: "#00A650", Headsign: "ΚΗΦΙΣΙΑ", T: "10:04", InS: 240},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "10:07", InS: 420},
				{Line: "M1", RouteColor: "#00A650", Headsign: "ΚΗΦΙΣΙΑ", T: "10:20", InS: 1200},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "24:30", InS: 24*3600 + 30*60 - 10*3600},
			},
		},
		{
			// The name is folded: no accents, any case. n caps each (line, headsign) group.
			name: "folded name, one per group", stop: "συνταγμα", n: 1, now: stasyNow,
			want: []STASYDeparture{
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "10:01", InS: 60},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΑΝΘΟΥΠΟΛΗ", T: "10:02", InS: 120},
				{Line: "M1", RouteColor: "#00A650", Headsign: "ΚΗΦΙΣΙΑ", T: "10:04", InS: 240},
			},
		},
		{
			// Service "we" (the 10:03 trip) does not run on a Wednesday.
			name: "only the services of the day", stop: "ΣΥΝΤΑΓΜΑ", n: 10, now: stasyNow,
			want: []STASYDeparture{
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "10:01", InS: 60},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΑΝΘΟΥΠΟΛΗ", T: "10:02", InS: 120},
				{Line: "M1", RouteColor: "#00A650", Headsign: "ΚΗΦΙΣΙΑ", T: "10:04", InS: 240},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "10:07", InS: 420},
				// M1-2 has no trip_headsign in the fixture: the last stop of the trip names it.
				{Line: "M1", RouteColor: "#00A650", Headsign: "ΚΗΦΙΣΙΑ", T: "10:20", InS: 1200},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "24:30", InS: 24*3600 + 30*60 - 10*3600},
			},
		},
		{
			// The feed ends on 2026-10-31: the same weekday inside it is planned instead.
			name: "expired feed is indicative", stop: "ΣΥΝΤΑΓΜΑ", n: 1, now: stasyWed, indicative: true,
			want: []STASYDeparture{
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "10:01", InS: 60},
				{Line: "M2", RouteColor: "#e30613", Headsign: "ΑΝΘΟΥΠΟΛΗ", T: "10:02", InS: 120},
				{Line: "M1", RouteColor: "#00A650", Headsign: "ΚΗΦΙΣΙΑ", T: "10:04", InS: 240},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := f.Departures(tc.stop, tc.n, tc.now)
			if !ok {
				t.Fatalf("no stop %q", tc.stop)
			}
			if got.Indicative != tc.indicative {
				t.Errorf("indicative = %v", got.Indicative)
			}
			if got.Stop != "ΣΥΝΤΑΓΜΑ" {
				t.Errorf("stop = %q", got.Stop)
			}
			if !reflect.DeepEqual(got.Departures, tc.want) {
				t.Errorf("departures:\n got %+v\nwant %+v", got.Departures, tc.want)
			}
		})
	}
}

func TestSTASYUnknownStop(t *testing.T) {
	f := testSTASY(t)
	if _, ok := f.Departures("ΔΕΝ ΥΠΑΡΧΕΙ", 6, stasyNow); ok {
		t.Fatal("unknown stop matched")
	}
}

// A departure at 24:30 of yesterday's service day is 00:30 today: the early morning still sees it.
func TestSTASYYesterdayOvernight(t *testing.T) {
	f := testSTASY(t)
	now := time.Date(2026, 10, 29, 0, 10, 0, 0, gtfs.Athens) // Thursday, 20 min after the 24:30 run
	got, ok := f.Departures("ΣΥΝΤΑΓΜΑ", 1, now)
	if !ok {
		t.Fatal("no stop ΣΥΝΤΑΓΜΑ")
	}
	want := []STASYDeparture{
		{Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", T: "24:30", InS: 20 * 60},
		{Line: "M2", RouteColor: "#e30613", Headsign: "ΑΝΘΟΥΠΟΛΗ", T: "10:02", InS: 10*3600 + 2*60 - 10*60},
		{Line: "M1", RouteColor: "#00A650", Headsign: "ΚΗΦΙΣΙΑ", T: "10:04", InS: 10*3600 + 4*60 - 10*60},
	}
	if !reflect.DeepEqual(got.Departures, want) {
		t.Errorf("departures:\n got %+v\nwant %+v", got.Departures, want)
	}
	// The 24:30 has left by 00:40: the previous day no longer contributes, only the next 09:50.
	late := time.Date(2026, 10, 29, 0, 40, 0, 0, gtfs.Athens)
	got, _ = f.Departures("ΣΥΝΤΑΓΜΑ", 10, late)
	if len(got.Departures) == 0 || got.Departures[0].InS < 9*3600 {
		t.Fatalf("departed train still listed: %+v", got.Departures[0])
	}
}

// stasyFixture is the testdata generation as file name -> body, for the fake catalogue.
func stasyFixture(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("testdata", "stasy"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("testdata", "stasy", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// stasyServer is the CKAN catalogue and the file host of the STASY feed, with a switch that
// makes one file fail.
type stasyServer struct {
	mu       sync.Mutex
	modified string
	fail     string
	files    map[string]string
	hits     int
}

func (f *stasyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	modified, fail, files := f.modified, f.fail, f.files
	f.mu.Unlock()
	if r.URL.Path == "/catalog" {
		res := make([]map[string]string, 0, len(files))
		for name := range files {
			res = append(res, map[string]string{"name": name, "url": "http://" + r.Host + "/files/" + name})
		}
		body, _ := json.Marshal(map[string]any{"success": true, "result": map[string]any{
			"metadata_modified": modified, "resources": res}})
		w.Write(body)
		return
	}
	name := path.Base(r.URL.Path)
	if name == fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	body, ok := files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	io.WriteString(w, body)
}

func TestSTASYRefreshKeepsTheOldGeneration(t *testing.T) {
	files := stasyFixture(t)
	fsrv := &stasyServer{modified: "v1", files: files}
	srv := httptest.NewServer(fsrv)
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "stasy")
	s := &STASYStore{CatalogURL: srv.URL + "/catalog", Dir: dir, HTTP: srv.Client(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if f, ok := s.Feed(); !ok || f.NumTrips() != 8 {
		t.Fatalf("feed after the first refresh: %v %v", ok, f)
	}

	// The next generation renames a stop, and its stop_times download fails.
	fsrv.mu.Lock()
	fsrv.modified, fsrv.fail = "v2", "stop_times.txt"
	fsrv.files = map[string]string{}
	for name, body := range files {
		fsrv.files[name] = body
	}
	fsrv.files["stops.txt"] = strings.ReplaceAll(files["stops.txt"], "ΣΥΝΤΑΓΜΑ", "ΑΛΛΟ")
	fsrv.mu.Unlock()
	if err := s.refresh(context.Background()); err == nil {
		t.Fatal("a failed download was not reported")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "stops.txt")); err != nil || !strings.Contains(string(b), "ΣΥΝΤΑΓΜΑ") {
		t.Fatalf("the failed generation touched the live one: %v %q", err, b)
	}

	// The next generation parses badly: the files on disk are still the old ones, all of them.
	fsrv.mu.Lock()
	fsrv.modified, fsrv.fail = "v3", ""
	fsrv.files["stop_times.txt"] = strings.ReplaceAll(files["stop_times.txt"], "10:01:00", "10:01")
	fsrv.mu.Unlock()
	if err := s.refresh(context.Background()); err == nil {
		t.Fatal("a broken generation was not reported")
	}
	f, err := ParseSTASY(dir)
	if err != nil || f.NumTrips() != 8 || f.Stops[0].Name != "ΣΥΝΤΑΓΜΑ" {
		t.Fatalf("a broken generation changed the live directory: %v %v", f, err)
	}
	if w, ok := s.Feed(); !ok || w.NumTrips() != 8 {
		t.Fatalf("the served timetable changed: %v", ok)
	}
	// No generation is left behind next to the live one.
	for _, pat := range []string{"stasy.new-*", "stasy.old"} {
		if m, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), pat)); len(m) != 0 {
			t.Fatalf("%s left behind: %v", pat, m)
		}
	}
}

func TestCopyCapped(t *testing.T) {
	var b strings.Builder
	if _, err := copyCapped(&b, strings.NewReader("0123456789"), 4); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("oversized body: %v", err)
	}
	b.Reset()
	if n, err := copyCapped(&b, strings.NewReader("0123"), 4); err != nil || n != 4 || b.String() != "0123" {
		t.Fatalf("copyCapped = %d %v %q", n, err, b.String())
	}
}
