package stats

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
)

var athens = mustLoc("Europe/Athens")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// fix is one history row; lat/lon in degrees.
type fix struct {
	t        time.Time
	line     string
	veh      string
	lat, lon float64
}

func row(f fix) string {
	return fmt.Sprintf("%d,%s,R,%s,%d,%d,g,,,-1,\n", f.t.Unix(), f.line, f.veh,
		int64(f.lat*1e6+0.5), int64(f.lon*1e6+0.5))
}

// writeHour writes one archive (name without extension, e.g. 2026-10-06T21 or 2026-10-06T21.1).
func writeHour(t *testing.T, dir, name, ext string, fixes ...fix) {
	t.Helper()
	var b strings.Builder
	b.WriteString(history.Header)
	for _, f := range fixes {
		b.WriteString(row(f))
	}
	p := filepath.Join(dir, name+ext)
	if ext == ".csv.gz" {
		out, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(out)
		zw.Write([]byte(b.String()))
		zw.Close()
		out.Close()
		return
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hourName(t time.Time) string { return t.UTC().Format("2006-01-02T15") }

// north moves lat by m metres.
func north(lat, m float64) float64 { return lat + m/111195 }

func local(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, athens)
}

func TestComputeSumsSpeedPerHourAndLine(t *testing.T) {
	dir := t.TempDir()
	t0 := local(2026, 10, 6, 8, 0, 0) // 05:00 UTC
	writeHour(t, dir, hourName(t0), ".csv.gz",
		fix{t0, "040", "1", 37.98, 23.72},
		fix{t0.Add(30 * time.Second), "040", "1", north(37.98, 150), 23.72}, // 150 m / 30 s
		fix{t0.Add(60 * time.Second), "040", "1", north(37.98, 300), 23.72}, // 150 m / 30 s
		fix{t0.Add(10 * time.Second), "X95", "2", 37.90, 23.80},
		fix{t0.Add(70 * time.Second), "X95", "2", north(37.90, 600), 23.80}, // 600 m / 60 s
	)
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := d.Hours[8]
	if h.Fixes != 5 || h.Vehicles != 2 || h.Lines != 2 || h.TimeS != 120 || !near(h.DistM, 900) {
		t.Fatalf("hour 8 = %+v", h)
	}
	l := byLine(d, "040")
	if l.Vehicles != 1 || l.TimeS != 60 || !near(l.DistM, 300) {
		t.Fatalf("040 = %+v", l)
	}
	if d.Fixes != 5 || d.Vehicles != 2 || d.Lines != 2 || len(d.Hours) != 24 {
		t.Fatalf("day = %+v", d)
	}
	if d.FirstFix != t0.Unix() || d.LastFix != t0.Add(70*time.Second).Unix() {
		t.Fatalf("first/last = %d %d", d.FirstFix, d.LastFix)
	}
}

func TestComputeSkipsBadPairs(t *testing.T) {
	dir := t.TempDir()
	t0 := local(2026, 10, 6, 12, 0, 0)
	writeHour(t, dir, hourName(t0), ".csv.gz",
		fix{t0, "1", "a", 37.98, 23.72},
		fix{t0.Add(200 * time.Second), "1", "a", north(37.98, 500), 23.72},  // gap > 120 s
		fix{t0.Add(203 * time.Second), "1", "a", north(37.98, 510), 23.72},  // gap < 5 s
		fix{t0.Add(233 * time.Second), "1", "a", north(37.98, 2000), 23.72}, // 49 m/s: GPS jump
		fix{t0.Add(263 * time.Second), "2", "a", north(37.98, 2100), 23.72}, // line changed
		fix{t0.Add(263 * time.Second), "2", "a", north(37.98, 2100), 23.72}, // duplicate fix
		fix{t0.Add(293 * time.Second), "2", "a", 0, 0},                      // no position
	)
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := d.Hours[12]; h.TimeS != 0 || h.DistM != 0 || h.Fixes != 6 {
		t.Fatalf("hour 12 = %+v", h)
	}
}

// A vehicle on line A, then B, then A again: the A→A pair must not bridge the B fix.
func TestComputePairsOnlyConsecutiveFixes(t *testing.T) {
	dir := t.TempDir()
	t0 := local(2026, 10, 6, 12, 0, 0)
	// Shuffled over two archives of the same hour (a restart inside the hour).
	writeHour(t, dir, hourName(t0)+".1", ".csv.gz",
		fix{t0.Add(60 * time.Second), "A", "v", north(37.98, 200), 23.72},
		fix{t0.Add(30 * time.Second), "B", "v", north(37.98, 100), 23.72})
	writeHour(t, dir, hourName(t0), ".csv.gz", fix{t0, "A", "v", 37.98, 23.72})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := d.Hours[12]; h.TimeS != 0 || h.Fixes != 3 {
		t.Fatalf("hour 12 = %+v", h)
	}
}

// A fix belongs to the local day of its fix time, not of the file it was received in: a
// 23:59:50 fix received after midnight counts for the old day; files a day earlier are not read.
func TestComputeUsesFixTimeAndLocalMidnight(t *testing.T) {
	dir := t.TempDir()
	mid := local(2026, 10, 7, 0, 0, 0) // 21:00 UTC on the 6th
	writeHour(t, dir, hourName(mid), ".csv.gz",
		fix{mid.Add(-10 * time.Second), "1", "a", 37.98, 23.72},
		fix{mid.Add(20 * time.Second), "1", "a", north(37.98, 300), 23.72})
	writeHour(t, dir, hourName(mid.Add(-time.Hour)), ".csv.gz",
		fix{mid.Add(-40 * time.Second), "1", "a", north(37.98, -300), 23.72})
	d6, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := d6.Hours[23]; h.Fixes != 2 || h.TimeS != 30 || !near(h.DistM, 300) {
		t.Fatalf("6th hour 23 = %+v", h)
	}
	d7, err := Compute("2026-10-07", athens, inputs(t, dir, "2026-10-07"), time.Unix(0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The pair crosses midnight: its earlier fix is outside the day, so it is not counted.
	if h := d7.Hours[0]; h.Fixes != 1 || h.TimeS != 0 {
		t.Fatalf("7th hour 0 = %+v", h)
	}
}

// 2026-10-25: clocks go back at 04:00 EEST, so local hour 3 happens twice (25 hours). Both
// merge into hours[3]; a vehicle seen in both counts once.
func TestComputeDSTFallBack(t *testing.T) {
	dir := t.TempDir()
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)  // 03:30 EEST
	second := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC) // 03:30 EET
	writeHour(t, dir, hourName(first), ".csv.gz",
		fix{first, "1", "a", 37.98, 23.72}, fix{first.Add(30 * time.Second), "1", "a", north(37.98, 100), 23.72})
	writeHour(t, dir, hourName(second), ".csv.gz",
		fix{second, "1", "a", 37.98, 23.72}, fix{second.Add(30 * time.Second), "1", "a", north(37.98, 100), 23.72})
	start, end, err := Bounds("2026-10-25", athens)
	if err != nil {
		t.Fatal(err)
	}
	if end.Sub(start) != 25*time.Hour {
		t.Fatalf("day length %v", end.Sub(start))
	}
	d, err := Compute("2026-10-25", athens, inputs(t, dir, "2026-10-25"), time.Unix(0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := d.Hours[3]; h.Fixes != 4 || h.Vehicles != 1 || h.TimeS != 60 {
		t.Fatalf("hour 3 = %+v", h)
	}
	if _, end, _ := Bounds("2026-03-29", athens); end.Sub(time.Date(2026, 3, 28, 22, 0, 0, 0, time.UTC)) != 23*time.Hour {
		t.Fatalf("spring day end %v", end)
	}
}

// 2026-03-29: clocks go forward at 03:00 EET, so local hour 3 does not exist (23 hours).
func TestComputeDSTSpringForward(t *testing.T) {
	dir := t.TempDir()
	before := time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC) // 02:30 EET
	after := time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC)  // 04:30 EEST
	writeHour(t, dir, hourName(before), ".csv.gz", fix{before, "1", "a", 37.98, 23.72})
	writeHour(t, dir, hourName(after), ".csv.gz", fix{after, "1", "a", 37.98, 23.72})
	d, err := Compute("2026-03-29", athens, inputs(t, dir, "2026-03-29"), time.Unix(0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Hours[2].Fixes != 1 || d.Hours[3].Fixes != 0 || d.Hours[4].Fixes != 1 || d.Fixes != 2 {
		t.Fatalf("hours 2-4 = %+v", d.Hours[2:5])
	}
}

// A corrupt archive fails the day without writing it; once the archive is fixed, a later run
// writes it.
func TestRunRetriesAfterAReadError(t *testing.T) {
	hist, out := t.TempDir(), t.TempDir()
	t0 := local(2026, 10, 6, 12, 0, 0)
	bad := filepath.Join(hist, hourName(t0)+".csv.gz")
	os.WriteFile(bad, []byte("not gzip"), 0o644)
	now := local(2026, 10, 7, 5, 0, 0)
	if got, err := Run(hist, out, athens, now, false, nil); err == nil || len(got) != 0 {
		t.Fatalf("corrupt archive: wrote %v, err %v", got, err)
	}
	if exists(filepath.Join(out, "days", "2026-10-06.json")) {
		t.Fatal("day written from a corrupt archive")
	}
	os.Remove(bad)
	writeHour(t, hist, hourName(t0), ".csv.gz", fix{t0, "1", "a", 37.98, 23.72})
	if got, err := Run(hist, out, athens, now, false, nil); err != nil || len(got) != 1 {
		t.Fatalf("retry: wrote %v, err %v", got, err)
	}
}

func TestInputsWaitsForOpenFiles(t *testing.T) {
	dir := t.TempDir()
	t0 := local(2026, 10, 6, 12, 0, 0)
	writeHour(t, dir, hourName(t0), ".csv.gz", fix{t0, "1", "a", 37.98, 23.72})
	writeHour(t, dir, hourName(t0.Add(time.Hour)), ".csv.part")
	if _, ready, err := Inputs(dir, "2026-10-06", athens); err != nil || ready {
		t.Fatalf("ready=%v err=%v with a .part file", ready, err)
	}
	os.Remove(filepath.Join(dir, hourName(t0.Add(time.Hour))+".csv.part"))
	writeHour(t, dir, hourName(t0.Add(time.Hour)), ".csv")
	if _, ready, _ := Inputs(dir, "2026-10-06", athens); ready {
		t.Fatal("ready with a plain .csv")
	}
	// A plain file outside the window (the next day + 1 h) does not block.
	os.Remove(filepath.Join(dir, hourName(t0.Add(time.Hour))+".csv"))
	writeHour(t, dir, hourName(local(2026, 10, 7, 9, 0, 0)), ".csv")
	files, ready, err := Inputs(dir, "2026-10-06", athens)
	if err != nil || !ready || len(files) != 1 {
		t.Fatalf("files=%v ready=%v err=%v", files, ready, err)
	}
}

func TestRunWritesFinishedDaysOnce(t *testing.T) {
	hist, out := t.TempDir(), t.TempDir()
	t0 := local(2026, 10, 6, 12, 0, 0)
	writeHour(t, hist, hourName(t0), ".csv.gz", fix{t0, "1", "a", 37.98, 23.72})
	t1 := local(2026, 10, 7, 12, 0, 0)
	writeHour(t, hist, hourName(t1), ".csv.gz", fix{t1, "1", "a", 37.98, 23.72})
	// A file early on the 6th makes the 5th a candidate, but it has no fix of the 5th.
	t5 := local(2026, 10, 6, 0, 10, 0)
	writeHour(t, hist, hourName(t5), ".csv.gz", fix{t5, "1", "b", 37.98, 23.72})

	// 03:30 on the 7th: the 6th ended less than Settle ago.
	if got, err := Run(hist, out, athens, local(2026, 10, 7, 3, 30, 0), false, nil); err != nil || len(got) != 0 {
		t.Fatalf("early run wrote %v, err %v", got, err)
	}
	now := local(2026, 10, 7, 4, 30, 0)
	got, err := Run(hist, out, athens, now, false, nil)
	if err != nil || strings.Join(got, ",") != "2026-10-06" {
		t.Fatalf("run wrote %v, err %v", got, err)
	}
	var idx Index
	readJSON(t, filepath.Join(out, "index.json"), &idx)
	if strings.Join(idx.Days, ",") != "2026-10-06" || idx.Latest != "2026-10-06" {
		t.Fatalf("index = %+v", idx)
	}
	var d Day
	readJSON(t, filepath.Join(out, "days", "2026-10-06.json"), &d)
	if d.Version != 1 || d.Date != "2026-10-06" || d.TZ != "Europe/Athens" || d.GeneratedAt != now.Unix() {
		t.Fatalf("day = %+v", d)
	}
	// A later run does not rewrite it, unless forced.
	if got, _ := Run(hist, out, athens, now.Add(time.Hour), false, nil); len(got) != 0 {
		t.Fatalf("second run wrote %v", got)
	}
	if got, _ := Run(hist, out, athens, now.Add(time.Hour), true, nil); len(got) != 1 {
		t.Fatalf("forced run wrote %v", got)
	}
}

func TestRunRebuildsIndexFromFiles(t *testing.T) {
	hist, out := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(out, "days"), 0o755)
	for _, d := range []string{"2026-10-04", "2026-10-02"} {
		os.WriteFile(filepath.Join(out, "days", d+".json"), []byte("{}"), 0o644)
	}
	os.WriteFile(filepath.Join(out, "days", "junk.json.tmp"), nil, 0o644)
	if _, err := Run(hist, out, athens, time.Now(), false, nil); err != nil {
		t.Fatal(err)
	}
	var idx Index
	readJSON(t, filepath.Join(out, "index.json"), &idx)
	if strings.Join(idx.Days, ",") != "2026-10-02,2026-10-04" || idx.Latest != "2026-10-04" {
		t.Fatalf("index = %+v", idx)
	}
}

func TestBoundsRejectsBadDates(t *testing.T) {
	for _, s := range []string{"", "2026-10-6", "2026-13-01", "../x", "2026-10-06x"} {
		if _, _, err := Bounds(s, athens); err == nil {
			t.Errorf("Bounds(%q) accepted", s)
		}
	}
}

func inputs(t *testing.T, dir, date string) []string {
	t.Helper()
	files, ready, err := Inputs(dir, date, athens)
	if err != nil || !ready {
		t.Fatalf("Inputs: ready=%v err=%v", ready, err)
	}
	return files
}

func byLine(d *Day, id string) Line {
	for _, l := range d.ByLine {
		if l.Line == id {
			return l
		}
	}
	return Line{}
}

func near(got int64, want float64) bool { return float64(got) > want-2 && float64(got) < want+2 }

func readJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}
