package segspeed

import (
	"compress/gzip"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
)

var athens = func() *time.Location {
	l, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		panic(err)
	}
	return l
}()

// Monday 2026-10-19 05:00 UTC = 08:00 in Athens (weekday, slot 8).
var mon0800 = time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC)

type fix struct {
	dt    int // seconds after mon0800
	veh   string
	shape string
	s     int // metres along the shape; -1 unknown
}

const feed = "2026-07-08"

// writeHour writes one gzipped history archive named after its UTC hour.
func writeHour(t *testing.T, dir string, hour time.Time, fixes []fix) {
	t.Helper()
	writeArchive(t, filepath.Join(dir, hour.UTC().Format("2006-01-02T15")+".csv.gz"), feed, fixes)
}

func writeArchive(t *testing.T, path, gtfs string, fixes []fix) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	var b strings.Builder
	b.WriteString(history.Header)
	for _, x := range fixes {
		fmt.Fprintf(&b, "%d,L,RC,%s,37980000,23720000,%s,T1,%s,%d,0\n", mon0800.Unix()+int64(x.dt), x.veh, gtfs, x.shape, x.s)
	}
	zw.Write([]byte(b.String()))
	zw.Close()
	f.Close()
}

// Two vehicles at 4 m/s over 0..400 m (100 s per 200 m bin), then v1 stands 60 s at 400 m.
func moving() []fix {
	var out []fix
	for _, v := range []string{"v1", "v2"} {
		for i := 0; i <= 5; i++ {
			out = append(out, fix{i * 20, v, "SH", i * 80})
		}
	}
	return append(out, fix{160, "v1", "SH", 400})
}

func build(t *testing.T, dir string) *Table {
	t.Helper()
	tab, err := Build(dir, athens, mon0800.Add(-24*time.Hour), mon0800.Add(24*time.Hour), feed)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-3 }

func TestSlot(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		want int
	}{
		{mon0800, 8},
		{time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC), 24 + 12},   // Saturday 12:00
		{time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), 2*24 + 12}, // Sunday 12:00
		{time.Date(2026, 10, 18, 21, 30, 0, 0, time.UTC), 0},       // Monday 00:30 local
		{time.Date(2026, 10, 26, 6, 0, 0, 0, time.UTC), 8},         // after the DST change: UTC+2
	} {
		if got := Slot(c.at, athens); got != c.want {
			t.Errorf("Slot(%v) = %d, want %d", c.at, got, c.want)
		}
	}
}

// TestSpeedPerBinAndSlot: distance and time of each fix pair go to the 200 m bins it covers;
// standing time stays in its bin (speed 0, known); another hour falls back to all hours.
func TestSpeedPerBinAndSlot(t *testing.T) {
	dir := t.TempDir()
	writeHour(t, dir, mon0800, moving())
	tab := build(t, dir)
	for _, c := range []struct {
		bin, slot int
		want      float64
	}{{0, 8, 4}, {1, 8, 4}, {2, 8, 0}, {0, 9, 4}, {1, 24 + 8, 4}} {
		v, ok := tab.Speed("SH", c.bin, c.slot)
		if !ok || !near(v, c.want) {
			t.Errorf("Speed(SH, %d, %d) = %v, %v; want %v", c.bin, c.slot, v, ok, c.want)
		}
	}
	if _, ok := tab.Speed("SH", 3, 8); ok {
		t.Error("bin 3 has no data")
	}
	if _, ok := tab.Speed("NONE", 0, 8); ok {
		t.Error("unknown shape")
	}
}

// TestTooLittleTime: a bin needs at least MinSecs of fix time.
func TestTooLittleTime(t *testing.T) {
	dir := t.TempDir()
	writeHour(t, dir, mon0800, []fix{{0, "v1", "SH", 0}, {20, "v1", "SH", 80}, {40, "v1", "SH", 160}})
	if _, ok := build(t, dir).Speed("SH", 0, 8); ok {
		t.Errorf("40 s in bin 0 must not count (MinSecs %v)", MinSecs)
	}
}

// TestRejectedPairs: none of these pairs may add to a bin.
func TestRejectedPairs(t *testing.T) {
	dir := t.TempDir()
	writeHour(t, dir, mon0800, []fix{
		{0, "a", "SH", 0}, {5, "a", "SH", 0}, // under 10 s apart
		{0, "b", "SH", 0}, {400, "b", "SH", 0}, // over 300 s apart
		{0, "c", "SH", 0}, {100, "c", "SH2", 0}, {200, "c", "SH", 0}, // shape changes
		{0, "d", "SH", -1}, {100, "d", "SH", 0}, {200, "d", "SH", -1}, // unknown distance
		{0, "e", "SH", 150}, {100, "e", "SH", 50}, // 100 m backwards
		{0, "f", "SH", 0}, {20, "f", "SH", 1000}, // 50 m/s
		{0, "g", "SH", 0}, {0, "g", "SH", 0}, // the same fix twice
	})
	tab := build(t, dir)
	for _, sh := range []string{"SH", "SH2"} {
		for bin := 0; bin < 6; bin++ {
			if v, ok := tab.Speed(sh, bin, 8); ok {
				t.Errorf("Speed(%s, %d) = %v from rejected pairs", sh, bin, v)
			}
		}
	}
}

// TestSmallForwardMoves: slow progress is speed, not standing (19 m per 10 s = 1.9 m/s).
func TestSmallForwardMoves(t *testing.T) {
	dir := t.TempDir()
	var fx []fix
	for i := 0; i <= 10; i++ {
		fx = append(fx, fix{i * 10, "v", "SH", i * 19})
	}
	writeHour(t, dir, mon0800, fx)
	if v, ok := build(t, dir).Speed("SH", 0, 8); !ok || !near(v, 1.9) {
		t.Errorf("Speed = %v, %v; want 1.9", v, ok)
	}
}

// TestWindow: only gzipped archives inside [from, to) are read; the open hour (plain CSV) is not.
func TestWindow(t *testing.T) {
	dir := t.TempDir()
	writeHour(t, dir, mon0800, moving())
	old := mon0800.Add(-3 * 24 * time.Hour)
	writeHour(t, dir, old, []fix{{-3 * 86400, "o", "SH2", 0}, {-3*86400 + 200, "o", "SH2", 0}})
	os.WriteFile(filepath.Join(dir, mon0800.Add(time.Hour).Format("2006-01-02T15")+".csv"),
		[]byte(history.Header+fmt.Sprintf("%d,L,RC,p,37980000,23720000,2026-07-08,T1,SH3,0,0\n%d,L,RC,p,37980000,23720000,2026-07-08,T1,SH3,0,0\n",
			mon0800.Unix()+3600, mon0800.Unix()+3800)), 0o644)
	tab := build(t, dir)
	if _, ok := tab.Speed("SH", 0, 8); !ok {
		t.Error("archive in the window not read")
	}
	if _, ok := tab.Speed("SH2", 0, 8); ok {
		t.Error("archive before the window read")
	}
	if _, ok := tab.Speed("SH3", 0, 9); ok {
		t.Error("plain CSV read")
	}
}

// TestSteps: m/s per 200 m step from the vehicle, time-weighted over the bins it covers; an
// unknown bin counts at the fallback, a standing bin at MinBinSpeed; a step with no known bin
// is null; steps end at the shape's end; one decimal.
func TestSteps(t *testing.T) {
	dir := t.TempDir()
	writeHour(t, dir, mon0800, moving())
	tab := build(t, dir)
	got := tab.Steps("SH", 100, 1000, 8, 2)
	// [100,300): 4 m/s. [300,500): 100 m at 4 + 100 m at 0.1 = 1025 s. [500,700): 100 m at 0.1
	// + 100 m at 2 (unknown) = 1050 s. [700,900), [900,1000): nothing known.
	want := []string{"4", "0.2", "0.2", "null", "null"}
	if len(got) != len(want) {
		t.Fatalf("Steps = %v, want %v", show(got), want)
	}
	for i := range want {
		if show(got)[i] != want[i] {
			t.Fatalf("Steps = %v, want %v", show(got), want)
		}
	}
	if s := tab.Steps("SH", 100, 1000+NSteps*StepM, 8, 2); len(s) != NSteps {
		t.Errorf("at most %d steps, got %d", NSteps, len(s))
	}
	if s := tab.Steps("SH", 1200, 3000, 8, 2); s != nil {
		t.Errorf("no known bin ahead: want nil, got %v", show(s))
	}
	if s := tab.Steps("NONE", 0, 3000, 8, 2); s != nil {
		t.Errorf("unknown shape: want nil, got %v", show(s))
	}
	var nilTab *Table
	if s := nilTab.Steps("SH", 0, 1000, 8, 2); s != nil {
		t.Error("nil table: want nil")
	}
}

func show(s []*float64) []string {
	out := make([]string, len(s))
	for i, v := range s {
		if v == nil {
			out[i] = "null"
		} else {
			out[i] = fmt.Sprint(*v)
		}
	}
	return out
}

// TestFeedVersion: rows of another GTFS feed are left out (a shape id may be reused with a new
// geometry); the table says which feed it is for.
func TestFeedVersion(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, filepath.Join(dir, mon0800.Format("2006-01-02T15")+".csv.gz"), "2026-01-01", moving())
	tab := build(t, dir)
	if _, ok := tab.Speed("SH", 0, 8); ok {
		t.Error("rows of another feed used")
	}
	if tab.Feed() != feed {
		t.Errorf("Feed() = %q", tab.Feed())
	}
	var nilTab *Table
	if nilTab.Feed() != "" {
		t.Error("nil table feed")
	}
}

// TestNumberedArchives: after a restart within an hour the writer adds hour.1.csv.gz; both are
// read, the base archive first.
func TestNumberedArchives(t *testing.T) {
	dir := t.TempDir()
	var first, second []fix
	for _, v := range []string{"v1", "v2"} {
		for i := 0; i <= 5; i++ {
			x := fix{i * 20, v, "SH", i * 80}
			if i <= 2 {
				first = append(first, x)
			} else {
				second = append(second, x)
			}
		}
	}
	h := mon0800.Format("2006-01-02T15")
	writeArchive(t, filepath.Join(dir, h+".csv.gz"), feed, first)
	writeArchive(t, filepath.Join(dir, h+".1.csv.gz"), feed, second)
	tab := build(t, dir)
	for bin := 0; bin < 2; bin++ {
		if v, ok := tab.Speed("SH", bin, 8); !ok || !near(v, 4) {
			t.Errorf("bin %d: %v, %v; want 4", bin, v, ok)
		}
	}
}

// TestMalformedInput: a bad row is skipped (the rest of the archive counts), a non-finite or
// absurd distance never allocates bins, and an unreadable archive is skipped and counted.
func TestMalformedInput(t *testing.T) {
	dir := t.TempDir()
	h := mon0800.Format("2006-01-02T15")
	var b strings.Builder
	b.WriteString(history.Header)
	row := func(dt int, veh, sm string) {
		fmt.Fprintf(&b, "%d,L,RC,%s,37980000,23720000,%s,T1,SH,%s,0\n", mon0800.Unix()+int64(dt), veh, feed, sm)
	}
	for _, x := range moving()[:6] {
		row(x.dt, x.veh, fmt.Sprint(x.s))
	}
	b.WriteString("1791999999,L,RC,q,3798\"0000,23720000,2026-07-08,T1,SH,0,0\n") // bare quote
	for _, x := range moving()[6:] {
		row(x.dt, x.veh, fmt.Sprint(x.s))
	}
	row(0, "n", "NaN")
	row(20, "n", "Inf")
	row(40, "n", "1e12")
	row(0, "far", "4000000")
	row(20, "far", "4000080")
	f, _ := os.Create(filepath.Join(dir, h+".csv.gz"))
	zw := gzip.NewWriter(f)
	zw.Write([]byte(b.String()))
	zw.Close()
	f.Close()
	os.WriteFile(filepath.Join(dir, mon0800.Add(time.Hour).Format("2006-01-02T15")+".csv.gz"), []byte("not gzip"), 0o644)
	tab := build(t, dir)
	if v, ok := tab.Speed("SH", 1, 8); !ok || !near(v, 4) {
		t.Errorf("rows around a bad one: %v, %v; want 4", v, ok)
	}
	if n := len(tab.shapes["SH"]); n > 100*slots {
		t.Errorf("absurd distances allocated %d cells", n)
	}
	if tab.BadFiles() != 1 {
		t.Errorf("BadFiles() = %d, want 1", tab.BadFiles())
	}
}
