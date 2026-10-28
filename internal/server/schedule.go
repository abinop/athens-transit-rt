package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/rail"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

const (
	scheduleMaxAge    = 60
	scheduleDaysAhead = 7

	dailyTimeout     = 8 * time.Second
	dailyNegativeTTL = 10 * time.Minute
)

// ScheduleResponse is the /v1/lines/{id}/schedule body.
type ScheduleResponse struct {
	Line       string              `json:"line"`
	Date       string              `json:"date"`
	Expired    bool                `json:"expired"`
	Source     string              `json:"source"` // "oasa-daily" or "gtfs"
	Directions []ScheduleDirection `json:"directions"`
}

// ScheduleDirection groups the trips of one direction that share a headsign.
type ScheduleDirection struct {
	Direction  int                 `json:"direction"`
	Headsign   string              `json:"headsign"`
	Departures []ScheduleDeparture `json:"departures"`
}

// ScheduleDeparture is one scheduled run of the line: its first stop departure, in GTFS style
// (times past midnight are "24:10"). Seen is set for today only.
type ScheduleDeparture struct {
	T    string `json:"t"`
	Trip string `json:"trip"`
	Seen *bool  `json:"seen,omitempty"`
}

// GET /v1/lines/{id}/schedule?date=YYYY-MM-DD: the timetable of a line, today by default.
func (a *App) handleSchedule(w http.ResponseWriter, r *http.Request) {
	line := CanonicalLine(r.PathValue("id"))
	a.mu.RLock()
	known := a.known[line]
	a.mu.RUnlock()
	if !known {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_line"})
		return
	}
	today := gtfs.Midnight(a.now())
	date := today
	if q := r.URL.Query().Get("date"); q != "" {
		d, err := time.ParseInLocation(time.DateOnly, q, gtfs.Athens)
		if err != nil || d.Before(today) || d.After(today.AddDate(0, 0, scheduleDaysAhead)) {
			writeJSON(w, http.StatusBadRequest, unknownMaxAge, map[string]string{"error": "bad_date"})
			return
		}
		date = d
	}
	w0 := a.world()
	if w0 == nil {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	if date.Equal(today) {
		if d, ok := a.dailyOfLine(r.Context(), line, today); ok {
			writeJSON(w, http.StatusOK, scheduleMaxAge, dailyToSchedule(line, date.Format(time.DateOnly), d))
			return
		}
	}
	resp := a.lineSchedule(w0.feed, line, date)
	resp.Source = "gtfs"
	writeJSON(w, http.StatusOK, scheduleMaxAge, resp)
}

// dailyEntry is the cached daily schedule of a line: a full success lasts for its Athens day; a
// failure (ok false), or a result with a line code missing (partial), until retry.
type dailyEntry struct {
	day     string
	d       telematics.DailySchedule
	ok      bool
	partial bool
	retry   time.Time
}

// dailyOfLine returns OASA's schedule of today for a line: the runs of all its line codes
// merged. ok is false when there is none (OASA down, empty, no line code): use the GTFS then.
func (a *App) dailyOfLine(ctx context.Context, line string, today time.Time) (telematics.DailySchedule, bool) {
	day := today.Format(time.DateOnly)
	now := a.now()
	a.dailyMu.Lock()
	e, cached := a.dailyCache[line]
	a.dailyMu.Unlock()
	if cached && e.day == day && ((e.ok && !e.partial) || now.Before(e.retry)) {
		return e.d, e.ok
	}
	var codes []string
	seen := map[string]bool{}
	for _, ref := range a.meta.LineRoutes()[line] {
		if ref.LineCode != "" && !seen[ref.LineCode] {
			seen[ref.LineCode] = true
			codes = append(codes, ref.LineCode)
		}
	}
	var merged telematics.DailySchedule
	partial := false
	for _, code := range codes {
		cctx, cancel := context.WithTimeout(ctx, dailyTimeout)
		d, err := a.daily(cctx, code)
		cancel()
		if err != nil {
			a.log.Warn("daily schedule", "line_code", code, "err", err)
			partial = true
			continue
		}
		if merged.Descr == "" {
			merged.Descr = d.Descr
		}
		merged.Circular = merged.Circular || d.Circular
		merged.Go = append(merged.Go, d.Go...)
		merged.Come = append(merged.Come, d.Come...)
	}
	sort.Strings(merged.Go)
	sort.Strings(merged.Come)
	ok := len(merged.Go)+len(merged.Come) > 0
	if !ok && ctx.Err() != nil {
		return telematics.DailySchedule{}, false // the client left: not OASA's fault, do not cache
	}
	e = dailyEntry{day: day, d: merged, ok: ok, partial: partial, retry: now.Add(dailyNegativeTTL)}
	a.dailyMu.Lock()
	if a.dailyCache == nil {
		a.dailyCache = map[string]dailyEntry{}
	}
	a.dailyCache[line] = e
	a.dailyMu.Unlock()
	return merged, ok
}

// dailyToSchedule shapes OASA's daily schedule like the GTFS timetable: the Go runs are
// direction 0 (headsign: the last place of the description), the Come runs direction 1
// (headsign: the first place). Seen flags and trip ids do not exist for these runs.
func dailyToSchedule(line, date string, d telematics.DailySchedule) ScheduleResponse {
	resp := ScheduleResponse{Line: line, Date: date, Source: "oasa-daily", Directions: []ScheduleDirection{}}
	deps := func(ts []string) []ScheduleDeparture {
		out := make([]ScheduleDeparture, len(ts))
		for i, t := range ts {
			out[i] = ScheduleDeparture{T: t}
		}
		return out
	}
	if len(d.Go) > 0 {
		head := d.Descr
		if i := strings.LastIndex(head, " - "); i >= 0 {
			head = head[i+3:]
		}
		resp.Directions = append(resp.Directions, ScheduleDirection{Direction: 0, Headsign: head, Departures: deps(d.Go)})
	}
	if len(d.Come) > 0 {
		head := d.Descr
		if i := strings.Index(head, " - "); i >= 0 {
			head = head[:i]
		}
		resp.Directions = append(resp.Directions, ScheduleDirection{Direction: 1, Headsign: head, Departures: deps(d.Come)})
	}
	return resp
}

// lineSchedule lists the trips of a line that run on a local day, by direction and headsign. With
// an expired feed the same weekday inside the feed is planned (gtfs.ServiceActive), and expired
// tells the client the timetable is not the published one.
func (a *App) lineSchedule(f *gtfs.Feed, line string, date time.Time) ScheduleResponse {
	type group struct {
		dir      int8
		headsign string
	}
	type departure struct {
		secs int32
		dep  ScheduleDeparture
	}
	resp := ScheduleResponse{Line: line, Date: date.Format(time.DateOnly),
		Expired: !f.FeedEnd().IsZero() && date.After(f.FeedEnd()), Directions: []ScheduleDirection{}}
	day := gtfs.ServiceDay(date)
	today := date.Equal(gtfs.Midnight(a.now()))
	groups := map[group][]departure{}

	a.seenMu.Lock()
	// Only the flags of today count: the map still holds yesterday's until the next poll resets
	// it, and showing them on another day's timetable would be wrong.
	seenToday := a.seenDay == date.Format(time.DateOnly)
	for _, ti := range f.TripsForLine(line) {
		t := &f.Trips[ti]
		if !f.ServiceActive(t.Service, day) {
			continue
		}
		g := group{int8(t.Dir), f.Headsign(t)}
		dep := ScheduleDeparture{T: rail.HHMM(f.Start(t)), Trip: t.ID}
		if today {
			seen := seenToday && a.seen[t.ID]
			dep.Seen = &seen
		}
		groups[g] = append(groups[g], departure{secs: f.Start(t), dep: dep})
	}
	a.seenMu.Unlock()

	for g, deps := range groups {
		sort.SliceStable(deps, func(i, j int) bool {
			if deps[i].secs != deps[j].secs {
				return deps[i].secs < deps[j].secs
			}
			return deps[i].dep.Trip < deps[j].dep.Trip
		})
		d := ScheduleDirection{Direction: int(g.dir), Headsign: g.headsign, Departures: make([]ScheduleDeparture, len(deps))}
		for i := range deps {
			d.Departures[i] = deps[i].dep
		}
		resp.Directions = append(resp.Directions, d)
	}
	sort.SliceStable(resp.Directions, func(i, j int) bool {
		a, b := resp.Directions[i], resp.Directions[j]
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		return a.Headsign < b.Headsign
	})
	return resp
}

// noteSeen remembers the trips a poll has matched a vehicle to, for the seen flag of the line
// schedule. It forgets everything at midnight (Athens) and when the static feed changes.
func (a *App) noteSeen(w *world, results []match.Result, now time.Time) {
	day := gtfs.Midnight(now).Format(time.DateOnly)
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	if day != a.seenDay {
		a.seen, a.seenDay = map[string]bool{}, day
	}
	today := gtfs.ServiceDay(now)
	for i := range results {
		// Only a trip of today's service day: an overnight run of yesterday's service day is a
		// departure of yesterday's timetable, not of today's.
		if results[i].Matched() && results[i].Day.Equal(today) {
			a.seen[w.feed.Trips[results[i].Trip].ID] = true
		}
	}
}

// forgetSeen drops the seen trips: they belong to the feed that was just replaced.
func (a *App) forgetSeen() {
	a.seenMu.Lock()
	a.seen, a.seenDay = map[string]bool{}, ""
	a.seenMu.Unlock()
}
