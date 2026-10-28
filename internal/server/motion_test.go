package server

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/livespeed"
)

var t0 = time.Unix(1_790_000_000, 0)

func straight() *geo.Polyline { return geo.NewPolyline([][2]float64{{0, 0}, {5000, 0}}) }

// feedFixes observes (s, seconds after t0) pairs and returns the last speed.
func feedFixes(m *motion, line *geo.Polyline, fixes ...[2]float64) *float64 {
	var sp *float64
	for _, f := range fixes {
		sp = m.observe("v", line, f[0], t0.Add(time.Duration(f[1])*time.Second))
	}
	return sp
}

func speedIs(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 0.05 {
		t.Fatalf("speed %v, want %.1f", deref(got), want)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestSpeedSteady(t *testing.T) {
	m := newMotion(nil)
	l := straight()
	if sp := feedFixes(m, l, [2]float64{100, 0}); sp != nil {
		t.Fatalf("one fix gave speed %v", *sp)
	}
	speedIs(t, feedFixes(m, l, [2]float64{400, 30}), 10)
	speedIs(t, feedFixes(m, l, [2]float64{700, 60}, [2]float64{1000, 90}), 10)
}

func TestSpeedIgnoresOneOutlier(t *testing.T) {
	m := newMotion(nil)
	// 8 m/s with one GPS fix 150 m ahead in the middle and one at the end.
	speedIs(t, feedFixes(m, straight(), [2]float64{0, 0}, [2]float64{240, 30}, [2]float64{630, 60},
		[2]float64{720, 90}, [2]float64{960, 120}), 8)
	m = newMotion(nil)
	speedIs(t, feedFixes(m, straight(), [2]float64{0, 0}, [2]float64{240, 30}, [2]float64{480, 60},
		[2]float64{720, 90}, [2]float64{1100, 120}), 8)
}

func TestSpeedStandingAndClamp(t *testing.T) {
	m := newMotion(nil)
	speedIs(t, feedFixes(m, straight(), [2]float64{500, 0}, [2]float64{512, 30}, [2]float64{495, 60}), 0)
	m = newMotion(nil)
	// 23 m/s is plausible enough to keep (< 20 m/s * dt + 100 m) but is clamped.
	speedIs(t, feedFixes(m, straight(), [2]float64{0, 0}, [2]float64{690, 30}), 20)
}

func TestSpeedNeedsTimeSpan(t *testing.T) {
	m := newMotion(nil)
	if sp := feedFixes(m, straight(), [2]float64{0, 0}, [2]float64{100, 5}); sp != nil {
		t.Fatalf("5 s span gave speed %v", *sp)
	}
	// Fixes older than the window do not count.
	m = newMotion(nil)
	if sp := feedFixes(m, straight(), [2]float64{0, 0}, [2]float64{3000, 400}); sp != nil {
		t.Fatalf("400 s gap gave speed %v", *sp)
	}
}

func TestResetOnNewLineAndJumps(t *testing.T) {
	m := newMotion(nil)
	l := straight()
	feedFixes(m, l, [2]float64{1000, 0}, [2]float64{1300, 30})
	if sp := feedFixes(m, straight(), [2]float64{1600, 60}); sp != nil {
		t.Fatal("new line kept history")
	}
	m = newMotion(nil)
	feedFixes(m, l, [2]float64{1000, 0}, [2]float64{1300, 30})
	if sp := feedFixes(m, l, [2]float64{200, 60}); sp != nil {
		t.Fatal("backward jump (new run) kept history")
	}
	speedIs(t, feedFixes(m, l, [2]float64{500, 90}), 10) // recovers from the new start
	m = newMotion(nil)
	feedFixes(m, l, [2]float64{1000, 0}, [2]float64{1300, 30})
	if sp := feedFixes(m, l, [2]float64{3000, 60}); sp != nil { // 1700 m in 30 s: another pass
		t.Fatal("forward jump kept history")
	}
}

func TestStaleFixIgnored(t *testing.T) {
	m := newMotion(nil)
	l := straight()
	feedFixes(m, l, [2]float64{1000, 0}, [2]float64{1300, 30})
	// An older fix (e.g. from another line's slower poll) neither resets nor gets a speed.
	if sp := m.observe("v", straight(), 50, t0.Add(10*time.Second)); sp != nil {
		t.Fatal("stale fix got a speed")
	}
	// The same fix polled again keeps its speed.
	speedIs(t, m.observe("v", l, 1300, t0.Add(30*time.Second)), 10)
	speedIs(t, feedFixes(m, l, [2]float64{1600, 60}), 10)
}

func TestForgetBoundsMemory(t *testing.T) {
	m := newMotion(nil)
	m.observe("a", straight(), 0, t0)
	m.observe("b", straight(), 0, t0.Add(9*time.Minute))
	m.forget(t0.Add(11 * time.Minute))
	if _, ok := m.tracks["a"]; ok {
		t.Fatal("a not forgotten")
	}
	if _, ok := m.tracks["b"]; !ok {
		t.Fatal("b forgotten too early")
	}
	for i := 0; i < 20; i++ {
		m.observe("b", straight(), float64(i*100), t0.Add(time.Duration(600+i*10)*time.Second))
	}
	if n := len(m.tracks["b"].fixes); n > motionFixes {
		t.Fatalf("%d fixes kept", n)
	}
}

func TestPickPass(t *testing.T) {
	// Out and back on one street: every point has two passes, 2000 m apart.
	l := geo.NewPolyline([][2]float64{{0, 0}, {1000, 0}, {1000, 1}, {0, 1}})
	m := newMotion(nil)
	cands := l.Candidates(300, 0, 150, 0, false)
	if len(cands) != 2 {
		t.Fatalf("cands %v", cands)
	}
	if _, ok := m.pick("v", l, cands, t0); ok {
		t.Fatal("no history: ambiguous pass picked")
	}
	// Heading tells the passes apart.
	east := l.Candidates(300, 0, 150, 90, true)
	if s, ok := m.pick("v", l, east, t0); !ok || math.Abs(s-300) > 1 {
		t.Fatalf("eastbound: %v %v", s, ok)
	}
	// History tells them apart: 200 m along 30 s ago.
	m.observe("v", l, 200, t0)
	if s, ok := m.pick("v", l, cands, t0.Add(30*time.Second)); !ok || math.Abs(s-300) > 1 {
		t.Fatalf("after 200 m: %v %v", s, ok)
	}
	// The same fix polled again keeps its pass.
	m.observe("v", l, 300, t0.Add(30*time.Second))
	if s, ok := m.pick("v", l, cands, t0.Add(30*time.Second)); !ok || math.Abs(s-300) > 1 {
		t.Fatalf("repeated fix: %v %v", s, ok)
	}
	// Both passes plausible (long gap): ambiguous.
	if _, ok := m.pick("v", l, cands, t0.Add(10*time.Minute)); ok {
		t.Fatal("two plausible passes: picked one")
	}
}

func TestPathAhead(t *testing.T) {
	l := straight()
	// path: to the next stop (rev 3), simplified to 2 points.
	p, b, at := pathAhead(l, 1000, nil, []float64{1600})
	if len(p) != 2 || !nearLL(p[0], l.XY[0], 1000) || !nearLL(p[1], l.XY[0], 1600) || b != nil || !slices.Equal(at, []int{600}) {
		t.Fatalf("to next stop: %v %v %v", p, b, at)
	}
	// path_beyond: on to the third stop ahead; path_stops along both.
	p, b, at = pathAhead(l, 1000, ptr(5.0), []float64{1300, 1700, 2100, 2400})
	if !nearLL(p[len(p)-1], l.XY[0], 1300) || !nearLL(b[0], l.XY[0], 1300) || !nearLL(b[len(b)-1], l.XY[0], 2100) ||
		!slices.Equal(at, []int{300, 700, 1100}) {
		t.Fatalf("three stops: %v %v %v", p, b, at)
	}
	// At most 1500 m: stops beyond are left out.
	p, b, at = pathAhead(l, 1000, nil, []float64{1200, 2800, 3000})
	if !nearLL(p[1], l.XY[0], 1200) || !nearLL(b[len(b)-1], l.XY[0], 2500) || !slices.Equal(at, []int{200}) {
		t.Fatalf("far stops: %v %v %v", p, b, at)
	}
	if p, _, _ := pathAhead(l, 1000, nil, []float64{4000}); !nearLL(p[1], l.XY[0], 2500) {
		t.Fatalf("far next stop: %v", p)
	}
	// At the next stop (rev 3: no path): the continuation starts at the vehicle.
	p, b, at = pathAhead(l, 1000, nil, []float64{1000.5, 1400, 1900})
	if p != nil || !nearLL(b[0], l.XY[0], 1000) || !nearLL(b[len(b)-1], l.XY[0], 1900) || !slices.Equal(at, []int{400, 900}) {
		t.Fatalf("at a stop: %v %v %v", p, b, at)
	}
	// Without stops: max(speed * 150 s, 300 m), at most 1500 m; no continuation.
	if p, b, at := pathAhead(l, 1000, nil, nil); !nearLL(p[1], l.XY[0], 1300) || b != nil || at != nil {
		t.Fatalf("no speed: %v %v %v", p, b, at)
	}
	if p, _, _ := pathAhead(l, 1000, ptr(4.0), nil); !nearLL(p[1], l.XY[0], 1600) {
		t.Fatalf("4 m/s: %v", p)
	}
	if p, _, _ := pathAhead(l, 1000, ptr(20.0), nil); !nearLL(p[1], l.XY[0], 2500) {
		t.Fatalf("20 m/s: %v", p)
	}
	// Shape end, at the last stop, past it: nothing to move along.
	if p, _, _ := pathAhead(l, 4900, nil, nil); !nearLL(p[1], l.XY[0], 5000) {
		t.Fatalf("shape end: %v", p)
	}
	if p, _, _ := pathAhead(l, 5000, nil, nil); p != nil {
		t.Fatalf("at the end: %v", p)
	}
	for _, next := range [][]float64{{1000}, {900}, {1000.5}, {}} {
		if p, b, at := pathAhead(l, 1000, nil, next); p != nil || b != nil || at != nil {
			t.Fatalf("next %v: %v %v %v", next, p, b, at)
		}
	}
	// A corner keeps its vertex.
	corner := geo.NewPolyline([][2]float64{{0, 0}, {100, 0}, {100, 1000}})
	if p, _, _ := pathAhead(corner, 50, nil, nil); len(p) != 3 {
		t.Fatalf("corner: %v", p)
	}
}

// TestPathStopsOnSimplifiedPath: a shape that zigzags 4 m (dropped by the 5 m simplification) is
// longer than the path sent; stop offsets are metres along the path sent.
func TestPathStopsOnSimplifiedPath(t *testing.T) {
	var xy [][2]float64
	for i := 0; i <= 200; i++ {
		xy = append(xy, [2]float64{float64(i) * 10, float64(i%2) * 4})
	}
	zz := geo.NewPolyline(xy)
	stops := []float64{zz.Cum[40], zz.Cum[80], zz.Cum[120]} // 1,292 m of shape: within 1.5 km
	p, b, at := pathAhead(zz, 0, nil, stops)
	if len(p) != 2 || len(b) != 2 || len(at) != 3 {
		t.Fatalf("zigzag: %v %v %v", p, b, at)
	}
	for k, want := range []float64{400, 800, 1200} {
		if math.Abs(float64(at[k])-want) > 2 {
			t.Fatalf("stop %d at %d m, want ~%v (shape along %v)", k, at[k], want, stops[k])
		}
	}
	if math.Abs(geo.Dist(geo.XY(b[1][0], b[1][1]), geo.XY(b[0][0], b[0][1]))-800) > 2 {
		t.Fatalf("beyond length: %v", b)
	}
}

// nearLL: the [lat, lon] point is (within 1 m) `along` metres east of origin on a west-east line.
func nearLL(p [2]float64, origin [2]float64, along float64) bool {
	return geo.Dist(geo.XY(p[0], p[1]), [2]float64{origin[0] + along, origin[1]}) < 1
}

// TestSimplifiedOffsets: offsets follow the points sent, also where the shape passes the same
// point twice and where rounding merges points.
func TestSimplifiedOffsets(t *testing.T) {
	loop := geo.NewPolyline([][2]float64{{0, 0}, {100, 0}, {100, 100}, {0, 100}, {0, 0}, {-100, 0}})
	pts, off := simplified(loop, 0, 500)
	if len(pts) != 6 {
		t.Fatalf("loop points %v", pts)
	}
	for _, x := range []float64{50, 250, 400, 450} {
		if math.Abs(off(x)-x) > 1 {
			t.Fatalf("loop: %v m along gives %v", x, off(x))
		}
	}
	// 0.4 m steps with 0.3 m jitter: rounding to 5 decimals (~1 m) merges points.
	var xy [][2]float64
	for i := 0; i <= 2000; i++ {
		xy = append(xy, [2]float64{float64(i) * 0.4, float64(i%2) * 0.3})
	}
	fine := geo.NewPolyline(xy)
	pts, off = simplified(fine, 0, fine.Length())
	sent := 0.0
	for i := 1; i < len(pts); i++ {
		sent += geo.Dist(geo.XY(pts[i-1][0], pts[i-1][1]), geo.XY(pts[i][0], pts[i][1]))
	}
	if math.Abs(off(fine.Length())-sent) > 0.5 {
		t.Fatalf("end offset %v, points sent %v m", off(fine.Length()), sent)
	}
}

// A bus waiting at its terminus is not traffic: minutes standing there would make the road look
// jammed to every line that passes (R3).
func TestWaitingIsNotTraffic(t *testing.T) {
	roads := livespeed.New()
	m := newMotion(roads)
	l := straight()
	for _, id := range []string{"a", "b"} {
		m.observeAs(id, l, 100, t0, false)
		m.observeAs(id, l, 100, t0.Add(60*time.Second), false)
	}
	if roads.Cells() != 0 {
		t.Fatalf("waiting buses made %d road cells", roads.Cells())
	}
	m.observeAs("a", l, 400, t0.Add(90*time.Second), true)
	if roads.Cells() == 0 {
		t.Fatal("a bus on its way is traffic")
	}
}
