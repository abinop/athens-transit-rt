// Package meta keeps the telematics metadata the poller needs: which LineCodes a line number
// has, which RouteCodes each LineCode has, and (for route codes that are not GTFS shape ids)
// the route's stop codes. It is persisted so a restart does not cost ~500 OASA requests.
package meta

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

const RefreshEvery = 24 * time.Hour

// LineCodes left without routes (OASA failed or answered empty) are asked again after
// IncompleteRetry, at most MaxMissingRetries times per daily refresh; only those LineCodes.
const (
	IncompleteRetry   = 5 * time.Minute
	MaxMissingRetries = 6
)

type Data struct {
	FetchedAt int64                         `json:"fetched_at"` // when Lines and Routes were complete
	Lines     []telematics.Line             `json:"lines"`
	Routes    map[string][]telematics.Route `json:"routes"` // by LineCode
	Stops     map[string][]string           `json:"stops"`  // by RouteCode
}

// API is the part of the telematics client the store uses.
type API interface {
	Lines(ctx context.Context) ([]telematics.Line, error)
	Routes(ctx context.Context, lineCode string) ([]telematics.Route, error)
	Stops(ctx context.Context, routeCode string) ([]telematics.RouteStop, error)
}

// Submitter runs one request in a scheduler slot (sched.Scheduler.Submit).
type Submitter func(func(ctx context.Context) error)

// Store is safe for concurrent use.
type Store struct {
	path   string
	api    API
	submit Submitter
	now    func() time.Time

	mu         sync.Mutex
	data       Data
	refreshing bool
	next       *Data // refresh in progress
	pendingLC  int
	partial    bool                 // the refresh in progress only re-asks missing LineCodes
	missTries  int                  // missing-LineCode retries since the last full refresh
	missAt     time.Time            // last missing-LineCode retry
	stopsAsked map[string]time.Time // route code -> do not ask again before
	onChange   func()
}

// Open loads the persisted metadata (if any). onChange is called after routes change.
func Open(path string, api API, submit Submitter, onChange func()) *Store {
	s := &Store{path: path, api: api, submit: submit, now: time.Now, onChange: onChange,
		stopsAsked: map[string]time.Time{}}
	s.data = Data{Routes: map[string][]telematics.Route{}, Stops: map[string][]string{}}
	if b, err := os.ReadFile(path); err == nil {
		var d Data
		if json.Unmarshal(b, &d) == nil && d.Routes != nil {
			if d.Stops == nil {
				d.Stops = map[string][]string{}
			}
			for rc, st := range d.Stops {
				if len(st) == 0 {
					delete(d.Stops, rc) // older caches stored empty answers; ask again
				}
			}
			s.data = d
		}
	}
	return s
}

// LineRoutes maps a line number (LineID) to its route codes, each with its LineCode.
type LineRoutes map[string][]RouteRef

type RouteRef struct{ Code, LineCode string }

func (s *Store) LineRoutes() LineRoutes {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := LineRoutes{}
	for _, l := range s.data.Lines {
		for _, r := range s.data.Routes[l.LineCode] {
			out[l.LineID] = append(out[l.LineID], RouteRef{r.RouteCode, l.LineCode})
		}
	}
	for _, refs := range out {
		sort.Slice(refs, func(i, j int) bool { return refs[i].Code < refs[j].Code })
	}
	return out
}

// LineCodes returns the telematics line codes of a line number, in metadata order, with or
// without known routes.
func (s *Store) LineCodes(lineID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, l := range s.data.Lines {
		if l.LineID == lineID && l.LineCode != "" && !seen[l.LineCode] {
			seen[l.LineCode] = true
			out = append(out, l.LineCode)
		}
	}
	return out
}

// LineIDs returns every telematics line number.
func (s *Store) LineIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, l := range s.data.Lines {
		if !seen[l.LineID] {
			seen[l.LineID] = true
			out = append(out, l.LineID)
		}
	}
	return out
}

// EmptyStopsRetry: a route whose stop list came back empty is asked again after this long.
const EmptyStopsRetry = 30 * time.Minute

// Stops returns a route's stop codes. When unknown it queues a fetch and returns ok=false.
func (s *Store) Stops(routeCode string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.data.Stops[routeCode]; len(st) > 0 {
		return st, true
	}
	if !s.now().Before(s.stopsAsked[routeCode]) {
		s.stopsAsked[routeCode] = time.Unix(1<<40, 0) // in flight
		s.submit(func(ctx context.Context) error {
			stops, err := s.api.Stops(ctx, routeCode)
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				delete(s.stopsAsked, routeCode) // retry on a later call
				return err
			}
			if len(stops) == 0 {
				s.stopsAsked[routeCode] = s.now().Add(EmptyStopsRetry)
				return nil
			}
			codes := make([]string, len(stops))
			for i, st := range stops {
				codes[i] = st.StopCode
			}
			s.data.Stops[routeCode] = codes
			s.saveLocked()
			return nil
		})
	}
	return nil, false
}

// Due reports whether a refresh should start: the daily full one, or a retry of LineCodes
// that have no routes yet.
func (s *Store) Due() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshing {
		return false
	}
	return s.fullDueLocked() || s.missingDueLocked()
}

func (s *Store) fullDueLocked() bool {
	return s.now().Sub(time.Unix(s.data.FetchedAt, 0)) >= RefreshEvery
}

func (s *Store) missingDueLocked() bool {
	last := time.Unix(s.data.FetchedAt, 0)
	if s.missAt.After(last) {
		last = s.missAt
	}
	return s.missTries < MaxMissingRetries && len(s.missingLocked()) > 0 && s.now().Sub(last) >= IncompleteRetry
}

// missingLocked lists LineCodes without routes.
func (s *Store) missingLocked() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range s.data.Lines {
		if !seen[l.LineCode] && len(s.data.Routes[l.LineCode]) == 0 {
			out = append(out, l.LineCode)
		}
		seen[l.LineCode] = true
	}
	sort.Strings(out)
	return out
}

// Incomplete reports whether the routes of some lines are not known yet and are still being
// asked for (first boot, or LineCodes that OASA failed for).
func (s *Store) Incomplete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.FetchedAt == 0 || (s.missTries < MaxMissingRetries && len(s.missingLocked()) > 0)
}

// Refresh re-fetches lines and routes through the submitter (or, when only that is due, the
// routes of LineCodes that have none). While it runs the old data stays in use; on first boot
// (no data) routes become usable line by line.
func (s *Store) Refresh() {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		return
	}
	if !s.fullDueLocked() {
		missing := s.missingLocked()
		if len(missing) == 0 {
			s.mu.Unlock()
			return
		}
		s.refreshing, s.partial = true, true
		s.missTries++
		s.missAt = s.now()
		routes := make(map[string][]telematics.Route, len(s.data.Routes))
		for k, v := range s.data.Routes {
			routes[k] = v
		}
		s.next = &Data{FetchedAt: s.data.FetchedAt, Lines: s.data.Lines, Routes: routes, Stops: s.data.Stops}
		s.pendingLC = len(missing)
		s.mu.Unlock()
		for _, code := range missing {
			s.submit(s.fetchRoutes(code, true, 1))
		}
		return
	}
	s.refreshing, s.partial = true, false
	s.mu.Unlock()
	s.submit(func(ctx context.Context) error {
		lines, err := s.api.Lines(ctx)
		if err != nil || len(lines) == 0 {
			s.mu.Lock()
			s.refreshing = false
			s.mu.Unlock()
			return err
		}
		s.mu.Lock()
		s.next = &Data{Lines: lines, Routes: map[string][]telematics.Route{}, Stops: s.data.Stops}
		codes := map[string]bool{}
		for _, l := range lines {
			codes[l.LineCode] = true
		}
		s.pendingLC = len(codes)
		bootstrap := len(s.data.Lines) == 0
		if bootstrap {
			s.data.Lines = lines
		}
		s.mu.Unlock()
		for code := range codes {
			s.submit(s.fetchRoutes(code, bootstrap, 1))
		}
		return nil
	})
}

const maxAttempts = 3

func (s *Store) fetchRoutes(lineCode string, bootstrap bool, attempt int) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		routes, err := s.api.Routes(ctx, lineCode)
		if err != nil && attempt < maxAttempts {
			s.submit(s.fetchRoutes(lineCode, bootstrap, attempt+1)) // retry later, behind other work
			return err
		}
		s.mu.Lock()
		if (err != nil || len(routes) == 0) && len(s.data.Routes[lineCode]) > 0 {
			routes = s.data.Routes[lineCode] // keep what we had
		}
		s.next.Routes[lineCode] = routes
		if bootstrap && len(routes) > 0 {
			s.data.Routes[lineCode] = routes
		}
		s.pendingLC--
		done := s.pendingLC == 0
		if done {
			if !s.partial {
				s.next.FetchedAt = s.now().Unix()
				s.missTries = 0
			}
			s.data = *s.next
			s.next = nil
			s.refreshing = false
			s.saveLocked()
		}
		cb := s.onChange
		s.mu.Unlock()
		if (bootstrap || done) && cb != nil {
			cb()
		}
		return err
	}
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(s.data)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.path)
	}
}

// Snapshot returns a copy of the current data (for tests and status).
func (s *Store) Snapshot() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}
