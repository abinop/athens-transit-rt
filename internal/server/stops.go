package server

import (
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
)

const (
	stopMaxAge = 15
	staleFix   = 5 * time.Minute // an older fix is not promised at a stop: the bus may be gone
	maxEta     = 2 * time.Hour
)

type StopInfo struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

// StopArrival is a matched bus on its way to the stop: its timetable time there plus its delay.
type StopArrival struct {
	Line       string `json:"line"`
	Headsign   string `json:"headsign"`
	Variant    string `json:"variant"`
	Vehicle    string `json:"vehicle"`
	EtaS       int    `json:"eta_s"`
	At         int64  `json:"at"`
	DelayS     int    `json:"delay_s"`
	PositionAt int64  `json:"position_at"`
}

type StopResponse struct {
	Stop      StopInfo      `json:"stop"`
	Lines     []string      `json:"lines"`
	Arrivals  []StopArrival `json:"arrivals"`
	UpdatedAt int64         `json:"updated_at"`
}

// stopLines maps each stop to the lines whose trips call there, sorted; built once per feed.
func stopLines(f *gtfs.Feed) map[int32][]string {
	byPattern := map[int32]map[string]bool{}
	for i := range f.Trips {
		t := &f.Trips[i]
		if byPattern[t.Pattern] == nil {
			byPattern[t.Pattern] = map[string]bool{}
		}
		byPattern[t.Pattern][f.Line(t)] = true
	}
	out := map[int32][]string{}
	for p, lines := range byPattern {
		for _, s := range f.Patterns[p].Stops {
			for l := range lines {
				if !slices.Contains(out[s], l) {
					out[s] = append(out[s], l)
				}
			}
		}
	}
	for _, l := range out {
		sort.Strings(l)
	}
	return out
}

// arrivalsAt lists the matched buses of results still to call at stop, soonest first.
func arrivalsAt(f *gtfs.Feed, results []match.Result, stop int32, now time.Time) []StopArrival {
	out := []StopArrival{}
	for i := range results {
		r := &results[i]
		if !r.Matched() || now.Sub(r.Time) >= staleFix {
			continue
		}
		t := f.Trip(r.Trip)
		from := r.NextIndex
		if r.Waiting {
			from = 0
		}
		stops := f.Patterns[t.Pattern].Stops
		for k := from; k < len(stops); k++ {
			if stops[k] != stop {
				continue
			}
			at := r.Day.Add(time.Duration(int(f.StopTime(t, k).Arr)+r.Delay) * time.Second)
			if at.Sub(now) > maxEta {
				break
			}
			if at.Before(now) {
				at = now // late by its last fix: due now
			}
			a := StopArrival{Line: r.Line, Headsign: f.Headsign(t), Vehicle: r.VehicleID, EtaS: int(at.Sub(now) / time.Second),
				At: at.Unix(), DelayS: r.Delay, PositionAt: r.Time.Unix()}
			if t.Shape >= 0 {
				a.Variant = f.Shapes[t.Shape].ID
			}
			out = append(out, a)
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].EtaS < out[j].EtaS })
	return out
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	wd := a.world()
	if wd == nil {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	f := wd.feed
	s := f.StopIndex(r.PathValue("id"))
	if s < 0 {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_stop"})
		return
	}
	lines := wd.stopLines[s]
	if lines == nil {
		lines = []string{}
	}
	arr := []StopArrival{}
	a.mu.RLock()
	// A bus that changed lines keeps its old line's result for a while: only its newest fix counts.
	type fix struct {
		at   time.Time
		line string
	}
	newest := map[string]fix{}
	for id, d := range a.lines {
		if d.feed != f {
			continue
		}
		for i := range d.results {
			r := &d.results[i]
			if n, ok := newest[r.VehicleID]; !ok || r.Time.After(n.at) || r.Time.Equal(n.at) && id < n.line {
				newest[r.VehicleID] = fix{r.Time, id}
			}
		}
	}
	for _, l := range lines {
		d := a.lines[l]
		if d == nil || d.feed != f {
			continue
		}
		var mine []match.Result
		for _, r := range d.results {
			if newest[r.VehicleID].line == l {
				mine = append(mine, r)
			}
		}
		arr = append(arr, arrivalsAt(f, mine, s, now)...)
	}
	a.mu.RUnlock()
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].EtaS < arr[j].EtaS })
	for _, l := range lines {
		a.sched.Watch(l)
	}
	st := f.Stops[s]
	writeJSON(w, http.StatusOK, stopMaxAge, StopResponse{Stop: StopInfo{st.ID, st.Name, st.Lat, st.Lon},
		Lines: lines, Arrivals: arr, UpdatedAt: now.Unix()})
}
