package livespeed

import (
	"fmt"
	"testing"

	"github.com/angelospk/athens-transit-rt/internal/geo"
)

// One poll round of the whole network: 1500 buses each move 300 m on one of 150 roads of 10 km
// that a 20-minute window has filled, then each gets its 8 steps ahead.
func BenchmarkRound1500(b *testing.B) {
	g := New()
	roads := make([]*geo.Polyline, 150)
	for i := range roads {
		y := float64(i) * 500
		roads[i] = geo.NewPolyline([][2]float64{{0, y}, {10000, y}})
	}
	for t := int64(0); t < 1200; t += 30 { // 20 minutes of passes
		for v := 0; v < 1500; v++ {
			s := float64((v*67 + int(t)*8) % 9000)
			g.Pass(fmt.Sprint(v), roads[v%150], s, s+300, t, t+30)
		}
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		t := int64(1200 + 30*n) // time goes on: old passes leave the window, the load stays the same
		for v := 0; v < 1500; v++ {
			s := float64((v*67 + int(t)*8) % 9000)
			g.Pass(fmt.Sprint(v), roads[v%150], s, s+300, t, t+30)
			g.Blend(nil, roads[v%150], s+300, t+30)
		}
		if n%40 == 0 {
			g.Prune(t + 30) // housekeeping, every minute in the server
		}
	}
	b.ReportMetric(float64(g.Cells()), "cells")
}
