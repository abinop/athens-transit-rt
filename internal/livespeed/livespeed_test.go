package livespeed

import (
	"math"
	"testing"

	"github.com/angelospk/athens-transit-rt/internal/geo"
)

// A straight 2 km road eastwards.
var east = geo.NewPolyline([][2]float64{{0, 0}, {2000, 0}})

// pass drives vehicle id over [s0, s1] of line at speed v (m/s), ending at time end.
func pass(g *Grid, id string, line *geo.Polyline, s0, s1, v float64, end int64) {
	g.Pass(id, line, s0, s1, end-int64((s1-s0)/v), end)
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.05 {
		t.Fatalf("got %.2f want %.2f", got, want)
	}
}

func TestTwoBusesGiveTheRoadSpeed(t *testing.T) {
	g := New()
	pass(g, "a", east, 0, 1000, 5, 1000)
	if _, ok := g.Speed([2]float64{500, 0}, 0, 1000); ok {
		t.Fatal("one bus is not traffic yet")
	}
	pass(g, "b", east, 0, 1000, 10, 1100)
	v, ok := g.Speed([2]float64{500, 0}, 0, 1100)
	if !ok {
		t.Fatal("no speed from two buses")
	}
	near(t, v, 2*100/(100.0/5+100.0/10)) // time-weighted over the cell: 6.67 m/s
	if _, ok := g.Speed([2]float64{500, 0}, math.Pi, 1100); ok {
		t.Fatal("the other side of the road has its own speed")
	}
	if _, ok := g.Speed([2]float64{1500, 0}, 0, 1100); ok {
		t.Fatal("a cell nobody crossed")
	}
}

func TestOtherLinesCountOnASharedRoad(t *testing.T) {
	g := New()
	other := geo.NewPolyline([][2]float64{{-3000, 0}, {0, 0}, {2000, 0}}) // another line joins the road
	pass(g, "line1", east, 0, 1000, 8, 1000)
	pass(g, "line2", other, 3000, 4000, 8, 1000)
	v, ok := g.Speed([2]float64{700, 0}, 0, 1000)
	if !ok {
		t.Fatal("two lines on one road are two buses")
	}
	near(t, v, 8)
}

func TestOldPassesAreForgotten(t *testing.T) {
	g := New()
	pass(g, "a", east, 0, 1000, 5, 1000)
	pass(g, "b", east, 0, 1000, 5, 1000)
	if _, ok := g.Speed([2]float64{500, 0}, 0, 1000+int64(Window.Seconds())+1); ok {
		t.Fatal("traffic from more than the window ago")
	}
	g.Prune(1000 + int64(Window.Seconds()) + 1)
	if n := g.Cells(); n != 0 {
		t.Fatalf("%d cells after the prune", n)
	}
}

func TestImplausiblePassesAreIgnored(t *testing.T) {
	g := New()
	for _, p := range []struct {
		s0, s1 float64
		t0, t1 int64
	}{
		{1000, 0, 0, 100},   // backwards
		{0, 1000, 0, 10},    // 100 m/s: a GPS jump
		{0, 1000, 0, 1000},  // a gap of 1000 s: a layover or a detour
		{0, 1000, 100, 100}, // no time
	} {
		g.Pass("a", east, p.s0, p.s1, p.t0, p.t1)
		g.Pass("b", east, p.s0, p.s1, p.t0, p.t1)
	}
	if g.Cells() != 0 {
		t.Fatalf("%d cells from implausible passes", g.Cells())
	}
}

func TestBlendIntoTheStepsAhead(t *testing.T) {
	g := New()
	pass(g, "a", east, 0, 400, 10, 1000)
	pass(g, "b", east, 0, 400, 10, 1000)
	five := 5.0
	typical := []*float64{&five, nil, &five}
	got := g.Blend(typical, east, 0, 1000)
	near(t, *got[0], 0.7*10+0.3*5) // live traffic and the usual speed at this hour
	near(t, *got[1], 10)           // no usual speed: live alone
	near(t, *got[2], 5)            // nobody crossed: the usual speed
	if typical[0] != &five || *typical[0] != 5 {
		t.Fatal("the typical steps were changed in place")
	}
	if g.Blend(nil, east, 1500, 1000) != nil {
		t.Fatal("no data anywhere ahead: nil, as before")
	}
	if got := g.Blend(nil, east, 0, 1000); len(got) < 2 || got[0] == nil || got[1] == nil {
		t.Fatalf("live traffic without a typical table: %v", got)
	}
}

// Live traffic over half a step: that half is blended, the other half keeps the usual speed
// (Codex). 100 m at 0.7×10 + 0.3×5 = 8.5 m/s and 100 m at 5 m/s: 200 m in 31.8 s.
func TestBlendHalfAStep(t *testing.T) {
	g := New()
	pass(g, "a", east, 0, 100, 10, 1000)
	pass(g, "b", east, 0, 100, 10, 1000)
	five := 5.0
	got := g.Blend([]*float64{&five}, east, 0, 1000)
	near(t, *got[0], math.Round(200/(100/8.5+100/5.0)*10)/10)
}

// Passes reach a cell out of time order (vehicles polled at different times): an expired one
// behind a newer one is pruned too (Codex).
func TestPruneOutOfOrder(t *testing.T) {
	g := New()
	pass(g, "a", east, 0, 100, 10, 2000)
	pass(g, "b", east, 0, 100, 10, 500) // older, arrives later
	g.Prune(500 + int64(Window.Seconds()) + 1)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, ss := range g.cells {
		for _, s := range ss {
			if s.t == 500 {
				t.Fatal("an expired pass kept behind a newer one")
			}
		}
	}
}

func TestNilGridKnowsNoTraffic(t *testing.T) {
	var g *Grid
	five := 5.0
	if got := g.Blend([]*float64{&five}, east, 0, 0); len(got) != 1 || got[0] != &five {
		t.Fatalf("%v", got)
	}
	g.Prune(0)
	if g.Cells() != 0 {
		t.Fatal("cells in a nil grid")
	}
}
