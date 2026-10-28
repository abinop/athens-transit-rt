// Package server runs the live service: it keeps the static GTFS snapshot current, polls
// OASA through the scheduler, matches vehicles to trips and serves the contract API.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	rt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angelospk/athens-transit-rt/internal/feed"
	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/history"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/meta"
	"github.com/angelospk/athens-transit-rt/internal/rail"
	"github.com/angelospk/athens-transit-rt/internal/release"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

const (
	ExpiryWarningDays = 7
	feedMaxAge        = 5 * time.Minute // vehicles older than this leave the GTFS-RT feeds
	lineKeep          = 5 * time.Minute // results of a line that stopped being polled
)

type Config struct {
	Listen        string
	MetricsListen string // loopback address for /metrics ("" = off)
	StateDir      string
	RPS           float64
	Matcher       match.Kind
	ReleaseURL    string        // snapshot release base URL ("" = release.DefaultBaseURL)
	GTFSZip       string        // load this zip instead of the release snapshot (local use)
	TelematicsURL string        // "" = telematics.BaseURL
	STASYURL      string        // STASY timetable catalogue ("" = rail.DefaultSTASYCatalog)
	SuburbanURL   string        // railway.gov.gr base URL ("" = rail.DefaultRailwayURL)
	MPassURL      string        // ose.mpass.ltd API base URL ("" = rail.DefaultMPassURL)
	Proxy         string        // proxy URL for OASA requests, e.g. socks5h://127.0.0.1:40001 ("" = direct)
	ReleaseCheck  time.Duration // how often to look for a new snapshot
	Sched         sched.Config
	// Fix history (docs/superpowers/specs/2026-10-06-fix-history.md): off when HistoryMaxBytes is 0.
	HistoryMaxBytes int64
	HistoryKeep     time.Duration
}

type world struct {
	feed     *gtfs.Feed
	manifest *release.Manifest
	matcher  *match.Matcher
	mapper   *match.RouteMapper
}

// lineData is the last published state of one line.
type lineData struct {
	updated  time.Time
	feed     *gtfs.Feed
	results  []match.Result
	vehicles []Vehicle
}

type App struct {
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
	pacer  *telematics.Pacer
	client *telematics.Client
	sched  *sched.Scheduler
	meta   *meta.Store
	fetch  *release.Fetcher

	matchMu sync.Mutex // serialises matching (matchers are not concurrency-safe) and motion
	motion  *motion

	mu           sync.RWMutex
	w            *world
	lines        map[string]*lineData
	known        map[string]bool
	warming      map[string]bool // scheduled now, but its route codes are not known yet
	active       int
	lastPublish  time.Time
	lastOK       time.Time
	warnedDay    string
	lastRequests int64

	rtMu    sync.Mutex
	rtBuilt time.Time
	rtFeeds map[string][]byte

	vehMu   sync.Mutex // lock order: vehMu, then mu
	vehSnap *vehiclesSnapshot

	hist *history.Writer                // nil: no fix history
	segs atomic.Pointer[segspeed.Table] // typical speeds from the history; nil until built

	stasy    *rail.STASYStore
	suburban *rail.Suburban

	seenMu  sync.Mutex
	seen    map[string]bool // trips the matcher has assigned a vehicle to, today
	seenDay string

	daily      func(ctx context.Context, lineCode string) (telematics.DailySchedule, error)
	dailyMu    sync.Mutex // guards dailyCache only; never held during a request
	dailyCache map[string]dailyEntry
	dailyCalls map[string]*dailyCall // fetches in flight, by line

	stopArrivals func(ctx context.Context, stopCode string) ([]telematics.Arrival, error)
	arrMu        sync.Mutex // guards arrCache only; never held during a request
	arrCache     map[string]arrivalsEntry

	mcpOnce    sync.Once
	mcpSrv     *mcp.Server
	apiOnce    sync.Once
	apiHandler http.Handler // the app's own REST handler, for the MCP tools
}

func New(cfg Config, log *slog.Logger) *App {
	a := &App{cfg: cfg, log: log, now: time.Now, lines: map[string]*lineData{}, known: map[string]bool{},
		motion: newMotion()}
	a.pacer = telematics.NewPacer(cfg.RPS)
	a.client = telematics.New(cfg.TelematicsURL, nil) // the scheduler paces every request
	a.client.MinGap = a.pacer.BaseInterval()
	if cfg.Proxy != "" {
		// Validated by the serve command; the Transport dials socks5/socks5h and http proxies.
		if u, err := url.Parse(cfg.Proxy); err == nil {
			a.client.SetTransport(&http.Transport{Proxy: http.ProxyURL(u), MaxIdleConnsPerHost: 8,
				IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second})
		}
	}
	a.daily = a.dailyQueued
	a.stopArrivals = a.stopArrivalsQueued
	a.sched = sched.New(cfg.Sched, a.pacer, a.client, a.onLine)
	a.meta = meta.Open(filepath.Join(cfg.StateDir, "telematics-meta.json"), a.client, a.sched.Submit,
		func() { a.refreshLines() })
	base := cfg.ReleaseURL
	if base == "" {
		base = release.DefaultBaseURL
	}
	a.fetch = &release.Fetcher{BaseURL: base, Dir: cfg.StateDir, HTTP: &http.Client{Timeout: 2 * time.Minute}}
	stasyURL := cfg.STASYURL
	if stasyURL == "" {
		stasyURL = rail.DefaultSTASYCatalog
	}
	a.stasy = &rail.STASYStore{CatalogURL: stasyURL, Dir: filepath.Join(cfg.StateDir, "stasy"),
		HTTP: &http.Client{Timeout: 5 * time.Minute}, Log: log, Now: func() time.Time { return a.now() }}
	a.suburban = &rail.Suburban{BaseURL: cfg.SuburbanURL, MPassURL: cfg.MPassURL,
		// Well under the 30 s WriteTimeout of the HTTP server: the MPass fallback and the response
		// still have to fit after a dead primary upstream.
		HTTP: &http.Client{Timeout: 8 * time.Second}, Now: func() time.Time { return a.now() }}
	return a
}

// SetFeed installs a static feed (new matcher state; published results stay until replaced).
func (a *App) SetFeed(f *gtfs.Feed, m *release.Manifest) {
	mapper := match.NewRouteMapper(f, a.meta.Stops)
	w := &world{feed: f, manifest: m, mapper: mapper, matcher: match.New(f, mapper, a.cfg.Matcher)}
	a.matchMu.Lock()
	a.mu.Lock()
	a.w = w
	a.mu.Unlock()
	a.motion = newMotion() // positions along the old feed's shapes mean nothing now
	// Trip ids of the old feed mean nothing either. Cleared before matchMu opens, so a poll that
	// starts right after cannot record its matches into a map that is dropped a moment later.
	a.forgetSeen()
	a.matchMu.Unlock()
	a.vehMu.Lock()
	a.vehSnap = nil
	a.vehMu.Unlock()
	a.log.Info("static GTFS loaded", "version", a.gtfsVersion(), "trips", len(f.Trips), "lines", len(f.Lines()))
	a.warnExpiry(true)
	a.refreshLines()
}

func (a *App) world() *world {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.w
}

func (a *App) gtfsVersion() string { return worldVersion(a.world()) }

// worldVersion is the GTFS version of a world, "" without one.
func worldVersion(w *world) string {
	if w == nil {
		return ""
	}
	if w.manifest != nil && w.manifest.GTFSVersion != "" {
		return w.manifest.GTFSVersion
	}
	return w.feed.Version()
}

func (a *App) gtfsExpires() string {
	w := a.world()
	if w == nil || w.feed.FeedEnd().IsZero() {
		return ""
	}
	return w.feed.FeedEnd().Format(time.DateOnly)
}

// warnExpiry logs once a day when the static GTFS expires within ExpiryWarningDays.
func (a *App) warnExpiry(force bool) {
	w := a.world()
	if w == nil {
		return
	}
	end := w.feed.FeedEnd()
	if end.IsZero() {
		return
	}
	today := gtfs.Midnight(a.now())
	day := today.Format(time.DateOnly)
	if !force && day == a.warnedDay {
		return
	}
	a.warnedDay = day
	left := int(end.Sub(today).Hours() / 24)
	switch {
	case left < 0:
		a.log.Warn("static GTFS expired; no trips can be matched until OASA publishes a new one", "end", end.Format(time.DateOnly))
	case left <= ExpiryWarningDays:
		a.log.Warn("static GTFS expires soon", "end", end.Format(time.DateOnly), "days_left", left)
	}
}

// refreshLines recomputes which lines and route codes have scheduled service now.
func (a *App) refreshLines() {
	w := a.world()
	if w == nil {
		return
	}
	now := a.now()
	routes := a.meta.LineRoutes()
	known := map[string]bool{}
	for _, l := range w.feed.Lines() {
		known[l] = true
	}
	for _, l := range a.meta.LineIDs() {
		known[l] = true
	}
	var specs []sched.LineSpec
	active := 0
	warming := map[string]bool{}
	bootstrapping := a.meta.Incomplete() // routes of some lines are not known yet
	plan := w.feed.PlanningTime(now)     // which lines run now, even when the feed expired
	for line := range known {
		cands := w.feed.CandidateTrips(line, plan, match.CandidateBefore, match.CandidateAfter)
		shapesNow := map[int32]bool{}
		for _, c := range cands {
			shapesNow[w.feed.Trips[c.Trip].Shape] = true
		}
		spec := sched.LineSpec{ID: line, Active: len(cands) > 0 && len(routes[line]) > 0}
		if len(cands) > 0 && len(routes[line]) == 0 && bootstrapping {
			warming[line] = true
		}
		for _, r := range routes[line] {
			on := spec.Active
			if on {
				if shapes := w.mapper.ShapesFor(line, r.Code); len(shapes) > 0 {
					on = false
					for s := range shapes {
						on = on || shapesNow[s]
					}
				}
			}
			spec.Routes = append(spec.Routes, sched.Route{Code: r.Code, LineCode: r.LineCode, Active: on})
		}
		if spec.Active {
			active++
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	a.sched.SetLines(specs)
	a.mu.Lock()
	a.known, a.active, a.warming = known, active, warming
	a.mu.Unlock()
}

// onLine matches one finished line poll and publishes it.
func (a *App) onLine(p sched.LinePoll) {
	a.matchMu.Lock()
	w := a.world()
	if w == nil {
		a.matchMu.Unlock()
		return
	}
	results := w.matcher.MatchLine(p.Line, p.Obs)
	a.noteSeen(w, results, p.Done)
	vehicles := make([]Vehicle, len(results))
	ver := ""
	if a.hist != nil {
		ver = a.gtfsVersion()
	}
	for i := range results {
		vehicles[i] = a.vehicleView(w, &results[i])
		if a.hist != nil {
			a.hist.Add(historyRow(w, p.Line, &results[i], ver))
		}
	}
	a.matchMu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	if p.Failed == p.Routes {
		return // keep the previous data; nothing new is known
	}
	a.lines[p.Line] = &lineData{updated: p.Done, feed: w.feed, results: results, vehicles: vehicles}
	a.lastPublish, a.lastOK = p.Done, p.Done
}

// Vehicle is one entry of the /v1/lines/{id} response (docs/CONTRACT.md).
type Vehicle struct {
	ID         string       `json:"id"`
	Lat        float64      `json:"lat"`
	Lon        float64      `json:"lon"`
	Bearing    *float64     `json:"bearing"`
	PositionAt int64        `json:"position_at"`
	RouteCode  string       `json:"route_code"`
	Variant    *string      `json:"variant"`
	TripID     *string      `json:"trip_id"`
	TripLabel  *string      `json:"trip_label"`
	DelayS     *int         `json:"delay_s"`
	NextStopID *string      `json:"next_stop_id"`
	Speed      *float64     `json:"speed"`       // m/s along the route
	Path       [][2]float64 `json:"path"`        // [lat, lon] of the route ahead, to the next stop
	PathBeyond [][2]float64 `json:"path_beyond"` // rev 4: on from path's end to the third stop
	PathStops  []int        `json:"path_stops"`  // rev 4: metres along path + path_beyond of each stop
	PathSpeeds []*float64   `json:"path_speeds"` // rev 6: typical m/s per 200 m step ahead
}

func ptr[T any](v T) *T { return &v }

func (a *App) vehicleView(w *world, r *match.Result) Vehicle {
	f := w.feed
	v := Vehicle{ID: r.VehicleID, Lat: r.Lat, Lon: r.Lon, PositionAt: r.Time.Unix(), RouteCode: r.RouteCode}
	if r.Bearing != 0 { // 0 means "unknown" more often than "due north"
		v.Bearing = ptr(r.Bearing)
	}
	if !r.Matched() {
		s := bestShape(f, w.mapper.ShapesFor(r.Line, r.RouteCode), r.Line)
		if s >= 0 {
			v.Variant = ptr(f.Shapes[s].ID)
		}
		v.Speed, v.Path, v.PathBeyond, v.PathStops, v.PathSpeeds = a.motionOf(w, r, s)
		return v
	}
	t := f.Trip(r.Trip)
	if t.Shape >= 0 {
		v.Variant = ptr(f.Shapes[t.Shape].ID)
	}
	v.Speed, v.Path, v.PathBeyond, v.PathStops, v.PathSpeeds = a.motionOf(w, r, t.Shape)
	v.TripID = ptr(t.ID)
	v.TripLabel = ptr(TripLabel(f, t))
	v.DelayS = ptr(r.Delay)
	v.NextStopID = ptr(f.Stops[feed.NextStop(f, r).Stop].ID)
	return v
}

// motionOf records the vehicle's position along its shape and returns its speed, the path
// ahead, its continuation, the stops on them and (matched, with history) typical speeds ahead. A matched trip whose stops fit its shape has its position from the matcher; others are
// projected onto the shape when the pass is clear.
func (a *App) motionOf(w *world, r *match.Result, shape int32) (speed *float64, path, beyond [][2]float64, at []int, steps []*float64) {
	pos := geo.XY(r.Lat, r.Lon)
	if r.Matched() {
		if g := w.matcher.Geometry(w.feed.Trip(r.Trip)); g != nil && g.FromShape {
			if geo.Dist(pos, g.Line.At(r.Along)) > motionMaxOffM {
				return nil, nil, nil, nil, nil
			}
			// Waiting to start: only up to the first stop (usually nothing: it is there).
			stops := g.StopAlong[r.NextIndex:]
			if r.Waiting {
				stops = g.StopAlong[:1]
			}
			speed = a.motion.observe(r.VehicleID, g.Line, r.Along, r.Time)
			path, beyond, at = pathAhead(g.Line, r.Along, speed, stops)
			if tab := a.segs.Load(); shape >= 0 && !r.Waiting && tab.Feed() == worldVersion(w) {
				steps = tab.Steps(w.feed.Shapes[shape].ID, r.Along, g.Line.Length(), segspeed.Slot(r.Time, gtfs.Athens), stepFallback(speed))
			}
			return speed, path, beyond, at, steps
		}
	}
	if shape < 0 {
		return nil, nil, nil, nil, nil
	}
	line := w.matcher.ShapeLine(shape)
	if len(line.XY) < 2 {
		return nil, nil, nil, nil, nil
	}
	s, ok := a.motion.pick(r.VehicleID, line, line.Candidates(pos[0], pos[1], motionMaxOffM, r.Bearing, r.Bearing != 0), r.Time)
	if !ok {
		return nil, nil, nil, nil, nil
	}
	speed = a.motion.observe(r.VehicleID, line, s, r.Time)
	path, _, _ = pathAhead(line, s, speed, nil)
	return speed, path, nil, nil, nil
}

// historyRow is one fix for the history: trip, shape and position along it when matched.
func historyRow(w *world, line string, r *match.Result, ver string) history.Row {
	row := history.Row{FixT: r.Time.Unix(), Line: line, RouteCode: r.RouteCode, Veh: r.VehicleID,
		Lat: r.Lat, Lon: r.Lon, GTFS: ver, SM: -1}
	if !r.Matched() {
		return row
	}
	t := w.feed.Trip(r.Trip)
	row.TripID, row.DelayS = t.ID, ptr(r.Delay)
	if t.Shape >= 0 {
		row.ShapeID = w.feed.Shapes[t.Shape].ID
	}
	if g := w.matcher.Geometry(t); g != nil && g.FromShape {
		row.SM = history.SM(r.Along)
	}
	return row
}

// bestShape picks the shape with the most trips of the line among a route code's shapes.
func bestShape(f *gtfs.Feed, shapes map[int32]bool, line string) int32 {
	if len(shapes) == 0 {
		return -1
	}
	count := map[int32]int{}
	for _, ti := range f.TripsForLine(line) {
		if s := f.Trips[ti].Shape; shapes[s] {
			count[s]++
		}
	}
	best := int32(-1)
	for s, n := range count {
		if s >= 0 && (best < 0 || n > count[best] || (n == count[best] && s < best)) {
			best = s
		}
	}
	return best
}

// TripLabel is "HH:MM FIRST STOP → LAST STOP" (GTFS 24:35 shows as 00:35).
func TripLabel(f *gtfs.Feed, t *gtfs.Trip) string {
	first, last := f.StopTime(t, 0), f.StopTime(t, f.NumStops(t)-1)
	mins := first.Dep / 60
	return fmt.Sprintf("%02d:%02d %s → %s", mins/60%24, mins%60, f.Stops[first.Stop].Name, f.Stops[last.Stop].Name)
}

var latinToGreek = strings.NewReplacer("A", "Α", "B", "Β", "E", "Ε", "Z", "Ζ", "H", "Η", "I", "Ι", "K", "Κ",
	"M", "Μ", "N", "Ν", "O", "Ο", "P", "Ρ", "T", "Τ", "Y", "Υ", "X", "Χ")

// CanonicalLine normalises a line id as users type it (Latin look-alikes for Greek letters).
func CanonicalLine(raw string) string {
	return latinToGreek.Replace(strings.ToUpper(strings.TrimSpace(raw)))
}

// gtfsRT returns the encoded feeds, rebuilt at most every 10 s.
func (a *App) gtfsRT(name string) ([]byte, bool) {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	now := a.now()
	if a.rtFeeds == nil || now.Sub(a.rtBuilt) >= 10*time.Second {
		feeds, err := a.buildRT(now)
		if err != nil {
			a.log.Error("building GTFS-RT", "err", err)
			return nil, false
		}
		a.rtFeeds, a.rtBuilt = feeds, now
	}
	b, ok := a.rtFeeds[name]
	return b, ok
}

func (a *App) buildRT(now time.Time) (map[string][]byte, error) {
	w := a.world()
	var results []match.Result
	a.mu.RLock()
	ids := make([]string, 0, len(a.lines))
	for id := range a.lines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	at := map[string]int{} // vehicle -> index in results: a vehicle that changed lines appears once
	for _, id := range ids {
		d := a.lines[id]
		if w == nil || d.feed != w.feed {
			continue
		}
		for _, r := range d.results {
			if now.Sub(r.Time) > feedMaxAge {
				continue
			}
			if i, ok := at[r.VehicleID]; !ok {
				at[r.VehicleID] = len(results)
				results = append(results, r)
			} else if r.Time.After(results[i].Time) {
				results[i] = r
			}
		}
	}
	a.mu.RUnlock()
	var vp, tu *rt.FeedMessage
	if w == nil {
		vp, tu = feed.Build(&gtfs.Feed{}, nil, now)
	} else {
		vp, tu = feed.Build(w.feed, results, now)
	}
	out := map[string][]byte{}
	for name, msg := range map[string]*rt.FeedMessage{"vehicle_positions": vp, "trip_updates": tu} {
		pb, js, err := feed.Encode(msg)
		if err != nil {
			return nil, err
		}
		out[name+".pb"], out[name+".json"] = pb, js
	}
	return out, nil
}

// Run loads the static feed, then polls and serves until ctx ends.
func (a *App) Run(ctx context.Context) error {
	if err := a.loadInitial(ctx); err != nil {
		return err
	}
	if a.meta.Due() {
		a.meta.Refresh()
	}
	if a.cfg.HistoryMaxBytes > 0 {
		h, err := history.Open(filepath.Join(a.cfg.StateDir, "history"), a.cfg.HistoryMaxBytes, a.cfg.HistoryKeep, a.now, a.log)
		if err != nil {
			return err
		}
		a.matchMu.Lock()
		a.hist = h
		a.matchMu.Unlock()
		defer h.Close()
		go a.dailyStats(ctx)
		go a.typicalSpeeds(ctx)
	}
	srv := &http.Server{Addr: a.cfg.Listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	var msrv *http.Server
	if a.cfg.MetricsListen != "" {
		msrv = &http.Server{Addr: a.cfg.MetricsListen, Handler: a.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- msrv.ListenAndServe() }()
	}
	go a.sched.Run(ctx)
	go a.housekeeping(ctx)
	go a.stasy.Run(ctx)
	a.log.Info("listening", "addr", a.cfg.Listen, "metrics", a.cfg.MetricsListen, "rps", a.pacer.BaseRPS())
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if msrv != nil {
			msrv.Shutdown(sctx)
		}
		return srv.Shutdown(sctx)
	case err := <-errc:
		return err
	}
}

func (a *App) loadInitial(ctx context.Context) error {
	if a.cfg.GTFSZip != "" {
		f, err := gtfs.LoadZip(a.cfg.GTFSZip, gtfs.LoadOptions{})
		if err != nil {
			return err
		}
		a.SetFeed(f, nil)
		return nil
	}
	if f, m, err := a.fetch.LoadLocal(); err != nil {
		a.log.Warn("local snapshot unusable", "err", err)
	} else if f != nil {
		a.SetFeed(f, m)
	}
	for a.world() == nil {
		f, m, err := a.fetch.Check(ctx)
		if err == nil && f != nil {
			a.SetFeed(f, m)
			break
		}
		a.log.Warn("waiting for a GTFS snapshot", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Minute):
		}
	}
	return nil
}

func (a *App) housekeeping(ctx context.Context) {
	minute := time.NewTicker(time.Minute)
	defer minute.Stop()
	check := a.cfg.ReleaseCheck
	if check <= 0 {
		check = 30 * time.Minute
	}
	release := time.NewTicker(check)
	defer release.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-minute.C:
			a.refreshLines()
			if a.meta.Due() {
				a.meta.Refresh()
			}
			a.warnExpiry(false)
			a.forget()
			a.logStats()
		case <-release.C:
			if a.cfg.GTFSZip != "" {
				continue
			}
			f, m, err := a.fetch.Check(ctx)
			switch {
			case err != nil:
				a.log.Warn("snapshot check failed", "err", err)
			case f != nil:
				a.SetFeed(f, m)
			}
		}
	}
}

// logStats writes one line a minute about the polling plan and its cost.
func (a *App) logStats() {
	st := a.sched.Stats()
	a.mu.RLock()
	vehicles, matched := 0, 0
	for _, d := range a.lines {
		if a.now().Sub(d.updated) > feedMaxAge {
			continue
		}
		for i := range d.results {
			vehicles++
			if d.results[i].Matched() {
				matched++
			}
		}
	}
	a.mu.RUnlock()
	reqs := a.client.Requests()
	a.log.Info("stats", "requests", reqs, "requests_last_min", reqs-a.lastRequests, "errors", a.client.Errors(),
		"watched", st.LinesByTier[sched.Watched], "dense", st.LinesByTier[sched.Dense], "other", st.LinesByTier[sched.Other],
		"stretch_other", fmt.Sprintf("%.2f", st.Stretch[sched.Other]), "rps_now", fmt.Sprintf("%.2f", a.pacer.RPS()),
		"vehicles", vehicles, "matched", matched)
	a.lastRequests = reqs
}

// forget drops lines not polled for a while and old matcher state.
func (a *App) forget() {
	now := a.now()
	a.matchMu.Lock()
	if w := a.world(); w != nil {
		w.matcher.Forget(now)
	}
	a.motion.forget(now)
	a.matchMu.Unlock()
	a.mu.Lock()
	for id, d := range a.lines {
		if now.Sub(d.updated) > time.Hour {
			delete(a.lines, id)
		}
	}
	a.mu.Unlock()
}

// queued runs one OASA call as a background request of the scheduler: it takes a request slot
// (budget, backoff, MaxInFlight) like the polls. A caller that gives up leaves it queued; the
// buffered channel lets it finish without anyone reading.
func queued[T any](ctx context.Context, a *App, call func(ctx context.Context) (T, error)) (T, error) {
	type answer struct {
		v   T
		err error
	}
	ch := make(chan answer, 1)
	a.sched.Submit(func(sctx context.Context) error {
		v, err := call(sctx)
		ch <- answer{v, err}
		return err
	})
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// dailyQueued fetches OASA's daily schedule of a line code through queued.
func (a *App) dailyQueued(ctx context.Context, code string) (telematics.DailySchedule, error) {
	return queued(ctx, a, func(sctx context.Context) (telematics.DailySchedule, error) {
		return a.client.DailySchedule(sctx, code)
	})
}
