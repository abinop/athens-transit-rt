package server

import (
	"math"
	"slices"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/livespeed"
)

// Motion: a few recent fixes per vehicle as distance along its line, for the `speed` and `path`
// fields. Owned by App.matchMu.
const (
	motionFixes      = 5                // fixes kept per vehicle
	motionWindowS    = 300              // fixes used for speed (other-tier lines poll every 150 s)
	motionMinSpanS   = 20               // speed needs fixes at least this far apart
	motionMinPairS   = 10               // pairs of fixes closer in time are not used for slopes
	motionNoiseM     = 20               // moved less than this over the window: standing
	motionMaxSpeed   = 20.0             // m/s (72 km/h)
	motionBackM      = 30               // GPS noise backwards along the line
	motionSlackM     = 100              // forward jump beyond maxSpeed*dt that is still plausible
	motionForget     = 10 * time.Minute // tracks of vehicles not seen for this long are dropped
	motionMaxOffM    = 150              // farther than this from the shape: no motion
	pathHorizonS     = 150              // path covers this many seconds at the current speed ...
	pathMinM         = 300              // ... at least this far ...
	pathMaxM         = 1500             // ... and at most this far
	pathStops        = 3                // with known stops: up to this many stops ahead
	pathSimplifyM    = 5
	motionCoordScale = 1e5 // 5 decimals
)

type motionFix struct {
	s float64 // metres along line
	t int64   // GPS time, unix seconds
}

type motionTrack struct {
	line  *geo.Polyline
	fixes []motionFix // oldest first, at most motionFixes
	seen  time.Time   // GPS time of the newest fix
}

type motion struct {
	tracks map[string]*motionTrack
	roads  *livespeed.Grid // every plausible move is today's traffic on its road
}

func newMotion(roads *livespeed.Grid) *motion {
	return &motion{tracks: map[string]*motionTrack{}, roads: roads}
}

// plausible reports whether a vehicle can be at s at time t after fix f on the same line.
func plausible(f motionFix, s float64, t int64) bool {
	dt := float64(t - f.t)
	return s >= f.s-motionBackM && s-f.s <= motionMaxSpeed*dt+motionSlackM
}

// observe records a fix and returns the vehicle's speed (nil when unknown). A fix older than
// the track's newest one changes nothing and gets no speed.
func (m *motion) observe(id string, line *geo.Polyline, s float64, at time.Time) *float64 {
	return m.observeAs(id, line, s, at, true)
}

// observeAs is observe; traffic false keeps the move out of the road speeds (a bus waiting at
// its terminus is not traffic).
func (m *motion) observeAs(id string, line *geo.Polyline, s float64, at time.Time, traffic bool) *float64 {
	t := at.Unix()
	tr := m.tracks[id]
	if tr != nil && len(tr.fixes) > 0 {
		last := tr.fixes[len(tr.fixes)-1]
		switch {
		case t < last.t:
			return nil
		case t == last.t && tr.line == line:
			return tr.speed() // the same fix polled again
		}
	}
	if tr == nil || tr.line != line || !plausible(tr.fixes[len(tr.fixes)-1], s, t) {
		// New shape, new run or another pass of a loop: start over.
		tr = &motionTrack{line: line, fixes: make([]motionFix, 0, motionFixes)}
		m.tracks[id] = tr
	}
	if n := len(tr.fixes); n > 0 && m.roads != nil && traffic {
		m.roads.Pass(id, line, tr.fixes[n-1].s, s, tr.fixes[n-1].t, t)
	}
	if len(tr.fixes) == motionFixes {
		tr.fixes = append(tr.fixes[:0], tr.fixes[1:]...)
	}
	tr.fixes = append(tr.fixes, motionFix{s, t})
	tr.seen = at
	return tr.speed()
}

// speed is the median slope over pairs of recent fixes (Theil-Sen), so one bad fix among three
// or more does not change it.
func (tr *motionTrack) speed() *float64 {
	newest := tr.fixes[len(tr.fixes)-1]
	first := len(tr.fixes) - 1
	for first > 0 && newest.t-tr.fixes[first-1].t <= motionWindowS {
		first--
	}
	fx := tr.fixes[first:]
	if len(fx) < 2 || newest.t-fx[0].t < motionMinSpanS {
		return nil
	}
	if math.Abs(newest.s-fx[0].s) < motionNoiseM {
		return ptr(0.0)
	}
	var slopes []float64
	for i := range fx {
		for j := i + 1; j < len(fx); j++ {
			if dt := fx[j].t - fx[i].t; dt >= motionMinPairS {
				slopes = append(slopes, (fx[j].s-fx[i].s)/float64(dt))
			}
		}
	}
	if len(slopes) == 0 {
		return nil
	}
	v := max(0, min(motionMaxSpeed, median(slopes)))
	return ptr(math.Round(v*10) / 10)
}

func median(v []float64) float64 {
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// pick chooses which pass of the line a vehicle is on: the only one that fits its heading, or
// the only one reachable from its last fix. ok is false when that stays ambiguous.
func (m *motion) pick(id string, line *geo.Polyline, cands []geo.Cand, at time.Time) (float64, bool) {
	right := make([]geo.Cand, 0, len(cands))
	for _, c := range cands {
		if !c.Wrong {
			right = append(right, c)
		}
	}
	if len(right) > 0 {
		cands = right
	}
	if len(cands) == 1 {
		return cands[0].Along, true
	}
	tr := m.tracks[id]
	if tr == nil || tr.line != line || len(tr.fixes) == 0 {
		return 0, false
	}
	last := tr.fixes[len(tr.fixes)-1]
	if at.Unix() < last.t {
		return 0, false
	}
	found, s := 0, 0.0
	for _, c := range cands {
		if plausible(last, c.Along, at.Unix()) {
			found, s = found+1, c.Along
		}
	}
	return s, found == 1
}

func (m *motion) forget(now time.Time) {
	for id, tr := range m.tracks {
		if now.Sub(tr.seen) > motionForget {
			delete(m.tracks, id)
		}
	}
}

// pathAhead is the line ahead of s as [lat, lon] points (contract rev 3): to the next stop
// (stops[0]) when stops are known, else max(speed * pathHorizonS, pathMinM); at most pathMaxM
// and never past the line's end. nil when there is nothing ahead (at the stop, at the end).
// With stops it also returns (rev 4) the continuation up to the pathStops-th stop ahead (from
// path's end, or from s when path is nil) and the stops on path + beyond, as metres along them.
func pathAhead(line *geo.Polyline, s float64, speed *float64, stops []float64) (path, beyond [][2]float64, at []int) {
	end := s + pathMaxM
	switch {
	case stops != nil:
		if len(stops) == 0 {
			return nil, nil, nil
		}
		end = min(end, stops[0])
	case speed != nil:
		end = s + min(pathMaxM, max(pathMinM, *speed*pathHorizonS))
	default:
		end = s + pathMinM
	}
	end = min(end, line.Length())
	var off func(float64) float64
	if end-s >= 1 {
		path, off = simplified(line, s, end)
	}
	if stops == nil {
		return path, nil, nil
	}
	var ahead []float64
	for _, x := range stops {
		if x > s+1 && (len(ahead) == 0 || x > ahead[len(ahead)-1]) {
			ahead = append(ahead, x)
		}
	}
	if len(ahead) == 0 {
		return path, nil, nil
	}
	from := s
	if path != nil {
		from = end
	}
	last := min(s+pathMaxM, line.Length(), ahead[min(pathStops, len(ahead))-1])
	var offB func(float64) float64
	if last-from >= 1 {
		beyond, offB = simplified(line, from, last)
	}
	base := 0.0
	if path != nil {
		base = off(end)
	}
	for _, x := range ahead {
		var m float64
		switch {
		case path != nil && x <= end:
			m = off(x)
		case beyond != nil && x <= last:
			m = base + offB(x)
		default:
			continue
		}
		if r := int(math.Round(m)); len(at) == 0 || r > at[len(at)-1] {
			at = append(at, r)
		}
	}
	return path, beyond, at
}

// simplified is the line from `from` to `to` as rounded [lat, lon] points (5 m simplification)
// and, for a position along the line in that range, the metres along the points sent (nil, nil
// when fewer than 2 points remain).
func simplified(line *geo.Polyline, from, to float64) ([][2]float64, func(float64) float64) {
	sl := line.Slice(from, to)
	along := make([]float64, len(sl)) // original distance along the line of each slice point
	along[0] = from
	for i := 1; i < len(sl); i++ {
		along[i] = along[i-1] + geo.Dist(sl[i-1], sl[i])
	}
	var out [][2]float64
	var orig, cum []float64 // per point sent: original along, metres along the points sent
	for _, i := range geo.SimplifyIndex(sl, pathSimplifyM) {
		lat, lon := geo.LatLon(sl[i])
		ll := [2]float64{round5(lat), round5(lon)}
		if n := len(out); n > 0 && out[n-1] == ll {
			continue
		}
		c := 0.0
		if n := len(out); n > 0 {
			c = cum[n-1] + geo.Dist(geo.XY(out[n-1][0], out[n-1][1]), geo.XY(ll[0], ll[1]))
		}
		out, orig, cum = append(out, ll), append(orig, along[i]), append(cum, c)
	}
	if len(out) < 2 {
		return nil, nil
	}
	return out, func(x float64) float64 {
		k := 0
		for k+2 < len(orig) && orig[k+1] < x {
			k++
		}
		if orig[k+1] <= orig[k] {
			return cum[k+1]
		}
		f := max(0, min(1, (x-orig[k])/(orig[k+1]-orig[k])))
		return cum[k] + f*(cum[k+1]-cum[k])
	}
}

func round5(v float64) float64 { return math.Round(v*motionCoordScale) / motionCoordScale }
