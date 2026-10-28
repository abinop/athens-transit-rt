package sched

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

var t0 = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

type fakeSlots struct{ rps float64 }

func (f *fakeSlots) Wait(ctx context.Context) error { return ctx.Err() }
func (f *fakeSlots) Report(time.Duration, error)    {}
func (f *fakeSlots) RPS() float64                   { return f.rps }

type fakeFetch struct {
	mu    sync.Mutex
	calls []string
	rows  map[string][]telematics.Vehicle
}

func (f *fakeFetch) BusLocations(_ context.Context, rc string) ([]telematics.Vehicle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rc)
	return f.rows[rc], nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newSched(rps float64, fetch Fetcher, c *clock) *Scheduler {
	s := New(DefaultConfig(), &fakeSlots{rps}, fetch, func(LinePoll) {})
	s.now = c.now
	return s
}

func routes(codes ...string) []Route {
	var out []Route
	for _, c := range codes {
		out = append(out, Route{Code: c, LineCode: "LC", Active: true})
	}
	return out
}

func TestAllocateWatchedCappedAtHalf(t *testing.T) {
	// 4 rps * 0.9 headroom = 3.6 rps. Watched demand 120 routes / 30 s = 4 rps > 1.8 cap.
	st := allocate(3.6, [numTiers]float64{Watched: 4, Dense: 1, Other: 0.5})
	if math.Abs(st[Watched]-4/1.8) > 1e-9 {
		t.Fatalf("watched stretch %v", st[Watched])
	}
	// Remaining 1.8 rps covers dense (1) and other (0.5): no stretch.
	if st[Dense] != 1 || st[Other] != 1 {
		t.Fatalf("dense %v other %v", st[Dense], st[Other])
	}
	// Small watched demand leaves the rest to others; others over budget get stretched.
	st = allocate(3.6, [numTiers]float64{Watched: 0.2, Dense: 2.4, Other: 2})
	if st[Watched] != 1 || st[Dense] != 1 || math.Abs(st[Other]-2/1.0) > 1e-9 {
		t.Fatalf("stretch %v", st)
	}
	// Dense alone would eat the budget: "other" still gets its 5-minute floor (stretch 2),
	// dense gets the rest, and watched only what is left.
	// Floors: dense 3.5*60/75 = 2.8, other 1*150/300 = 0.5; 0.3 rps left for watched.
	st = allocate(3.6, [numTiers]float64{Watched: 1.8, Dense: 3.5, Other: 1})
	if math.Abs(st[Other]-2) > 1e-9 || math.Abs(st[Dense]-1.25) > 1e-9 || math.Abs(st[Watched]-6) > 1e-9 {
		t.Fatalf("stretch %v", st)
	}
	// Codex example: watched still gets a small reserved rate, and the plan never exceeds
	// the budget.
	st = allocate(1, [numTiers]float64{Watched: 1, Dense: 2, Other: 2})
	if planned := 1/st[Watched] + 2/st[Dense] + 2/st[Other]; planned > 1+1e-9 {
		t.Fatalf("planned %.3f rps over a budget of 1 (stretch %v)", planned, st)
	}
	// Infeasible floors (1.6 + 1 > 1): dense and other share the budget in proportion.
	st = allocate(1, [numTiers]float64{Dense: 2, Other: 2})
	if math.Abs(st[Dense]-3.25) > 1e-9 || math.Abs(st[Other]-5.2) > 1e-9 {
		t.Fatalf("infeasible stretch %v", st)
	}
}

// B5: with every line watched, watched requests stay within their share of the slots even
// while non-watched lines are due.
func TestWatchedShareEnforced(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	var specs []LineSpec
	for i := 0; i < 40; i++ {
		specs = append(specs, LineSpec{ID: fmt.Sprintf("W%02d", i), Active: true, Routes: routes("w1", "w2", "w3")})
		specs = append(specs, LineSpec{ID: fmt.Sprintf("O%02d", i), Active: true, Routes: routes("o1", "o2")})
	}
	s.SetLines(specs)
	for i := 0; i < 40; i++ {
		s.Watch(fmt.Sprintf("W%02d", i))
	}
	for i := 0; i < 200; i++ {
		s.pick()
	}
	st := s.Stats()
	// 200 slots: watched gets at most half (+ the initial burst); all 80 "other" requests ran.
	if st.Sent[Watched] > 100+watchedBurst || st.Sent[Other] != 80 {
		t.Fatalf("sent %v", st.Sent)
	}
}

func TestClassify(t *testing.T) {
	c := DefaultConfig()
	now := t0
	l := &lineState{spec: LineSpec{Active: true}}
	if c.classify(l, now) != Other {
		t.Fatal("active line should be Other")
	}
	l.vehicles = 5
	if c.classify(l, now) != Dense {
		t.Fatal("5 vehicles should be Dense")
	}
	l.watchedAt = now.Add(-9 * time.Minute)
	if c.classify(l, now) != Watched {
		t.Fatal("recently requested line should be Watched")
	}
	l.watchedAt = now.Add(-11 * time.Minute)
	if c.classify(l, now) != Dense {
		t.Fatal("watch should expire after 10 min")
	}
	l.spec.Active = false
	l.watchedAt = now
	if c.classify(l, now) != Inactive {
		t.Fatal("inactive line is never polled, even when watched")
	}
}

func TestEarliestDueFirstAndRouteBatching(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	s.SetLines([]LineSpec{
		{ID: "A", Active: true, Routes: routes("a1", "a2")},
		{ID: "B", Active: true, Routes: routes("b1")},
	})
	s.Watch("B")
	s.replan()
	// B is watched: same due time, higher tier first; its routes come back to back.
	var got []string
	for i := 0; i < 3; i++ {
		r := s.pick()
		if r == nil {
			t.Fatalf("pick %d returned nil", i)
		}
		got = append(got, r.route.Code)
	}
	if got[0] != "b1" || got[1] != "a1" || got[2] != "a2" {
		t.Fatalf("order %v", got)
	}
	if s.pick() != nil {
		t.Fatal("nothing else is due")
	}
}

func TestPollCompletesAndReschedules(t *testing.T) {
	c := &clock{t0}
	fresh := telematics.Vehicle{VehNo: "1", RouteCode: "a1", Time: t0.Add(-time.Minute)}
	stale := telematics.Vehicle{VehNo: "2", RouteCode: "a1", Time: t0.Add(-6 * time.Minute)}
	f := &fakeFetch{rows: map[string][]telematics.Vehicle{"a1": {fresh, stale}}}
	var polls []LinePoll
	s := New(DefaultConfig(), &fakeSlots{4}, f, func(p LinePoll) { polls = append(polls, p) })
	s.now = c.now
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1", "a2")}})
	s.replan()
	for r := s.pick(); r != nil; r = s.pick() {
		s.execute(context.Background(), r)
	}
	if len(polls) != 1 {
		t.Fatalf("polls %d", len(polls))
	}
	p := polls[0]
	want := []match.Obs{{Vehicle: fresh, LineCode: "LC"}}
	if p.Line != "A" || len(p.Obs) != 1 || p.Obs[0].VehNo != want[0].VehNo || p.Obs[0].LineCode != "LC" || p.Failed != 0 {
		t.Fatalf("poll %+v", p)
	}
	next, polled := s.NextUpdate("A")
	if !polled || !next.After(t0.Add(DefaultConfig().Other.Base)) {
		t.Fatalf("next update %v %v", next, polled)
	}
	if s.pick() != nil {
		t.Fatal("line polled again before its interval")
	}
	c.t = t0.Add(DefaultConfig().Other.Base)
	if s.pick() == nil {
		t.Fatal("line not due after its interval")
	}
}

func TestEmptyRoutesSlowDown(t *testing.T) {
	c := &clock{t0}
	f := &fakeFetch{rows: map[string][]telematics.Vehicle{"a1": {{VehNo: "1", Time: t0}}}}
	s := newSched(4, f, c)
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1", "a2")}})
	poll := func() {
		s.replan()
		for r := s.pick(); r != nil; r = s.pick() {
			s.execute(context.Background(), r)
		}
	}
	for i := 0; i < 2; i++ {
		poll()
		c.t = c.t.Add(3 * time.Minute)
	}
	// a2 was empty in its last two polls (the last 3 min ago): skipped until 300 s have passed.
	f.calls = nil
	poll()
	if len(f.calls) != 1 || f.calls[0] != "a1" {
		t.Fatalf("calls %v, want only a1", f.calls)
	}
	// Watching the line polls every route again.
	c.t = c.t.Add(time.Minute)
	s.Watch("A")
	f.calls = nil
	poll()
	if len(f.calls) != 2 {
		t.Fatalf("watched calls %v", f.calls)
	}
}

func TestWatchPullsNextPollForward(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1")}})
	s.replan()
	for r := s.pick(); r != nil; r = s.pick() {
		s.execute(context.Background(), r)
	}
	before, _ := s.NextUpdate("A")
	c.t = t0.Add(10 * time.Second)
	s.Watch("A")
	after, _ := s.NextUpdate("A")
	if !after.Before(before) || after.After(t0.Add(40*time.Second)) {
		t.Fatalf("next update before %v after %v", before.Sub(t0), after.Sub(t0))
	}
}

func TestInactiveLinesNotPolled(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	s.SetLines([]LineSpec{{ID: "N", Active: false, Routes: routes("n1")}})
	s.Watch("N")
	s.replan()
	if s.pick() != nil {
		t.Fatal("inactive line polled")
	}
	if _, polled := s.NextUpdate("N"); polled {
		t.Fatal("inactive line reported as polled")
	}
}

func TestBackgroundGetsEveryFourthSlot(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8")}})
	s.replan()
	for i := 0; i < 3; i++ {
		s.Submit(func(context.Context) error { return nil })
	}
	var kinds []string
	for r := s.pick(); r != nil; r = s.pick() {
		if r.bg != nil {
			kinds = append(kinds, "bg")
		} else {
			kinds = append(kinds, "line")
		}
	}
	want := "line line line bg line line line bg line line bg"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	if st := s.Stats(); st.Sent[Other] != 8 || st.SentBackground != 3 || st.Sent[Watched] != 0 {
		t.Fatalf("sent %v background %d", st.Sent, st.SentBackground)
	}
	if ls := s.Lines(); len(ls) != 1 || ls[0].ID != "A" || ls[0].Tier != Other || !ls[0].Polling {
		t.Fatalf("lines %+v", ls)
	}
}

func TestBackgroundRuns(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1")}})
	s.replan()
	ran := false
	s.Submit(func(context.Context) error { ran = true; return nil })
	if r := s.pick(); r == nil || r.bg != nil {
		t.Fatal("a due line poll should go first")
	}
	r := s.pick()
	if r == nil || r.bg == nil {
		t.Fatal("background work not picked when idle")
	}
	s.execute(context.Background(), r)
	if !ran {
		t.Fatal("background work not run")
	}
}

func TestRunRespectsSlots(t *testing.T) {
	// Every request must take a slot: count Wait calls vs fetches.
	slots := &countingSlots{}
	f := &fakeFetch{}
	s := New(DefaultConfig(), slots, f, func(LinePoll) {})
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1", "a2", "a3")}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 3 || slots.waits < 3 {
		t.Fatalf("calls %d waits %d", len(f.calls), slots.waits)
	}
}

type countingSlots struct {
	mu    sync.Mutex
	waits int
}

func (c *countingSlots) Wait(ctx context.Context) error {
	c.mu.Lock()
	c.waits++
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return nil
	}
}
func (c *countingSlots) Report(time.Duration, error) {}
func (c *countingSlots) RPS() float64                { return 4 }

// A watched line with many routes: its queued routes also respect the watched share, and
// other lines get the slots in between.
func TestWatchedPendingRoutesRespectShare(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, fmt.Sprintf("w%d", i))
	}
	specs := []LineSpec{{ID: "W", Active: true, Routes: routes(many...)}}
	for i := 0; i < 20; i++ {
		specs = append(specs, LineSpec{ID: fmt.Sprintf("O%02d", i), Active: true, Routes: routes("o")})
	}
	s.SetLines(specs)
	s.Watch("W")
	for i := 0; i < 20; i++ {
		s.pick()
	}
	if st := s.Stats(); st.Sent[Watched] > 10+watchedBurst || st.Sent[Other] < 20-10-watchedBurst {
		t.Fatalf("sent %v", st.Sent)
	}
}

// pollAll runs every request that is due now.
func pollAll(s *Scheduler) {
	for r := s.pick(); r != nil; r = s.pick() {
		s.execute(context.Background(), r)
	}
}

// twoPolled returns a scheduler with lines A and B, both polled at t0, the clock at t0+d and
// B overdue by 50 s. Both routes always answer with a vehicle (no empty-route slowdown).
func twoPolled(c *clock, d time.Duration) *Scheduler {
	v := []telematics.Vehicle{{VehNo: "1", Time: t0}}
	s := newSched(4, &fakeFetch{rows: map[string][]telematics.Vehicle{"a1": v, "b1": v}}, c)
	s.SetLines([]LineSpec{
		{ID: "A", Active: true, Routes: routes("a1")},
		{ID: "B", Active: true, Routes: routes("b1")},
	})
	pollAll(s)
	c.t = t0.Add(d)
	s.lines["B"].nextDue = c.t.Add(-50 * time.Second)
	return s
}

// A user opens a line that was polled a while ago while another line is overdue: the opened
// line is polled at once, ahead of the overdue one, not after its watched interval.
func TestWatchPollsAtOnceAndJumpsQueue(t *testing.T) {
	c := &clock{t0}
	s := twoPolled(c, 20*time.Second)
	s.Watch("A")
	next, _ := s.NextUpdate("A")
	if limit := c.t.Add(time.Second/4 + 3*time.Second); next.After(limit) {
		t.Fatalf("next update in %v", next.Sub(c.t))
	}
	if r := s.pick(); r == nil || r.route.Code != "a1" {
		t.Fatalf("first pick %+v, want a1", r)
	}
	if r := s.pick(); r == nil || r.route.Code != "b1" {
		t.Fatalf("second pick %+v, want b1", r)
	}
}

// Data younger than urgentAfter is fresh enough: no extra poll.
func TestWatchRecentPollNotUrgent(t *testing.T) {
	c := &clock{t0}
	s := twoPolled(c, 5*time.Second)
	s.Watch("A")
	if r := s.pick(); r == nil || r.route.Code != "b1" {
		t.Fatalf("first pick %+v, want b1", r)
	}
	if r := s.pick(); r != nil {
		t.Fatalf("A polled again after 5 s: %+v", r)
	}
}

// The frontend refetches a watched line again and again; that must not force polls.
func TestRewatchNotUrgent(t *testing.T) {
	c := &clock{t0}
	s := twoPolled(c, 20*time.Second)
	s.Watch("A")
	pollAll(s) // urgent poll of A, then B
	c.t = c.t.Add(20 * time.Second)
	s.lines["B"].nextDue = c.t.Add(-50 * time.Second)
	s.Watch("A")
	if r := s.pick(); r == nil || r.route.Code != "b1" {
		t.Fatalf("first pick %+v, want b1", r)
	}
	if r := s.pick(); r != nil {
		t.Fatalf("watched A polled before its interval: %+v", r)
	}
}

// Opening a line whose poll is running adds nothing: its data is on the way.
func TestWatchDuringPollNotUrgent(t *testing.T) {
	c := &clock{t0}
	s := twoPolled(c, 20*time.Second)
	s.lines["A"].nextDue = c.t.Add(-60 * time.Second)
	r := s.pick() // A's poll starts (due first)
	if r == nil || r.route.Code != "a1" {
		t.Fatalf("pick %+v, want a1", r)
	}
	s.Watch("A")
	if s.lines["A"].urgent {
		t.Fatal("line being polled marked urgent")
	}
}

// Urgency ends with the watch: a line that drops out of the watched tier before its urgent
// poll does not keep jumping the queue.
func TestUrgentClearedWhenWatchExpires(t *testing.T) {
	c := &clock{t0}
	s := twoPolled(c, 20*time.Second)
	s.Watch("A")
	c.t = c.t.Add(DefaultConfig().WatchWindow + time.Second)
	s.replan()
	if s.lines["A"].urgent {
		t.Fatal("urgent after the watch expired")
	}
}

// Until a finished poll is published, NextUpdate must not announce the following poll:
// otherwise /v1/lines would cache the old payload for a whole interval.
func TestNextUpdateSoonWhilePublishing(t *testing.T) {
	c := &clock{t0}
	f := &fakeFetch{}
	var s *Scheduler
	var during time.Time
	s = New(DefaultConfig(), &fakeSlots{4}, f, func(p LinePoll) { during, _ = s.NextUpdate(p.Line) })
	s.now = c.now
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1")}})
	pollAll(s)
	if during.After(t0.Add(3 * time.Second)) {
		t.Fatalf("next update %v ahead while publishing", during.Sub(t0))
	}
	if after, _ := s.NextUpdate("A"); !after.After(t0.Add(time.Minute)) {
		t.Fatalf("next update %v ahead after publishing", after.Sub(t0))
	}
}

// An urgent poll still waits for watched credit, so mass opening of lines cannot take more
// than the watched share.
func TestUrgentRespectsWatchedCredit(t *testing.T) {
	c := &clock{t0}
	s := twoPolled(c, 20*time.Second)
	s.Watch("A")
	s.watchedCredit = -1 // pick adds WatchedShare: still below 1
	if r := s.pick(); r == nil || r.route.Code != "b1" {
		t.Fatalf("first pick %+v, want b1", r)
	}
}

// A line opened while its poll is being published gets the watched interval for its next
// poll, not the slower one it had when the poll started.
func TestWatchWhilePublishingUsesWatchedInterval(t *testing.T) {
	c := &clock{t0}
	var s *Scheduler
	s = New(DefaultConfig(), &fakeSlots{4}, &fakeFetch{}, func(p LinePoll) { s.Watch(p.Line) })
	s.now = c.now
	s.SetLines([]LineSpec{{ID: "A", Active: true, Routes: routes("a1")}})
	pollAll(s)
	if due := s.lines["A"].nextDue; due.After(t0.Add(DefaultConfig().Watched.Base)) {
		t.Fatalf("next poll due after %v", due.Sub(t0))
	}
}

// Work someone is waiting for (SubmitFirst) goes before queued background work such as the
// metadata bootstrap, in the order submitted among themselves.
func TestSubmitFirstJumpsTheQueue(t *testing.T) {
	c := &clock{t0}
	s := newSched(4, &fakeFetch{}, c)
	var order []string
	for _, name := range []string{"meta1", "meta2"} {
		s.Submit(func(context.Context) error { order = append(order, name); return nil })
	}
	for _, name := range []string{"user1", "user2"} {
		s.SubmitFirst(func(context.Context) error { order = append(order, name); return nil })
	}
	for r := s.pick(); r != nil; r = s.pick() {
		s.execute(context.Background(), r)
	}
	if got := strings.Join(order, " "); got != "user1 user2 meta1 meta2" {
		t.Fatalf("order %q", got)
	}
}

type reportSlots struct {
	fakeSlots
	got  []time.Duration
	errs int
}

func (r *reportSlots) Report(d time.Duration, err error) {
	r.got = append(r.got, d)
	if err != nil {
		r.errs++
	}
}

// A background job (a daily schedule is a big answer, slow by nature) must not slow the line polls
// down by its latency; its errors still count.
func TestBackgroundLatencyIsNotLoad(t *testing.T) {
	slots := &reportSlots{}
	s := New(DefaultConfig(), slots, &fakeFetch{}, func(LinePoll) {})
	slow := func(context.Context) error { time.Sleep(20 * time.Millisecond); return nil }
	s.execute(context.Background(), &request{bg: slow})
	s.execute(context.Background(), &request{bg: func(context.Context) error { return fmt.Errorf("boom") }})
	if len(slots.got) != 2 || slots.got[0] != 0 || slots.got[1] != 0 || slots.errs != 1 {
		t.Fatalf("reports %v errors %d", slots.got, slots.errs)
	}
}
