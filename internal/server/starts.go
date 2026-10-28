package server

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/rail"
)

// The runs of OASA's daily timetable have no trip ids, so the seen flag of a GTFS trip cannot mark
// them. runStarts watches the buses leave instead: a bus seen near the start of its shape and then
// well along it left at its last fix near the start.
const (
	startNearM  = 300.0            // still at the start (waiting, or just pulling out)
	startAwayM  = 600.0            // gone: on its way
	startMaxGap = 15 * time.Minute // longer between the two fixes is not one departure
	seenEarly   = 3 * time.Minute  // a run left this early still counts
	seenLate    = 20 * time.Minute // ...or this late; after that without a bus it is missed
)

type startKey struct {
	line  string
	shape int32
}

type startState struct {
	key  startKey
	near time.Time // last fix near the start; zero once the run is counted
}

// A nil runStarts watches nothing.
type runStarts struct {
	mu    sync.Mutex
	day   string    // Athens date of the starts
	since time.Time // the first fix of the day: what left before it was not watched
	veh   map[string]*startState
	got   map[startKey][]time.Time
}

func newRunStarts() *runStarts { return &runStarts{} }

func (rs *runStarts) reset(day string, t time.Time) {
	rs.day, rs.since = day, t
	rs.veh, rs.got = map[string]*startState{}, map[startKey][]time.Time{}
}

// observe takes one fix of a vehicle: s metres along shape of line, at t.
func (rs *runStarts) observe(vehicle, line string, shape int32, s float64, t time.Time) {
	if rs == nil || shape < 0 {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if day := t.In(gtfs.Athens).Format(time.DateOnly); day != rs.day {
		rs.reset(day, t)
	}
	k := startKey{line, shape}
	st := rs.veh[vehicle]
	if st == nil || st.key != k {
		st = &startState{key: k}
		rs.veh[vehicle] = st
	}
	switch {
	case s <= startNearM:
		st.near = t
	case s >= startAwayM && !st.near.IsZero():
		if t.Sub(st.near) <= startMaxGap {
			rs.got[k] = append(rs.got[k], st.near)
		}
		st.near = time.Time{}
	}
}

// of returns the starts of a shape of line today (the Athens day of now) in time order, and since
// when the starts were watched.
func (rs *runStarts) of(line string, shape int32, now time.Time) ([]time.Time, time.Time) {
	if rs == nil {
		return nil, now
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.day != now.In(gtfs.Athens).Format(time.DateOnly) {
		return nil, now
	}
	got := append([]time.Time(nil), rs.got[startKey{line, shape}]...)
	sort.Slice(got, func(i, j int) bool { return got[i].Before(got[j]) })
	return got, rs.since
}

// forget drops everything: the shape indexes belong to a feed that was replaced.
func (rs *runStarts) forget() {
	if rs == nil {
		return
	}
	rs.mu.Lock()
	rs.day = ""
	rs.mu.Unlock()
}

// markSeen sets the seen flags of the runs of one direction on day (Athens midnight), in time
// order, from the starts of its buses (sorted): a run takes the first unused start from seenEarly
// before to seenLate after its time. A run without one is missed once seenLate has passed, and
// unknown before that or when it was due before since (not watched).
func markSeen(deps []ScheduleDeparture, day time.Time, starts []time.Time, since, now time.Time) {
	used := make([]bool, len(starts))
	for i := range deps {
		deps[i].Seen = nil
		m, ok := minutesOf(deps[i].T)
		if !ok {
			continue
		}
		t := day.Add(time.Duration(m) * time.Minute)
		if t.Add(-seenEarly).Before(since) || t.After(now) {
			continue
		}
		seen := false
		for k, s := range starts {
			if used[k] || s.Before(t.Add(-seenEarly)) {
				continue
			}
			if s.After(t.Add(seenLate)) {
				break
			}
			used[k], seen = true, true
			break
		}
		if seen || !now.Before(t.Add(seenLate)) {
			deps[i].Seen = &seen
		}
	}
}

// minutesOf reads "HH:MM" (hours past 24 for runs after midnight).
func minutesOf(hhmm string) (int, bool) {
	h, m, ok := strings.Cut(hhmm, ":")
	if !ok {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return hh*60 + mm, true
}

var bracketed = regexp.MustCompile(`\([^)]*\)`)

// endMatches tells whether a GTFS headsign ("START - VIA - END") ends at the place a daily
// direction is named after; bracketed notes do not count.
func endMatches(gtfsHeadsign, daily string) bool {
	end := bracketed.ReplaceAllString(gtfsHeadsign, "")
	if i := strings.LastIndex(end, " - "); i >= 0 {
		end = end[i+3:]
	}
	a := strings.TrimSpace(rail.Fold(end))
	b := strings.TrimSpace(rail.Fold(bracketed.ReplaceAllString(daily, "")))
	return a != "" && b != "" && (strings.Contains(a, b) || strings.Contains(b, a))
}

// markDailySeen sets the seen flags of today's daily timetable of line from the buses that left
// the start of a shape ending where the direction does.
func (a *App) markDailySeen(f *gtfs.Feed, line string, resp *ScheduleResponse, today time.Time) {
	now := a.now()
	shapes := map[int32]string{}
	for _, ti := range f.TripsForLine(line) {
		t := f.Trip(ti)
		if t.Shape >= 0 {
			if _, ok := shapes[t.Shape]; !ok {
				shapes[t.Shape] = f.Headsign(t)
			}
		}
	}
	for i := range resp.Directions {
		d := &resp.Directions[i]
		var starts []time.Time
		since := now
		for shape, head := range shapes {
			if !endMatches(head, d.Headsign) {
				continue
			}
			got, s := a.starts.of(line, shape, now)
			starts = append(starts, got...)
			since = s
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
		markSeen(d.Departures, today, starts, since, now)
	}
}
