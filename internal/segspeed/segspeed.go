// Package segspeed gives the typical speed per shape, 200 m bin and time slot (day type x
// local hour), built from the fix history (internal/history). It follows the
// TheTransitClock/Swiftly-style historical average; the shape was chosen in the motion lab
// of athens-transit-map (lab/motion/program.md, round 2026-10-21).
package segspeed

import (
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Europe/Athens without the system zoneinfo (distroless image)
)

const (
	BinM        = 200.0    // bin length along the shape, metres
	StepM       = 200.0    // step length in Steps, metres
	NSteps      = 8        // most steps Steps returns
	MinSecs     = 60.0     // fix time a bin needs before its speed counts
	MinBinSpeed = 0.1      // m/s; a standing bin counts this fast in Steps
	MaxGap      = 300      // s; pairs further apart may hide a detour or a layover
	MinGap      = 10       // s; pairs closer are GPS noise
	BackM       = 30       // m; small backwards jitter is allowed
	MaxSpeed    = 20.0     // m/s; faster pairs are GPS jumps
	SlackM      = 100.0    // m; slack on the MaxSpeed bound
	MaxShapeM   = 500000.0 // m; longer than any bus shape, so bad values stay cheap

	hourLayout = "2006-01-02T15" // history file names, UTC
	allHours   = 72              // slot of the all-hours cell
	slots      = allHours + 1    // cells per bin
)

// Table holds one dense speed grid per shape, indexed bin*slots + slot; -1 is unknown.
// The all-hours fallback lives in slot 72 of each bin.
type Table struct {
	shapes   map[string][]float32
	feed     string
	badFiles int
}

// Slot returns day type (0 weekday, 1 Saturday, 2 Sunday) * 24 + local hour of t in loc.
func Slot(t time.Time, loc *time.Location) int {
	lt := t.In(loc)
	day := 0
	switch lt.Weekday() {
	case time.Saturday:
		day = 1
	case time.Sunday:
		day = 2
	}
	return day*24 + lt.Hour()
}

type prevFix struct {
	t     int64
	shape string
	s     float64
}

// shapeSums accumulates (metres, seconds) per cell while Build reads.
type shapeSums struct {
	cells [][2]float32
}

func (s *shapeSums) add(bin, slot int, m, secs float64) {
	i := bin*slots + slot
	if n := i + 1 - len(s.cells); n > 0 {
		s.cells = append(s.cells, make([][2]float32, n)...)
	}
	s.cells[i][0] += float32(m)
	s.cells[i][1] += float32(secs)
}

// pair credits the travel of two consecutive fixes to the bins their path covers.
func pair(s *shapeSums, p, r prevFix, dt float64, slot int) {
	ds := r.s - p.s
	if ds <= 0 {
		bin := int(math.Floor(p.s / BinM))
		s.add(bin, slot, 0, dt)
		s.add(bin, allHours, 0, dt)
		return
	}
	for bin := int(math.Floor(p.s / BinM)); float64(bin)*BinM < r.s; bin++ {
		ov := math.Min(r.s, float64(bin+1)*BinM) - math.Max(p.s, float64(bin)*BinM)
		if ov <= 0 {
			continue
		}
		s.add(bin, slot, ov, dt*ov/ds)
		s.add(bin, allHours, ov, dt*ov/ds)
	}
}

// Build reads the gzipped history archives in dir whose UTC hour start is in [from-1h, to)
// and returns their typical speeds for rows of the gtfs feed. Plain .csv files are still
// being written and are ignored. Damaged archives are skipped and counted, not returned as an
// error (only a directory that cannot be read is).
func Build(dir string, loc *time.Location, from, to time.Time, feed string) (*Table, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type archive struct {
		name string
		h    time.Time
		n    int
	}
	var files []archive
	for _, e := range entries {
		h, n, ok := archiveName(e.Name())
		if !ok || h.Before(from.Add(-time.Hour)) || !h.Before(to) {
			continue
		}
		files = append(files, archive{e.Name(), h, n})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].h.Equal(files[j].h) {
			return files[i].h.Before(files[j].h)
		}
		return files[i].n < files[j].n
	})
	sums := map[string]*shapeSums{}
	prev := map[string]prevFix{}
	bad := 0
	for _, f := range files {
		if err := readFile(filepath.Join(dir, f.name), loc, from, to, feed, prev, sums); err != nil {
			bad++
		}
	}
	tab := &Table{shapes: make(map[string][]float32, len(sums)), feed: feed, badFiles: bad}
	for shape, s := range sums {
		cells := make([]float32, len(s.cells))
		for i, c := range s.cells {
			if c[1] >= MinSecs {
				cells[i] = c[0] / c[1]
			} else {
				cells[i] = -1
			}
		}
		tab.shapes[shape] = cells
	}
	return tab, nil
}

// archiveName splits hour.csv.gz and hour.N.csv.gz (N > 0, after a restart within the hour).
func archiveName(name string) (time.Time, int, bool) {
	rest, ok := strings.CutSuffix(name, ".csv.gz")
	if !ok {
		return time.Time{}, 0, false
	}
	hour, num := rest, ""
	if i := strings.LastIndexByte(rest, '.'); i >= 0 {
		hour, num = rest[:i], rest[i+1:]
	}
	h, err := time.Parse(hourLayout, hour)
	if err != nil {
		return time.Time{}, 0, false
	}
	n := 0
	if num != "" {
		n, err = strconv.Atoi(num)
		if err != nil || n < 1 {
			return time.Time{}, 0, false
		}
	}
	return h, n, true
}

// readFile streams one archive for the gtfs feed, keeping the previous accepted fix per
// vehicle. A bad row is skipped; any other read error drops the rest of the archive.
func readFile(path string, loc *time.Location, from, to time.Time, feed string, prev map[string]prevFix, sums map[string]*shapeSums) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	cr := csv.NewReader(zr)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	head, err := cr.Read()
	if err != nil {
		return err
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	for _, c := range []string{"fix_t", "veh", "gtfs", "shape_id", "s_m"} {
		if _, ok := col[c]; !ok {
			return fmt.Errorf("no %s column", c)
		}
	}
	fromS, toS := from.Unix(), to.Unix()
	for {
		row, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				continue // a bad row does not abort the archive
			}
			return err
		}
		if len(row) != len(head) {
			continue // a row cut by a crash
		}
		if row[col["gtfs"]] != feed {
			continue // another feed may reuse the shape id
		}
		t, err := strconv.ParseInt(row[col["fix_t"]], 10, 64)
		if err != nil || t < fromS || t >= toS {
			continue
		}
		s, err := strconv.ParseFloat(row[col["s_m"]], 64)
		if err != nil || s < 0 || s > MaxShapeM || math.IsNaN(s) || math.IsInf(s, 0) {
			continue
		}
		shape := strings.Clone(row[col["shape_id"]])
		if shape == "" {
			continue
		}
		veh := strings.Clone(row[col["veh"]])
		r := prevFix{t: t, shape: shape, s: s}
		p, ok := prev[veh]
		if !ok {
			prev[veh] = r
			continue
		}
		if t <= p.t {
			continue // the same fix twice, or an older row
		}
		prev[veh] = r
		dt := float64(t - p.t)
		ds := s - p.s
		if p.shape != r.shape || dt < MinGap || dt > MaxGap || ds < -BackM || ds > MaxSpeed*dt+SlackM {
			continue
		}
		ss := sums[shape]
		if ss == nil {
			ss = &shapeSums{}
			sums[shape] = ss
		}
		pair(ss, p, r, dt, Slot(time.Unix(p.t, 0), loc))
	}
}

// Speed returns the typical speed of one cell, or of its all-hours cell when the slot has
// too little data. ok is false for a nil table, an unknown shape, a bad bin or no data.
func (t *Table) Speed(shape string, bin, slot int) (float64, bool) {
	if t == nil || slot < 0 || slot >= slots {
		return 0, false
	}
	cells, ok := t.shapes[shape]
	if !ok || bin < 0 || bin*slots+slot >= len(cells) {
		return 0, false
	}
	if v := cells[bin*slots+slot]; v >= 0 {
		return float64(v), true
	}
	if slot != allHours {
		if v := cells[bin*slots+allHours]; v >= 0 {
			return float64(v), true
		}
	}
	return 0, false
}

// Steps returns the speed of up to NSteps steps of StepM metres from s, each time-weighted
// over the bins it covers. known bins count at their speed, unknown ones at fallback; a step
// with no known bin is nil. The result is nil when every step is nil.
func (t *Table) Steps(shape string, s, length float64, slot int, fallback float64) []*float64 {
	if t == nil || length <= s {
		return nil
	}
	fb := math.Max(fallback, MinBinSpeed)
	out := make([]*float64, 0, NSteps)
	seen := false
	for k := 0; k < NSteps; k++ {
		start := s + float64(k)*StepM
		if start >= length {
			break
		}
		end := math.Min(start+StepM, length)
		secs, known := 0.0, false
		for bin := int(math.Floor(start / BinM)); float64(bin)*BinM < end; bin++ {
			lo := math.Max(start, float64(bin)*BinM)
			hi := math.Min(end, float64(bin+1)*BinM)
			if hi <= lo {
				continue
			}
			v := fb
			if got, ok := t.Speed(shape, bin, slot); ok {
				known = true
				v = math.Max(got, MinBinSpeed)
			}
			secs += (hi - lo) / v
		}
		if !known {
			out = append(out, nil)
			continue
		}
		seen = true
		v := math.Max(MinBinSpeed, math.Round((end-start)/secs*10)/10)
		out = append(out, &v)
	}
	if !seen {
		return nil
	}
	return out
}

// Feed returns the GTFS feed the table was built for, or "" for a nil table.
func (t *Table) Feed() string {
	if t == nil {
		return ""
	}
	return t.feed
}

// BadFiles returns the number of archives skipped as unreadable.
func (t *Table) BadFiles() int {
	if t == nil {
		return 0
	}
	return t.badFiles
}

// Shapes returns the number of shapes with data.
func (t *Table) Shapes() int {
	if t == nil {
		return 0
	}
	return len(t.shapes)
}
