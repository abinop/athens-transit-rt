// Package livespeed keeps how fast buses of every line crossed each piece of road in the last
// minutes, so the speeds ahead of a bus (contract rev 6 `path_speeds`) follow today's traffic
// and not only the usual speed at this hour: a bus of another line that just crossed the same
// road counts. A road piece is a CellM square and a heading sector; a cell has a speed once at
// least MinVehicles buses crossed it within Window.
package livespeed

import (
	"math"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
)

const (
	CellM       = 100.0            // cell side, metres
	Window      = 20 * time.Minute // passes older than this are not today's traffic any more
	MinVehicles = 2                // distinct buses a cell needs before its speed counts
	LiveWeight  = 0.7              // share of the live speed when the usual one is known too
	sectors     = 8                // heading sectors (45°): the two sides of a road are apart
	pieceM      = 25.0             // a pass is cut into pieces this long, each to its cell
	minGapS     = 10               // fixes closer in time are GPS noise
	maxGapS     = 300              // fixes further apart may hide a layover or a detour
	maxSpeed    = 20.0             // m/s; faster is a GPS jump
	backM       = 30.0             // GPS noise backwards along the line: standing
)

type key struct {
	x, y int32
	dir  uint8
}

type sample struct {
	t       int64 // unix seconds, end of the pass
	vehicle string
	m, secs float64
}

// Grid is safe for concurrent use.
type Grid struct {
	mu    sync.Mutex
	cells map[key][]sample // oldest first
}

func New() *Grid { return &Grid{cells: map[key][]sample{}} }

func sector(dx, dy float64) uint8 {
	a := math.Atan2(dy, dx) // -π..π
	return uint8(int(math.Floor((a+math.Pi)/(2*math.Pi)*sectors+0.5)) % sectors)
}

func cellOf(p [2]float64, dir uint8) key {
	return key{int32(math.Floor(p[0] / CellM)), int32(math.Floor(p[1] / CellM)), dir}
}

// heading is the direction of line around along.
func heading(line *geo.Polyline, along float64) uint8 {
	a, b := line.At(along-5), line.At(along+5)
	return sector(b[0]-a[0], b[1]-a[1])
}

// Pass records that a vehicle went from s0 (time t0) to s1 (time t1) along line, at an even
// speed in between. A standing vehicle counts too: a jam is traffic. Implausible passes (a GPS
// jump, a long gap, backwards) are ignored.
func (g *Grid) Pass(vehicle string, line *geo.Polyline, s0, s1 float64, t0, t1 int64) {
	dt := float64(t1 - t0)
	ds := s1 - s0
	if dt < minGapS || dt > maxGapS || ds < -backM || ds > maxSpeed*dt || line == nil || len(line.XY) < 2 {
		return
	}
	ds = math.Max(ds, 0)
	add := map[key][2]float64{}
	if ds < pieceM {
		mid := (s0 + s0 + ds) / 2
		k := cellOf(line.At(mid), heading(line, mid))
		add[k] = [2]float64{ds, dt}
	} else {
		n := math.Ceil(ds / pieceM)
		step := ds / n
		for i := 0.0; i < n; i++ {
			a := s0 + i*step
			mid := a + step/2
			k := cellOf(line.At(mid), heading(line, mid))
			v := add[k]
			add[k] = [2]float64{v[0] + step, v[1] + dt*step/ds}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	old := t1 - int64(Window.Seconds())
	for k, v := range add {
		ss := g.cells[k]
		i := 0
		for i < len(ss) && ss[i].t < old {
			i++
		}
		g.cells[k] = append(ss[i:], sample{t1, vehicle, v[0], v[1]})
	}
}

// Speed returns the speed (metres over seconds of every pass) of the road at p in the heading
// sector of h (radians, 0 = east), over the passes within Window of now, when at least
// MinVehicles distinct vehicles made them.
func (g *Grid) Speed(p [2]float64, h float64, now int64) (float64, bool) {
	return g.speedAt(cellOf(p, sector(math.Cos(h), math.Sin(h))), now)
}

func (g *Grid) speedAt(k key, now int64) (float64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	old := now - int64(Window.Seconds())
	m, secs := 0.0, 0.0
	var seen []string
	for _, s := range g.cells[k] {
		if s.t < old || s.t > now {
			continue
		}
		m += s.m
		secs += s.secs
		if !contains(seen, s.vehicle) {
			seen = append(seen, s.vehicle)
		}
	}
	if len(seen) < MinVehicles || secs <= 0 {
		return 0, false
	}
	return m / secs, true
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Prune drops passes older than Window and the cells left empty.
func (g *Grid) Prune(now int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	old := now - int64(Window.Seconds())
	for k, ss := range g.cells {
		i := 0
		for i < len(ss) && ss[i].t < old {
			i++
		}
		if i == len(ss) {
			delete(g.cells, k)
		} else if i > 0 {
			g.cells[k] = append([]sample(nil), ss[i:]...)
		}
	}
}

// Cells is the number of cells with passes.
func (g *Grid) Cells() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.cells)
}

// Blend returns the steps ahead (segspeed.Steps: m/s per StepM from s along line, nil unknown)
// with today's traffic: a step whose road has a live speed gets LiveWeight of it and the rest
// of the usual speed, or the live speed alone when there is no usual one. typical is not
// changed. nil when no step has a speed, as Steps.
func (g *Grid) Blend(typical []*float64, line *geo.Polyline, s float64, now int64) []*float64 {
	if line == nil || len(line.XY) < 2 {
		return typical
	}
	length := line.Length()
	var out []*float64
	seen := false
	for k := 0; k < segspeed.NSteps; k++ {
		start := s + float64(k)*segspeed.StepM
		if start >= length {
			break
		}
		var v *float64
		if k < len(typical) {
			v = typical[k]
		}
		if live, ok := g.stepSpeed(line, start, math.Min(start+segspeed.StepM, length), now); ok {
			b := live
			if v != nil {
				b = LiveWeight*live + (1-LiveWeight)**v
			}
			b = math.Max(segspeed.MinBinSpeed, math.Round(b*10)/10)
			v = &b
		}
		seen = seen || v != nil
		out = append(out, v)
	}
	if !seen {
		return nil
	}
	return out
}

// stepSpeed is the live speed over [from, to] of line, time-weighted over the cells it crosses
// that have one.
func (g *Grid) stepSpeed(line *geo.Polyline, from, to float64, now int64) (float64, bool) {
	m, secs := 0.0, 0.0
	for a := from; a < to; a += pieceM {
		b := math.Min(a+pieceM, to)
		mid := (a + b) / 2
		if v, ok := g.speedAt(cellOf(line.At(mid), heading(line, mid)), now); ok {
			m += b - a
			secs += (b - a) / math.Max(v, segspeed.MinBinSpeed)
		}
	}
	if secs == 0 {
		return 0, false
	}
	return m / secs, true
}
