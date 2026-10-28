// Package sched decides which OASA route codes to poll and when, under one global request
// budget shared by the whole network.
//
// Unit of work: one line poll = getBusLocation for each of the line's active route codes,
// issued back to back, then handed to the matcher. Lines are polled earliest-due first. A
// line's interval depends on its tier:
//
//	Watched  requested through the API in the last WatchWindow  -> ~30 s, at most WatchedShare of the budget
//	Dense    at least DenseVehicles fresh vehicles last poll   -> ~60 s
//	Other    scheduled service now                              -> ~150 s (2-5 min)
//	Inactive no scheduled service now                           -> not polled
//
// When a tier's demand exceeds its share of the budget, its intervals are stretched. A line
// that becomes watched is polled at once if its data is older than urgentAfter.
package sched

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

type Tier int

const (
	Inactive Tier = iota
	Other
	Dense
	Watched
	numTiers
)

func (t Tier) String() string {
	return [...]string{"inactive", "other", "dense", "watched"}[t]
}

type TierConfig struct {
	Base time.Duration // interval when the budget allows
	Min  time.Duration // never poll faster than this
}

type Config struct {
	Watched, Dense, Other TierConfig
	EmptyRouteInterval    time.Duration // a route empty in its last 2 polls waits this long (unless watched)
	WatchWindow           time.Duration
	DenseVehicles         int
	WatchedShare          float64 // of the budget
	Headroom              float64 // fraction of the budget planned for line polls
	StaleAfter            time.Duration
	MaxInFlight           int
	MaxStretch            float64
}

func DefaultConfig() Config {
	return Config{
		Watched:            TierConfig{Base: 30 * time.Second, Min: 30 * time.Second},
		Dense:              TierConfig{Base: 60 * time.Second, Min: 60 * time.Second},
		Other:              TierConfig{Base: 150 * time.Second, Min: 120 * time.Second},
		EmptyRouteInterval: 300 * time.Second,
		WatchWindow:        10 * time.Minute,
		DenseVehicles:      5,
		WatchedShare:       0.5,
		Headroom:           0.9,
		StaleAfter:         5 * time.Minute,
		MaxInFlight:        8,
		MaxStretch:         20,
	}
}

func (c *Config) tier(t Tier) TierConfig {
	switch t {
	case Watched:
		return c.Watched
	case Dense:
		return c.Dense
	}
	return c.Other
}

// Slots hands out request slots (telematics.Pacer).
type Slots interface {
	Wait(ctx context.Context) error
	Report(latency time.Duration, err error)
	RPS() float64
}

type Fetcher interface {
	BusLocations(ctx context.Context, routeCode string) ([]telematics.Vehicle, error)
}

type Route struct {
	Code     string
	LineCode string
	Active   bool // has scheduled service now
}

type LineSpec struct {
	ID     string
	Active bool
	Routes []Route
}

// LinePoll is the outcome of one line poll: fresh vehicles of every polled route.
type LinePoll struct {
	Line           string
	Obs            []match.Obs
	Started, Done  time.Time
	Routes, Failed int
}

type lineState struct {
	spec      LineSpec
	tier      Tier
	interval  time.Duration
	nextDue   time.Time
	lastStart time.Time
	polled    bool // at least one poll finished
	polling   bool // from the poll start until its result is published
	urgent    bool // just opened by a user: polled before every other due line
	inFlight  int
	obs       []match.Obs
	routes    int
	failed    int
	vehicles  int // fresh vehicles in the last poll
	watchedAt time.Time
	empty     map[string]int       // consecutive empty answers per route code
	lastPoll  map[string]time.Time // per route code
}

type request struct {
	line  string
	tier  Tier
	route Route
	bg    func(ctx context.Context) error
}

type Scheduler struct {
	cfg    Config
	slots  Slots
	fetch  Fetcher
	onLine func(LinePoll)
	now    func() time.Time

	mu         sync.Mutex
	lines      map[string]*lineState
	pending    []*request
	background []func(ctx context.Context) error
	urgent     int // the first urgent entries of background are SubmitFirst work
	stretch    [numTiers]float64
	lastPlan   time.Time
	sinceBG    int // line requests since the last background request
	sent       [numTiers]int64
	sentBG     int64
	// watchedCredit enforces WatchedShare per slot: +share each slot, -1 per watched request.
	watchedCredit float64
}

// Slowest acceptable intervals: their rates are reserved before watched lines get extra.
const (
	MaxDenseInterval = 75 * time.Second
	MaxOtherInterval = 300 * time.Second
)

// watchedBurst caps the saved-up credit of the watched tier, in requests.
const watchedBurst = 4

// urgentAfter: a user opening a line whose last poll started longer ago than this gets a
// poll at once (see Watch).
const urgentAfter = 10 * time.Second

// bgEvery: while background work is queued, every bgEvery-th slot goes to it, so metadata
// (first boot: ~500 requests) is not starved by line polls.
const bgEvery = 4

func New(cfg Config, slots Slots, fetch Fetcher, onLine func(LinePoll)) *Scheduler {
	return &Scheduler{cfg: cfg, slots: slots, fetch: fetch, onLine: onLine, now: time.Now,
		lines: map[string]*lineState{}, watchedCredit: watchedBurst}
}

func (c *Config) classify(l *lineState, now time.Time) Tier {
	switch {
	case !l.spec.Active:
		return Inactive
	case !l.watchedAt.IsZero() && now.Sub(l.watchedAt) <= c.WatchWindow:
		return Watched
	case l.vehicles >= c.DenseVehicles:
		return Dense
	}
	return Other
}

// allocate returns the stretch factor per tier for a budget (requests/s) and per-tier demand
// (requests/s at base intervals).
func allocate(budget float64, demand [numTiers]float64) [numTiers]float64 {
	return allocateCfg(budget, demand, DefaultConfig())
}

func allocateCfg(budget float64, demand [numTiers]float64, cfg Config) [numTiers]float64 {
	var got [numTiers]float64
	remaining := budget
	take := func(t Tier, want float64) {
		g := min(max(want, 0), max(remaining, 0))
		got[t] += g
		remaining -= g
	}
	watchedCap := budget * cfg.WatchedShare
	// 1. A small reserve so watched lines never get nothing (their interval is then at most
	//    MaxStretch x base).
	take(Watched, min(demand[Watched]/cfg.MaxStretch, watchedCap))
	// 2. Floors: the rate that keeps dense and other lines within their slowest acceptable
	//    interval. If both do not fit, they share what is left in proportion.
	floorOf := func(t Tier, slowest time.Duration) float64 {
		return demand[t] * cfg.tier(t).Base.Seconds() / max(slowest, cfg.tier(t).Base).Seconds()
	}
	fD, fO := floorOf(Dense, MaxDenseInterval), floorOf(Other, MaxOtherInterval)
	if fD+fO > remaining && fD+fO > 0 {
		r := max(remaining, 0)
		got[Dense], got[Other], remaining = r*fD/(fD+fO), r*fO/(fD+fO), 0
	} else {
		take(Dense, fD)
		take(Other, fO)
	}
	// 3. Watched up to its share, then the rest to dense and other.
	take(Watched, min(demand[Watched], watchedCap)-got[Watched])
	take(Dense, demand[Dense]-got[Dense])
	take(Other, demand[Other]-got[Other])
	var st [numTiers]float64
	for t := range st {
		st[t] = 1
		if demand[t] > 0 {
			// No budget at all cannot happen in practice (budget = pacer rate x Headroom, and
			// the pacer never drops below 1/8 of its base rate). The plan is advisory either way:
			// every request still waits for a pacer slot.
			if got[t] <= 0 {
				st[t] = cfg.MaxStretch
			} else {
				st[t] = max(demand[t]/got[t], 1)
			}
		}
	}
	return st
}

// SetLines replaces the set of lines and their activity, keeping polling state.
func (s *Scheduler) SetLines(specs []LineSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, sp := range specs {
		seen[sp.ID] = true
		l := s.lines[sp.ID]
		if l == nil {
			l = &lineState{empty: map[string]int{}, lastPoll: map[string]time.Time{}, nextDue: s.now()}
			s.lines[sp.ID] = l
		}
		l.spec = sp
	}
	for id, l := range s.lines {
		if !seen[id] && !l.polling {
			delete(s.lines, id)
		}
	}
	s.replanLocked()
}

// Watch marks a line as requested by a user now. A line that just became watched and was
// not polled in the last urgentAfter is polled at once, ahead of other due lines (still
// within the watched share). Requests for a line already watched (the frontend refetching)
// change nothing, so clients cannot force polls.
func (s *Scheduler) Watch(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lines[line]
	if l == nil {
		return
	}
	now := s.now()
	wasWatched := s.cfg.classify(l, now) == Watched
	l.watchedAt = now
	if wasWatched || !l.spec.Active {
		return
	}
	if !l.polling && (!l.polled || now.Sub(l.lastStart) >= urgentAfter) {
		l.urgent, l.nextDue = true, now
	}
	s.replanLocked()
}

// IsWatched reports whether the line is in the watched tier.
func (s *Scheduler) IsWatched(line string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lines[line]
	return l != nil && s.cfg.classify(l, s.now()) == Watched
}

func (s *Scheduler) replan() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replanLocked()
}

func (s *Scheduler) routeDemand(l *lineState, t Tier, base time.Duration) float64 {
	d := 0.0
	for _, r := range l.spec.Routes {
		if !r.Active {
			continue
		}
		iv := base
		if t != Watched && l.empty[r.Code] >= 2 {
			iv = max(iv, s.cfg.EmptyRouteInterval)
		}
		d += 1 / iv.Seconds()
	}
	return d
}

func (s *Scheduler) replanLocked() {
	now := s.now()
	s.lastPlan = now
	var demand [numTiers]float64
	for _, l := range s.lines {
		l.tier = s.cfg.classify(l, now)
		if l.tier != Watched {
			l.urgent = false
		}
		if l.tier != Inactive {
			demand[l.tier] += s.routeDemand(l, l.tier, s.cfg.tier(l.tier).Base)
		}
	}
	s.stretch = allocateCfg(s.slots.RPS()*s.cfg.Headroom, demand, s.cfg)
	for _, l := range s.lines {
		if l.tier == Inactive {
			continue
		}
		tc := s.cfg.tier(l.tier)
		iv := max(time.Duration(float64(tc.Base)*s.stretch[l.tier]), tc.Min)
		if l.polled && !l.polling && (l.interval == 0 || iv < l.interval) {
			// A faster tier (e.g. just watched) brings the next poll forward.
			l.nextDue = minTime(l.nextDue, l.lastStart.Add(iv))
		}
		l.interval = iv
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// Submit queues one background request (e.g. metadata refresh); it runs only when no line
// poll is due, and takes a request slot like any other.
func (s *Scheduler) Submit(fn func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.background = append(s.background, fn)
}

// SubmitFirst queues one background request that someone is waiting for (an API caller): it
// goes before the queued background work, after the other waited-for requests.
func (s *Scheduler) SubmitFirst(fn func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.background = slices.Insert(s.background, s.urgent, fn)
	s.urgent++
}

// pick returns the next request to send, or nil.
func (s *Scheduler) pick() *request {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchedCredit = min(s.watchedCredit+s.cfg.WatchedShare, watchedBurst)
	if len(s.background) > 0 && s.sinceBG >= bgEvery-1 {
		return s.popBackground()
	}
	if r := s.pickLine(); r != nil {
		s.sinceBG++
		s.sent[r.tier]++
		if r.tier == Watched {
			s.watchedCredit--
		}
		return r
	}
	if len(s.background) > 0 {
		return s.popBackground()
	}
	return nil
}

func (s *Scheduler) popBackground() *request {
	fn := s.background[0]
	s.background = s.background[1:]
	if s.urgent > 0 {
		s.urgent--
	}
	s.sinceBG = 0
	s.sentBG++
	return &request{bg: fn}
}

func (s *Scheduler) pickLine() *request {
	// Queued routes of lines already being polled go first; watched ones only within the
	// watched share (the rest of their line waits while other lines use the slot).
	for i, r := range s.pending {
		if r.tier != Watched || s.watchedCredit >= 1 {
			s.pending = slices.Delete(s.pending, i, i+1)
			return r
		}
	}
	now := s.now()
	due := make([]*lineState, 0)
	for _, l := range s.lines {
		if l.tier != Inactive && !l.polling && !l.nextDue.After(now) {
			due = append(due, l)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		if a.urgent != b.urgent {
			return a.urgent
		}
		if !a.nextDue.Equal(b.nextDue) {
			return a.nextDue.Before(b.nextDue)
		}
		if a.tier != b.tier {
			return a.tier > b.tier
		}
		return a.spec.ID < b.spec.ID
	})
	for _, l := range due {
		if l.tier == Watched && s.watchedCredit < 1 {
			continue // over the watched share; lower tiers go first
		}
		var reqs []*request
		for _, r := range l.spec.Routes {
			if !r.Active {
				continue
			}
			if l.tier != Watched && l.empty[r.Code] >= 2 && now.Sub(l.lastPoll[r.Code]) < s.cfg.EmptyRouteInterval {
				continue
			}
			reqs = append(reqs, &request{line: l.spec.ID, tier: l.tier, route: r})
		}
		if len(reqs) == 0 {
			l.nextDue, l.urgent = now.Add(l.interval), false
			continue
		}
		l.polling, l.urgent, l.lastStart, l.inFlight = true, false, now, len(reqs)
		l.obs, l.routes, l.failed = nil, len(reqs), 0
		s.pending = append(s.pending, reqs[1:]...)
		return reqs[0]
	}
	return nil
}

func (s *Scheduler) execute(ctx context.Context, r *request) {
	start := time.Now()
	if r.bg != nil {
		// Only its errors: a background answer (a daily schedule) is big and slow by nature, and
		// its latency counted as load kept the pace at its slowest while the prefetch ran.
		s.slots.Report(0, r.bg(ctx))
		return
	}
	vs, err := s.fetch.BusLocations(ctx, r.route.Code)
	s.slots.Report(time.Since(start), err)

	s.mu.Lock()
	l := s.lines[r.line]
	if l == nil {
		s.mu.Unlock()
		return
	}
	now := s.now()
	if err != nil {
		l.failed++
	} else {
		l.lastPoll[r.route.Code] = now
		if len(vs) == 0 {
			l.empty[r.route.Code]++
		} else {
			l.empty[r.route.Code] = 0
		}
		for _, v := range vs {
			if v.TimeErr != nil || now.Sub(v.Time) > s.cfg.StaleAfter {
				continue
			}
			l.obs = append(l.obs, match.Obs{Vehicle: v, LineCode: r.route.LineCode})
		}
	}
	l.inFlight--
	if l.inFlight > 0 {
		s.mu.Unlock()
		return
	}
	l.polled = true
	l.nextDue = l.lastStart.Add(l.interval)
	if l.failed < l.routes {
		l.vehicles = len(l.obs)
	}
	poll := LinePoll{Line: r.line, Obs: slices.Clone(l.obs), Started: l.lastStart, Done: now,
		Routes: l.routes, Failed: l.failed}
	l.obs = nil
	s.mu.Unlock()
	s.onLine(poll)
	s.mu.Lock()
	l.polling = false
	// A replan during publication (e.g. the line was just watched) skips polling lines.
	l.nextDue = minTime(l.nextDue, l.lastStart.Add(l.interval))
	s.mu.Unlock()
}

// NextUpdate estimates when fresh data for a line will be published. polled=false for lines
// that are not polled (inactive or unknown).
func (s *Scheduler) NextUpdate(line string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lines[line]
	if l == nil || l.tier == Inactive {
		return time.Time{}, false
	}
	rps := max(s.slots.RPS(), 0.1)
	now := s.now()
	n := 0
	for _, r := range l.spec.Routes {
		if r.Active {
			n++
		}
	}
	if l.polling {
		return now.Add(time.Duration(float64(l.inFlight)/rps*float64(time.Second)) + 2*time.Second), true
	}
	start := l.nextDue
	if start.Before(now) {
		start = now
	}
	return start.Add(time.Duration(float64(n)/rps*float64(time.Second)) + 2*time.Second), true
}

// Stats summarises the plan for /v1/status, logs and metrics.
type Stats struct {
	LinesByTier    [numTiers]int
	Stretch        [numTiers]float64
	Sent           [numTiers]int64 // line requests handed out, by the line's tier at the time
	SentBackground int64
}

func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st Stats
	for _, l := range s.lines {
		st.LinesByTier[l.tier]++
	}
	st.Stretch, st.Sent, st.SentBackground = s.stretch, s.sent, s.sentBG
	return st
}

// LineInfo is the polling state of one line (metrics).
type LineInfo struct {
	ID        string
	Tier      Tier
	Interval  time.Duration
	NextDue   time.Time
	LastStart time.Time // zero before the first poll
	Polling   bool
}

func (s *Scheduler) Lines() []LineInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LineInfo, 0, len(s.lines))
	for _, l := range s.lines {
		out = append(out, LineInfo{ID: l.spec.ID, Tier: l.tier, Interval: l.interval, NextDue: l.nextDue,
			LastStart: l.lastStart, Polling: l.polling})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Run sends requests until ctx is cancelled: one per slot, at most MaxInFlight at once.
func (s *Scheduler) Run(ctx context.Context) {
	sem := make(chan struct{}, max(s.cfg.MaxInFlight, 1))
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		if s.now().Sub(s.lastPlanTime()) >= 5*time.Second {
			s.replan()
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		if err := s.slots.Wait(ctx); err != nil {
			<-sem
			return
		}
		r := s.pick()
		if r == nil {
			<-sem
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.execute(ctx, r)
		}()
	}
}

func (s *Scheduler) lastPlanTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPlan
}

// SetClock replaces the time source (tests).
func (s *Scheduler) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Tier returns a line's current tier (Inactive for unknown lines).
func (s *Scheduler) Tier(line string) Tier {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.lines[line]; l != nil {
		return l.tier
	}
	return Inactive
}
