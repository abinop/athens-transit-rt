package stats

import (
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
)

const (
	// MinCoverage is the share of a trip's shape its fixes must span for the trip to count as run.
	MinCoverage = 0.5
	// TripSlack is how long before a trip's start or after its end a fix still counts for it.
	TripSlack = 30 * time.Minute
	// TripsAfter is how long past a day's end its last trips run: Inputs reach this far.
	TripsAfter = 3 * time.Hour
)

// PlanTrip is one scheduled trip of a day. FromM and ToM are the metres along the shape of its
// first and last stop; they are equal when the trip has no usable shape.
type PlanTrip struct {
	ID    string  `json:"id"`
	Line  string  `json:"line"`
	Start int64   `json:"start"` // unix s
	End   int64   `json:"end"`
	FromM float64 `json:"from_m"`
	ToM   float64 `json:"to_m"`
}

// Plan is the schedule of one local day, from the GTFS feed version GTFS.
type Plan struct {
	GTFS    string     `json:"gtfs"`
	Expired bool       `json:"expired"` // the day lies outside the feed's validity
	Trips   []PlanTrip `json:"trips"`
}

// PlanFunc returns the plan of a local day (YYYY-MM-DD).
type PlanFunc func(date string) (*Plan, error)

// TripCount counts scheduled trips by what the history shows of them.
type TripCount struct {
	Planned    int `json:"planned"`
	Run        int `json:"run"`        // fixes cover at least MinCoverage of the shape
	Partial    int `json:"partial"`    // seen, but covering less
	Missed     int `json:"missed"`     // no fix at all
	Unmeasured int `json:"unmeasured"` // no usable shape to measure against
}

type TripLine struct {
	Line string `json:"line"`
	TripCount
	MissedTrips []string `json:"missed_trips"`
}

type Trips struct {
	GTFS        string  `json:"gtfs"`
	GTFSExpired bool    `json:"gtfs_expired"`
	MinCoverage float64 `json:"min_coverage"`
	TripCount
	ByLine []TripLine `json:"by_line"`
}

// NewPlan lists the trips of feed f (of the given version) that start in a local day. Times past
// 24:00 belong to the previous service day, and on a spring-forward day the service day counts
// from 23:00 the evening before, so D-1, D and D+1 are read.
func NewPlan(f *gtfs.Feed, version, date string, loc *time.Location) (*Plan, error) {
	start, end, err := Bounds(date, loc)
	if err != nil {
		return nil, err
	}
	p := &Plan{GTFS: version, Trips: []PlanTrip{}}
	if fs, fe := f.FeedStart(), f.FeedEnd(); (!fs.IsZero() && date < fs.Format(dateLayout)) ||
		(!fe.IsZero() && date > fe.Format(dateLayout)) {
		p.Expired = true
	}
	noon := time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, loc)
	m := match.New(f, nil, match.Greedy)
	for _, sd := range []time.Time{gtfs.ServiceDay(noon.AddDate(0, 0, -1)), gtfs.ServiceDay(noon), gtfs.ServiceDay(noon.AddDate(0, 0, 1))} {
		for i := range f.Trips {
			t := &f.Trips[i]
			if !f.ServiceActive(t.Service, sd) {
				continue
			}
			s := sd.Add(time.Duration(f.Start(t)) * time.Second)
			if s.Before(start) || !s.Before(end) {
				continue
			}
			pt := PlanTrip{ID: t.ID, Line: f.Line(t), Start: s.Unix(),
				End: sd.Add(time.Duration(f.End(t)) * time.Second).Unix()}
			if g := m.Geometry(t); g != nil && g.FromShape && len(g.StopAlong) > 0 {
				pt.FromM, pt.ToM = g.StopAlong[0], g.StopAlong[len(g.StopAlong)-1]
			}
			p.Trips = append(p.Trips, pt)
		}
	}
	sort.Slice(p.Trips, func(i, j int) bool {
		a, b := p.Trips[i], p.Trips[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.ID < b.ID
	})
	return p, nil
}

// tripCounter sums how the plan's trips ran from the matched history rows, one row at a time,
// so the rows need not be kept.
type tripCounter struct {
	plan          *Plan
	byID          map[string][]int
	state         []tripSeen
	stamped, ours int // rows of the day with a feed version, and those of the plan's
}

type tripSeen struct {
	any, measured bool
	lo, hi        float64
}

func newTripCounter(plan *Plan) *tripCounter {
	c := &tripCounter{plan: plan, byID: map[string][]int{}, state: make([]tripSeen, len(plan.Trips))}
	for i, pt := range plan.Trips {
		c.byID[pt.ID] = append(c.byID[pt.ID], i)
	}
	return c
}

// add counts one matched row: fix time t, trip id, feed version and metres along the shape (< 0
// when unknown).
func (c *tripCounter) add(t int64, trip, gtfsVer string, sm float64) {
	if gtfsVer != c.plan.GTFS {
		return
	}
	slack := int64(TripSlack / time.Second)
	for _, k := range c.byID[trip] {
		pt := &c.plan.Trips[k]
		if t < pt.Start-slack || t > pt.End+slack {
			continue
		}
		s := &c.state[k]
		s.any = true
		if sm < 0 {
			continue
		}
		sm := min(max(sm, pt.FromM), pt.ToM)
		if !s.measured {
			s.lo, s.hi, s.measured = sm, sm, true
		} else {
			s.lo, s.hi = min(s.lo, sm), max(s.hi, sm)
		}
	}
}

// stamp counts one row of the day (matched or not) by the feed version it was written with.
func (c *tripCounter) stamp(gtfsVer string) {
	c.stamped++
	if gtfsVer == c.plan.GTFS {
		c.ours++
	}
}

// result is the trips section; nil when the day's rows are mostly of another feed version.
func (c *tripCounter) result() *Trips {
	if c.ours == 0 || 2*c.ours < c.stamped {
		return nil
	}
	plan, state := c.plan, c.state
	out := &Trips{GTFS: plan.GTFS, GTFSExpired: plan.Expired, MinCoverage: MinCoverage}
	lines := map[string]*TripLine{}
	for k, pt := range plan.Trips {
		l := lines[pt.Line]
		if l == nil {
			l = &TripLine{Line: pt.Line}
			lines[pt.Line] = l
		}
		s := state[k]
		l.Planned++
		switch {
		case pt.ToM <= pt.FromM:
			l.Unmeasured++
		case !s.any:
			l.Missed++
			l.MissedTrips = append(l.MissedTrips, pt.ID)
		case s.measured && (s.hi-s.lo)/(pt.ToM-pt.FromM) >= MinCoverage:
			l.Run++
		default:
			l.Partial++
		}
	}
	out.ByLine = make([]TripLine, 0, len(lines))
	for _, l := range lines {
		out.Planned += l.Planned
		out.Run += l.Run
		out.Partial += l.Partial
		out.Missed += l.Missed
		out.Unmeasured += l.Unmeasured
		out.ByLine = append(out.ByLine, *l)
	}
	sort.Slice(out.ByLine, func(i, j int) bool { return out.ByLine[i].Line < out.ByLine[j].Line })
	return out
}
