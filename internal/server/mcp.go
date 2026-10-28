package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angelospk/athens-transit-rt/internal/rail"
	"github.com/angelospk/athens-transit-rt/internal/stats"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

const (
	mcpInstructions = "Athens OASA buses and trolleys live (positions refresh every 30-60 s), today's timetables, " +
		"OASA's own stop predictions, daily network statistics, suburban rail (live) and metro/tram " +
		"(STASY timetable). Names are Greek; all times are Europe/Athens."
	stopArrivalsTTL     = 30 * time.Second
	stopArrivalsTimeout = 20 * time.Second // the longest a shared OASA call waits in the queue
	slowestMinTimeS     = 15 * 3600        // a line needs this much measured time to enter the slowest list
	mostMissedMinTrp    = 20               // measured trips a line needs to enter the most-missed list
)

type arrivalsEntry struct {
	at   time.Time
	list []telematics.Arrival
}

// arrivalsCall is an OASA call in flight for a stop: requests for the same stop wait for it.
type arrivalsCall struct {
	done chan struct{}
	at   time.Time
	list []telematics.Arrival
	err  error
}

// mcpBodyTimeout bounds how long a client may take to send its request body.
var mcpBodyTimeout = 15 * time.Second

// MCPServer is the read-only Model Context Protocol server (tools over the app's own API).
func (a *App) MCPServer() *mcp.Server {
	a.mcpOnce.Do(func() {
		s := mcp.NewServer(&mcp.Implementation{Name: "athens-transit", Version: "1"},
			&mcp.ServerOptions{Instructions: mcpInstructions})
		addTool(s, "search_lines", "Find bus and trolley lines by number or name (accents and case ignored). "+
			"Returns up to 20 {line, name}.", a.toolSearchLines)
		addTool(s, "line_now", "Live buses of a line: position, delay, next stop (id and name), and when the data "+
			"updates next. Returns the line's JSON.", a.toolLineNow)
		addTool(s, "line_timetable", "Departures of a line for a day (default today) per direction, from the GTFS "+
			"timetable or OASA's daily schedule. Returns the schedule JSON.", a.toolLineTimetable)
		addTool(s, "find_stops", "Find bus stops by name (accents and case ignored). Returns up to 15 "+
			"{id, name, lat, lon}.", a.toolFindStops)
		addTool(s, "stop_arrivals", "OASA's own predictions for a stop: the next buses with minutes to arrival. "+
			"Returns [{line, minutes, vehicle}] in OASA order.", a.toolStopArrivals)
		addTool(s, "network_day", "Network statistics of a day (default the latest): vehicles, fixes, average speed, "+
			"trips planned, run and missed, the slowest lines and the lines that missed most trips.", a.toolNetworkDay)
		addTool(s, "suburban_departures", "Next Proastiakos (suburban rail) trains of a station, live, with delays. "+
			"Returns {station, departures}.", a.toolSuburban)
		addTool(s, "metro_departures", "Next metro and tram departures at a station from the STASY timetable. "+
			"Returns the departures JSON.", a.toolMetro)
		a.mcpSrv = s
	})
	return a.mcpSrv
}

func (a *App) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return a.MCPServer() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true,
			// The server sits behind a reverse proxy on 127.0.0.1, so every request arrives on
			// loopback with the public Host. The DNS rebinding guard would refuse them all; it
			// protects private local servers, and this one serves public read-only data.
			DisableLocalhostProtection: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if rc.SetReadDeadline(time.Now().Add(mcpBodyTimeout)) == nil { // unsupported in tests with a recorder
			r.Body = &bodyDeadline{ReadCloser: r.Body, rc: rc}
		}
		h.ServeHTTP(w, r)
	})
}

// bodyDeadline lifts the read deadline once the body is read to its end, so the connection's
// later reads (the next request, the close watch) are not cut. A body that never arrives keeps
// the deadline: the read fails and the server closes the connection.
type bodyDeadline struct {
	io.ReadCloser
	rc *http.ResponseController
}

func (b *bodyDeadline) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// addTool registers a tool whose handler returns the tool result; Go errors are never used for
// problems the caller can fix.
func addTool[In any](s *mcp.Server, name, desc string, fn func(ctx context.Context, in In) *mcp.CallToolResult) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			return fn(ctx, in), nil, nil
		})
}

func textResult(text string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isErr}
}

func okJSON(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return textResult("internal_error", true)
	}
	return textResult(string(b), false)
}

func toolErr(format string, args ...any) *mcp.CallToolResult {
	return textResult(fmt.Sprintf(format, args...), true)
}

// apiGet runs a GET against the app's own REST handler.
func (a *App) apiGet(ctx context.Context, target string) (int, []byte) {
	a.apiOnce.Do(func() { a.apiHandler = a.Handler() })
	rec := httptest.NewRecorder()
	a.apiHandler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil))
	return rec.Code, rec.Body.Bytes()
}

// errCode reads {"error": code} from an API error body.
func errCode(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return "error"
	}
	return e.Error
}

// foldGreek lowercases, drops Greek accents, maps final sigma to sigma and trims.
func foldGreek(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case 'ά', 'Ά':
			return 'α'
		case 'έ', 'Έ':
			return 'ε'
		case 'ή', 'Ή':
			return 'η'
		case 'ί', 'Ί', 'ϊ', 'Ϊ', 'ΐ':
			return 'ι'
		case 'ό', 'Ό':
			return 'ο'
		case 'ύ', 'Ύ', 'ϋ', 'Ϋ', 'ΰ':
			return 'υ'
		case 'ώ', 'Ώ':
			return 'ω'
		case 'ς':
			return 'σ'
		}
		return []rune(strings.ToLower(string(r)))[0]
	}, s))
}

type queryIn struct {
	Query string `json:"query" jsonschema:"text to look for"`
}

type lineIn struct {
	Line string `json:"line" jsonschema:"line number as OASA shows it, for example 040 or Α1"`
}

type timetableIn struct {
	Line string `json:"line" jsonschema:"line number, for example 040"`
	Date string `json:"date,omitempty" jsonschema:"day as YYYY-MM-DD; default today"`
}

type stopIn struct {
	StopID string `json:"stop_id" jsonschema:"stop id from find_stops"`
}

type dateIn struct {
	Date string `json:"date,omitempty" jsonschema:"day as YYYY-MM-DD; default the latest day with statistics"`
}

type stationIn struct {
	Station string `json:"station" jsonschema:"station name, Greek, for example Σύνταγμα"`
}

func (a *App) toolSearchLines(_ context.Context, in queryIn) *mcp.CallToolResult {
	q := foldGreek(in.Query)
	if q == "" {
		return toolErr("missing_query")
	}
	type hit struct {
		Line string `json:"line"`
		Name string `json:"name"`
	}
	out := []hit{}
	seen := map[string]bool{}
	for _, l := range a.meta.Snapshot().Lines {
		if seen[l.LineID] || !(strings.Contains(foldGreek(l.LineID), q) || strings.Contains(foldGreek(l.LineDescr), q)) {
			continue
		}
		seen[l.LineID] = true
		out = append(out, hit{l.LineID, l.LineDescr})
		if len(out) == 20 {
			break
		}
	}
	return okJSON(out)
}

func (a *App) toolLineNow(ctx context.Context, in lineIn) *mcp.CallToolResult {
	if strings.TrimSpace(in.Line) == "" {
		return toolErr("missing_line")
	}
	code, body := a.apiGet(ctx, "/v1/lines/"+url.PathEscape(in.Line))
	switch {
	case code == http.StatusNotFound:
		return toolErr("unknown_line: %s", in.Line)
	case code != http.StatusOK:
		return toolErr("%s", errCode(body))
	}
	var resp map[string]json.RawMessage
	var vehicles []map[string]json.RawMessage
	if json.Unmarshal(body, &resp) != nil || json.Unmarshal(resp["vehicles"], &vehicles) != nil {
		return textResult(string(body), false)
	}
	if w := a.world(); w != nil {
		for _, v := range vehicles {
			var id string
			if json.Unmarshal(v["next_stop_id"], &id) != nil || id == "" {
				continue
			}
			if i := w.feed.StopIndex(id); i >= 0 {
				v["next_stop_name"], _ = json.Marshal(w.feed.Stops[i].Name)
			}
		}
	}
	resp["vehicles"], _ = json.Marshal(vehicles)
	return okJSON(resp)
}

func (a *App) toolLineTimetable(ctx context.Context, in timetableIn) *mcp.CallToolResult {
	if strings.TrimSpace(in.Line) == "" {
		return toolErr("missing_line")
	}
	target := "/v1/lines/" + url.PathEscape(in.Line) + "/schedule"
	if in.Date != "" {
		target += "?date=" + url.QueryEscape(in.Date)
	}
	code, body := a.apiGet(ctx, target)
	switch code {
	case http.StatusOK:
		return textResult(string(body), false)
	case http.StatusNotFound:
		return toolErr("unknown_line: %s", in.Line)
	case http.StatusBadRequest:
		return toolErr("bad_date: %s", in.Date)
	}
	return toolErr("%s", errCode(body))
}

func (a *App) toolFindStops(_ context.Context, in queryIn) *mcp.CallToolResult {
	q := foldGreek(in.Query)
	if q == "" {
		return toolErr("missing_query")
	}
	w := a.world()
	if w == nil {
		return toolErr("warming_up")
	}
	type hit struct {
		ID   string  `json:"id"`
		Name string  `json:"name"`
		Lat  float64 `json:"lat"`
		Lon  float64 `json:"lon"`
	}
	out := []hit{}
	for _, s := range w.feed.Stops {
		if strings.Contains(foldGreek(s.Name), q) {
			out = append(out, hit{s.ID, s.Name, s.Lat, s.Lon})
			if len(out) == 15 {
				break
			}
		}
	}
	return okJSON(out)
}

func (a *App) toolStopArrivals(ctx context.Context, in stopIn) *mcp.CallToolResult {
	id := strings.TrimSpace(in.StopID)
	if id == "" {
		return toolErr("missing_stop")
	}
	w := a.world()
	if w == nil {
		return toolErr("warming_up")
	}
	if w.feed.StopIndex(id) < 0 {
		return toolErr("unknown_stop: %s", id)
	}
	e, err := a.arrivals(ctx, id)
	if errors.Is(err, errQueueFull) {
		return toolErr("busy: try again in a minute")
	}
	if err != nil {
		return toolErr("upstream_unavailable")
	}
	lineOf := a.routeLines()
	type row struct {
		Line    string `json:"line"`
		Minutes int    `json:"minutes"`
		Vehicle string `json:"vehicle"`
	}
	out := make([]row, 0, len(e.list))
	for _, ar := range e.list {
		out = append(out, row{lineOf[ar.RouteCode], ar.Minutes, ar.VehCode})
	}
	return okJSON(out)
}

// routeLines maps each OASA route code to its line number, from the line metadata.
func (a *App) routeLines() map[string]string {
	lineOf := map[string]string{}
	for line, refs := range a.meta.LineRoutes() {
		for _, r := range refs {
			lineOf[r.Code] = line
		}
	}
	return lineOf
}

// arrivals returns OASA's predictions for a stop and when they were fetched: cached for
// stopArrivalsTTL, one call per stop at a time (the others wait for it). Errors are not cached.
func (a *App) arrivals(ctx context.Context, id string) (arrivalsEntry, error) {
	a.arrMu.Lock()
	if e, ok := a.arrCache[id]; ok && a.now().Sub(e.at) < stopArrivalsTTL {
		a.arrMu.Unlock()
		return e, nil
	}
	if c, busy := a.arrCalls[id]; busy {
		a.arrMu.Unlock()
		select {
		case <-c.done:
			return arrivalsEntry{c.at, c.list}, c.err
		case <-ctx.Done():
			return arrivalsEntry{}, ctx.Err()
		}
	}
	c := &arrivalsCall{done: make(chan struct{})}
	if a.arrCalls == nil {
		a.arrCalls = map[string]*arrivalsCall{}
	}
	a.arrCalls[id] = c
	a.arrMu.Unlock()

	// The call outlives the request that started it: the others waiting must not fail because
	// that one client left.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopArrivalsTimeout)
	c.list, c.err = a.stopArrivals(fctx, id)
	cancel()
	c.at = a.now()

	a.arrMu.Lock()
	if c.err == nil {
		if a.arrCache == nil {
			a.arrCache = map[string]arrivalsEntry{}
		}
		a.arrCache[id] = arrivalsEntry{c.at, c.list}
	}
	delete(a.arrCalls, id)
	a.arrMu.Unlock()
	close(c.done)
	return arrivalsEntry{c.at, c.list}, c.err
}

// cachedArrivals returns the last good predictions for a stop if they are at most maxAge old,
// whatever the TTL: the fallback when OASA or the request queue cannot answer now.
func (a *App) cachedArrivals(id string, maxAge time.Duration) (arrivalsEntry, bool) {
	a.arrMu.Lock()
	defer a.arrMu.Unlock()
	e, ok := a.arrCache[id]
	if !ok || a.now().Sub(e.at) > maxAge {
		return arrivalsEntry{}, false
	}
	return e, true
}

// stopArrivalsQueued asks OASA for a stop's predictions as a background request of the scheduler.
func (a *App) stopArrivalsQueued(ctx context.Context, stopCode string) ([]telematics.Arrival, error) {
	return queued(ctx, a, func(sctx context.Context) ([]telematics.Arrival, error) {
		return a.client.StopArrivals(sctx, stopCode)
	})
}

func (a *App) toolNetworkDay(ctx context.Context, in dateIn) *mcp.CallToolResult {
	date := strings.TrimSpace(in.Date)
	if date == "" {
		code, body := a.apiGet(ctx, "/v1/stats")
		var idx stats.Index
		if code != http.StatusOK || json.Unmarshal(body, &idx) != nil || idx.Latest == "" {
			return toolErr("no_stats")
		}
		date = idx.Latest
	}
	code, body := a.apiGet(ctx, "/v1/stats/days/"+url.PathEscape(date))
	switch code {
	case http.StatusOK:
	case http.StatusBadRequest:
		return toolErr("bad_date: %s", date)
	default:
		return toolErr("no_stats: %s", date)
	}
	var d stats.Day
	if json.Unmarshal(body, &d) != nil {
		return toolErr("no_stats: %s", date)
	}
	return okJSON(summarizeDay(d))
}

type kmhRow struct {
	Line string  `json:"line"`
	Kmh  float64 `json:"kmh"`
}

type missedRow struct {
	Line     string `json:"line"`
	Missed   int    `json:"missed"`
	Measured int    `json:"measured"`
}

type daySummary struct {
	Date     string   `json:"date"`
	Vehicles int      `json:"vehicles"`
	Fixes    int      `json:"fixes"`
	Kmh      *float64 `json:"kmh"`
	Trips    *struct {
		Planned int `json:"planned"`
		Run     int `json:"run"`
		Missed  int `json:"missed"`
	} `json:"trips,omitempty"`
	Slowest     []kmhRow    `json:"slowest"`
	MostMissed  []missedRow `json:"most_missed"`
	GTFSExpired bool        `json:"gtfs_expired,omitempty"`
}

func kmh(distM, timeS int64) float64 { return math.Round(float64(distM)/float64(timeS)*3.6*10) / 10 }

func summarizeDay(d stats.Day) daySummary {
	s := daySummary{Date: d.Date, Vehicles: d.Vehicles, Fixes: d.Fixes, Slowest: []kmhRow{}, MostMissed: []missedRow{}}
	var dist, secs int64
	for _, h := range d.Hours {
		dist += h.DistM
		secs += h.TimeS
	}
	if secs > 0 {
		v := kmh(dist, secs)
		s.Kmh = &v
	}
	for _, l := range d.ByLine {
		if l.TimeS >= slowestMinTimeS {
			s.Slowest = append(s.Slowest, kmhRow{l.Line, kmh(l.DistM, l.TimeS)})
		}
	}
	sort.SliceStable(s.Slowest, func(i, j int) bool { return s.Slowest[i].Kmh < s.Slowest[j].Kmh })
	if len(s.Slowest) > 10 {
		s.Slowest = s.Slowest[:10]
	}
	if t := d.Trips; t != nil {
		s.Trips = &struct {
			Planned int `json:"planned"`
			Run     int `json:"run"`
			Missed  int `json:"missed"`
		}{t.Planned, t.Run, t.Missed}
		s.GTFSExpired = t.GTFSExpired
		for _, l := range t.ByLine {
			measured := l.Planned - l.Unmeasured
			// With an expired timetable a line never seen may not run any more: not ranked then.
			if t.GTFSExpired && l.Run+l.Partial == 0 {
				continue
			}
			if measured >= mostMissedMinTrp {
				s.MostMissed = append(s.MostMissed, missedRow{l.Line, l.Missed, measured})
			}
		}
		sort.SliceStable(s.MostMissed, func(i, j int) bool {
			a, b := s.MostMissed[i], s.MostMissed[j]
			return float64(a.Missed)/float64(a.Measured) > float64(b.Missed)/float64(b.Measured)
		})
		if len(s.MostMissed) > 10 {
			s.MostMissed = s.MostMissed[:10]
		}
	}
	return s
}

func (a *App) toolSuburban(ctx context.Context, in stationIn) *mcp.CallToolResult {
	q := foldGreek(in.Station)
	if q == "" {
		return toolErr("missing_station")
	}
	code, body := a.apiGet(ctx, "/v1/rail/suburban/stations")
	var list struct {
		Stations []rail.Station `json:"stations"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		return toolErr("upstream_unavailable")
	}
	var exact, part []rail.Station
	for _, s := range list.Stations {
		switch n := foldGreek(s.Name); {
		case n == q:
			exact = append(exact, s)
		case strings.Contains(n, q):
			part = append(part, s)
		}
	}
	var pick rail.Station
	switch {
	case len(exact) > 0:
		pick = exact[0]
	case len(part) == 1:
		pick = part[0]
	default:
		names := make([]string, 0, 10)
		for _, s := range part[:min(len(part), 10)] {
			names = append(names, s.Name)
		}
		if len(names) == 0 {
			return toolErr("unknown_station: %s", in.Station)
		}
		return toolErr("ambiguous_station: %s; candidates: %s", in.Station, strings.Join(names, ", "))
	}
	code, body = a.apiGet(ctx, "/v1/rail/suburban/stations/"+url.PathEscape(pick.ID)+"/departures")
	if code != http.StatusOK {
		return toolErr("upstream_unavailable")
	}
	var dep rail.SuburbanDepartures
	if json.Unmarshal(body, &dep) != nil {
		return toolErr("upstream_unavailable")
	}
	dep.Station = pick.Name // the board answers with the station id
	return okJSON(dep)
}

func (a *App) toolMetro(ctx context.Context, in stationIn) *mcp.CallToolResult {
	stop := strings.TrimSpace(in.Station)
	if stop == "" {
		return toolErr("missing_station")
	}
	code, body := a.apiGet(ctx, "/v1/rail/stasy/departures?n=6&stop="+url.QueryEscape(stop))
	if code != http.StatusOK {
		return toolErr("%s", errCode(body))
	}
	return textResult(string(body), false)
}
