package rail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

// The Proastiakos sources (docs/research/rail-data.md in the app repo): railway.gov.gr has the
// full day per station, ose.mpass.ltd has "next 5 trains" screens. Both are unofficial, so the
// server keeps the only copy and clients never call them.
const (
	DefaultRailwayURL = "https://railway.gov.gr"
	DefaultMPassURL   = "https://ose.mpass.ltd/api/v1"

	stationsTTL = 24 * time.Hour
	boardTTL    = 10 * time.Minute
	mpassTTL    = time.Minute
	// upstreamRetry is how long a failed fetch is remembered while stale data is served. Without
	// it every request would start a new fetch (the board is 743 KB) at a dead upstream.
	upstreamRetry = time.Minute
)

// Station is one Proastiakos station of the corridor-stations table.
type Station struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Corridors []string `json:"corridors"`
}

// SuburbanDeparture is one next Proastiakos departure of a station.
type SuburbanDeparture struct {
	Train    string  `json:"train"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	T        string  `json:"t"`
	DelayMin *int    `json:"delay_min"`
	Platform *string `json:"platform"`
	Status   string  `json:"status"`
}

// SuburbanDepartures is the body of the departures endpoint.
type SuburbanDepartures struct {
	Station    string              `json:"station"`
	Departures []SuburbanDeparture `json:"departures"`
}

// Suburban is the server-side client of railway.gov.gr, with the ose.mpass boards as fallback.
// Caches: stations 24 h, schedule boards 10 min; a stale entry is served when the upstream fails.
// Only one upstream fetch runs at a time, so a burst of requests costs one board download.
type Suburban struct {
	BaseURL  string
	MPassURL string
	HTTP     *http.Client
	Now      func() time.Time

	fetchMu sync.Mutex // one upstream fetch at a time

	mu           sync.Mutex
	stations     stationIndex
	stationsAt   time.Time
	stationsFail time.Time
	boards       map[string]*boardEntry
	boardFail    time.Time
	mpass        map[string]*mpassEntry
}

type stationIndex struct {
	List    []Station
	members map[string][]string // canonical id -> every id of the station
	primary map[string]string   // any id -> canonical id
}

type boardEntry struct {
	at    time.Time
	board *scheduleBoard
}

// mpassEntry is one screen as fetched: every train it lists, with its instant.
type mpassEntry struct {
	at   time.Time
	name string
	deps []mpassDep
}

// mpassDep is one train of a screen, timestamped: each request filters by its own now.
type mpassDep struct {
	at time.Time
	d  SuburbanDeparture
}

// Stations returns every station with its corridors (24 h cache).
func (s *Suburban) Stations(ctx context.Context) ([]Station, bool) {
	if list, ok := s.cachedStations(); ok {
		return list, true
	}
	if s.backoffStations() {
		return s.staleStations()
	}
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if list, ok := s.cachedStations(); ok {
		return list, true
	}
	if s.backoffStations() {
		return s.staleStations()
	}
	idx, err := s.fetchStations(ctx)
	if err != nil {
		s.mu.Lock()
		s.stationsFail = s.now()
		s.mu.Unlock()
		return s.staleStations()
	}
	s.mu.Lock()
	s.stations, s.stationsAt, s.stationsFail = idx, s.now(), time.Time{}
	s.mu.Unlock()
	return idx.List, true
}

// backoffStations reports whether the last fetch failed less than upstreamRetry ago. Then nothing
// is fetched: without this, a cold cache at a dead upstream fetches on every request.
func (s *Suburban) backoffStations() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.stationsFail.IsZero() && s.now().Sub(s.stationsFail) < upstreamRetry
}

// cachedStations returns the list when it is younger than stationsTTL.
func (s *Suburban) cachedStations() ([]Station, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stations.List == nil || s.now().Sub(s.stationsAt) >= stationsTTL {
		return nil, false
	}
	return s.stations.List, true
}

// staleStations returns the last list whatever its age: an upstream failure does not drop data
// that is a day old anyway.
func (s *Suburban) staleStations() ([]Station, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stations.List, s.stations.List != nil
}

// Departures returns the next n departures of a station, from now, from the day's board. When
// railway.gov.gr fails and nothing is cached, the ose.mpass board of the station is used if its
// screen number is known. ok is false when neither source answered.
func (s *Suburban) Departures(ctx context.Context, stationID string, n int, now time.Time) (*SuburbanDepartures, bool) {
	n = min(max(n, 1), 20)
	// The station table gives the display name and the alias ids. Losing it is cosmetic, so a
	// failed load does not stop the departures.
	_, _ = s.Stations(ctx)
	s.mu.Lock()
	primary, ids, name := s.lookupLocked(stationID)
	s.mu.Unlock()

	date := now.In(gtfs.Athens).Format(time.DateOnly)
	out := &SuburbanDepartures{Station: name, Departures: []SuburbanDeparture{}}
	if b, ok := s.board(ctx, date); ok {
		out.Departures = b.departures(ids, now, n)
		return out, true
	}
	if no, ok := mpassStationNumbers[primary]; ok {
		if deps, mname, ok := s.mpassDepartures(ctx, primary, no, now, n); ok {
			out.Departures = deps
			if name == primary && mname != "" {
				// The stations table did not load: the screen knows the station name.
				out.Station = mname
			}
			return out, true
		}
	}
	return nil, false
}

// lookupLocked resolves the path id to the station's canonical id, all ids it answers to and its
// name, so a request for an alias id still finds the station.
func (s *Suburban) lookupLocked(id string) (primary string, ids []string, name string) {
	primary, ok := s.stations.primary[id]
	if !ok {
		return id, []string{id}, id
	}
	for _, st := range s.stations.List {
		if st.ID == primary {
			name = st.Name
			break
		}
	}
	return primary, s.stations.members[primary], name
}

// board returns the schedule board of a date, cached for boardTTL. On failure the stale board of
// that same date is served; a board of another day would be wrong times, so it is not.
func (s *Suburban) board(ctx context.Context, date string) (*scheduleBoard, bool) {
	if b, ok := s.cachedBoard(date); ok {
		return b, true
	}
	if s.backoffBoard() {
		return s.staleBoard(date)
	}
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if b, ok := s.cachedBoard(date); ok {
		return b, true
	}
	if s.backoffBoard() {
		return s.staleBoard(date)
	}
	b, err := s.fetchBoard(ctx, date)
	if err != nil {
		s.mu.Lock()
		s.boardFail = s.now()
		s.mu.Unlock()
		return s.staleBoard(date)
	}
	s.mu.Lock()
	s.boardFail = time.Time{}
	if s.boards == nil {
		s.boards = map[string]*boardEntry{}
	}
	// Only today's board is worth keeping: one entry per date and the cache would grow.
	for d := range s.boards {
		if d != date {
			delete(s.boards, d)
		}
	}
	s.boards[date] = &boardEntry{at: s.now(), board: b}
	s.mu.Unlock()
	return b, true
}

func (s *Suburban) cachedBoard(date string) (*scheduleBoard, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.boards[date]
	if e == nil || s.now().Sub(e.at) >= boardTTL {
		return nil, false
	}
	return e.board, true
}

// backoffBoard reports whether the last board fetch failed less than upstreamRetry ago. Then no
// new fetch starts: without this, a cold cache at a dead upstream fetches on every request.
func (s *Suburban) backoffBoard() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.boardFail.IsZero() && s.now().Sub(s.boardFail) < upstreamRetry
}

// staleBoard returns the board of a date whatever its age. Another day's times would be wrong,
// so a stale date is no board at all.
func (s *Suburban) staleBoard(date string) (*scheduleBoard, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.boards[date]
	if e == nil {
		return nil, false
	}
	return e.board, true
}

func (s *Suburban) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Suburban) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *Suburban) baseURL() string {
	if s.BaseURL == "" {
		return DefaultRailwayURL
	}
	return strings.TrimSuffix(s.BaseURL, "/")
}

func (s *Suburban) mpassURL() string {
	if s.MPassURL == "" {
		return DefaultMPassURL
	}
	return strings.TrimSuffix(s.MPassURL, "/")
}

// getJSON fetches one upstream JSON document with the polite User-Agent.
func (s *Suburban) getJSON(ctx context.Context, from string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", from, resp.StatusCode)
	}
	b, err := readCapped(resp.Body, maxJSONBody)
	if err != nil {
		return fmt.Errorf("GET %s: %w", from, err)
	}
	return json.Unmarshal(b, v)
}

// fetchStations reads corridor-stations. One physical station can have several ids (Αεροδρόμιο
// has two) and the same id can appear on several corridors, so both are merged.
func (s *Suburban) fetchStations(ctx context.Context) (stationIndex, error) {
	var p struct {
		Corridors []struct {
			ID       string `json:"id"`
			Stations []struct {
				StationID  string   `json:"stationId"`
				StationIDs []string `json:"stationIds"`
				NameGreek  string   `json:"nameGreek"`
				Name       string   `json:"name"`
			} `json:"stations"`
		} `json:"corridors"`
	}
	if err := s.getJSON(ctx, s.baseURL()+"/api/public/corridor-stations", &p); err != nil {
		return stationIndex{}, err
	}
	idx := stationIndex{members: map[string][]string{}, primary: map[string]string{}}
	at := map[string]int{}
	for _, c := range p.Corridors {
		for _, st := range c.Stations {
			if st.StationID == "" {
				continue
			}
			i, ok := at[st.StationID]
			if !ok {
				i = len(idx.List)
				at[st.StationID] = i
				name := strings.TrimSpace(st.NameGreek)
				if name == "" {
					name = strings.TrimSpace(st.Name)
				}
				idx.List = append(idx.List, Station{ID: st.StationID, Name: name, Corridors: []string{}})
			}
			if !contains(idx.List[i].Corridors, c.ID) {
				idx.List[i].Corridors = append(idx.List[i].Corridors, c.ID)
			}
			ids := append([]string{st.StationID}, st.StationIDs...)
			for _, id := range ids {
				if !contains(idx.members[st.StationID], id) {
					idx.members[st.StationID] = append(idx.members[st.StationID], id)
				}
				idx.primary[id] = st.StationID
			}
		}
	}
	if len(idx.List) == 0 {
		return stationIndex{}, fmt.Errorf("corridor-stations: no stations")
	}
	return idx, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// scheduleBoard is the part of /api/public/schedule-board we read: per run, its stops and times.
type scheduleBoard struct {
	Schedules []boardSchedule `json:"schedules"`
}

type boardSchedule struct {
	TrainNumber string      `json:"trainNumber"`
	Status      string      `json:"status"`
	Delay       *int        `json:"delay"`
	Destination boardPlace  `json:"destination"`
	RouteStops  []boardStop `json:"routeStations"`
}

type boardPlace struct {
	NameGreek string `json:"nameGreek"`
}

type boardStop struct {
	StationID          string  `json:"stationId"`
	NameGreek          string  `json:"nameGreek"`
	Role               string  `json:"role"` // origin, stop, destination
	ScheduledArrival   string  `json:"scheduledArrival"`
	ScheduledDeparture string  `json:"scheduledDeparture"`
	Platform           *string `json:"platform"`
	Status             string  `json:"status"`
	Delay              *int    `json:"delay"`
}

func (s *Suburban) fetchBoard(ctx context.Context, date string) (*scheduleBoard, error) {
	from := s.baseURL() + "/api/public/schedule-board?date=" + url.QueryEscape(date)
	b := &scheduleBoard{}
	if err := s.getJSON(ctx, from, b); err != nil {
		return nil, err
	}
	return b, nil
}

// departures lists the next n departures from a station's stops, by scheduled departure time.
func (b *scheduleBoard) departures(ids []string, now time.Time, n int) []SuburbanDeparture {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	type timed struct {
		at time.Time
		d  SuburbanDeparture
	}
	var all []timed
	for i := range b.Schedules {
		sc := &b.Schedules[i]
		if sc.Status == "cancelled" {
			continue
		}
		for j := range sc.RouteStops {
			st := &sc.RouteStops[j]
			if st.Role == "destination" || !want[st.StationID] {
				continue // a terminus has no departure: its time is the arrival of the run
			}
			at, ok := boardTime(st.ScheduledDeparture, st.ScheduledArrival)
			if !ok || at.Before(now) {
				continue
			}
			d := SuburbanDeparture{Train: strings.TrimSpace(sc.TrainNumber), To: sc.Destination.NameGreek,
				T: at.In(gtfs.Athens).Format("15:04"), Platform: st.Platform, Status: sc.Status,
				DelayMin: st.Delay}
			if d.DelayMin == nil {
				d.DelayMin = sc.Delay
			}
			if st.Status != "" {
				d.Status = st.Status
			}
			if d.Status == "" {
				d.Status = "scheduled"
			}
			// The stop before this one: the mpass board shows it as "from" and it is what a
			// passenger sees on a platform display.
			if j > 0 {
				d.From = sc.RouteStops[j-1].NameGreek
			}
			all = append(all, timed{at, d})
			break
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}
		return all[i].d.Train < all[j].d.Train
	})
	out := make([]SuburbanDeparture, 0, min(n, len(all)))
	for _, t := range all[:min(n, len(all))] {
		out = append(out, t.d)
	}
	return out
}

// boardTime parses one of the ISO UTC times of the board (departure, else arrival).
func boardTime(dep, arr string) (time.Time, bool) {
	for _, v := range []string{dep, arr} {
		if v == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// mpassDepartures reads the fallback board: at most five trains, times as " H:MM" in Athens.
// It also returns the station name of the screen, for when the stations table is unavailable.
func (s *Suburban) mpassDepartures(ctx context.Context, id string, no int, now time.Time, n int) ([]SuburbanDeparture, string, bool) {
	if deps, name, ok := s.cachedMPass(id, now, n); ok {
		return deps, name, true
	}
	// One screen download at a time: a burst of fallbacks must not fetch the same screen again.
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if deps, name, ok := s.cachedMPass(id, now, n); ok {
		return deps, name, true
	}
	var p struct {
		Screen *struct {
			Area struct {
				NameEl string `json:"nameEl"`
				Name   string `json:"name"`
			} `json:"area"`
		} `json:"screen"`
		Routes []struct {
			El struct {
				Train      string  `json:"TRENO"`
				FromTo     string  `json:"FROMTO"`
				Arr        *string `json:"DR_TAFIX"`
				Dep        *string `json:"DR_TANA"`
				Forecasted string  `json:"DR_FORECASTED_DATE"`
				Platform   *string `json:"DR_PLATFORM"`
				Delay      *int    `json:"DR_DELAY_TANA"`
			} `json:"routesEl"`
		} `json:"routes"`
	}
	from := s.mpassURL() + "/screens/" + strconv.Itoa(no) + "/data/multilingual"
	if err := s.getJSON(ctx, from, &p); err != nil {
		return nil, "", false
	}
	// The whole screen is kept, past trains included: every request filters by its own now and
	// truncates to its own n, so a caller asking for more does not get the first caller's cut.
	all := make([]mpassDep, 0, len(p.Routes))
	for _, r := range p.Routes {
		e := r.El
		at, ok := mpassTime(e.Dep, e.Arr, e.Forecasted, now)
		if !ok {
			continue
		}
		d := SuburbanDeparture{Train: strings.TrimSpace(e.Train), T: at.In(gtfs.Athens).Format("15:04"),
			DelayMin: e.Delay, Platform: e.Platform, Status: "scheduled"}
		if f, t, ok := strings.Cut(e.FromTo, " - "); ok {
			d.From, d.To = strings.TrimSpace(f), strings.TrimSpace(t)
		}
		all = append(all, mpassDep{at: at, d: d})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	name := ""
	if p.Screen != nil {
		name = strings.TrimSpace(p.Screen.Area.NameEl)
		if name == "" {
			name = strings.TrimSpace(p.Screen.Area.Name)
		}
	}
	s.mu.Lock()
	if s.mpass == nil {
		s.mpass = map[string]*mpassEntry{}
	}
	s.mpass[id] = &mpassEntry{at: s.now(), name: name, deps: all}
	s.mu.Unlock()
	return mpassNext(all, now, n), name, true
}

// cachedMPass returns the cached screen of a station, filtered to the next n trains from now.
func (s *Suburban) cachedMPass(id string, now time.Time, n int) ([]SuburbanDeparture, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.mpass[id]
	if e == nil || s.now().Sub(e.at) >= mpassTTL {
		return nil, "", false
	}
	return mpassNext(e.deps, now, n), e.name, true
}

// mpassNext keeps the trains still to come, at most n of them. deps is ordered by time.
func mpassNext(deps []mpassDep, now time.Time, n int) []SuburbanDeparture {
	out := make([]SuburbanDeparture, 0, min(n, len(deps)))
	for _, d := range deps {
		if len(out) == n {
			break
		}
		if d.at.Before(now) {
			continue
		}
		out = append(out, d.d)
	}
	return out
}

// mpassTime reads the mpass times: departure and arrival are Athens wall-clock strings (" 5:32"),
// the forecasted time is ISO UTC.
func mpassTime(dep, arr *string, forecasted string, now time.Time) (time.Time, bool) {
	day := gtfs.Midnight(now)
	for _, v := range []*string{dep, arr} {
		if v == nil {
			continue
		}
		if t, ok := clockTime(day, *v); ok {
			return t, true
		}
	}
	if forecasted != "" {
		if t, err := time.Parse(time.RFC3339, forecasted); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// clockTime reads " H:MM" as Athens wall-clock time on the calendar day of day. The instant is
// built from the wall clock, so a DST change moves it with the clock; hours past 24:00 roll into
// the next day.
func clockTime(day time.Time, v string) (time.Time, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok {
		return time.Time{}, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 47 || mm < 0 || mm > 59 {
		return time.Time{}, false
	}
	d := day.In(gtfs.Athens)
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, gtfs.Athens), true
}

// mpassStationNumbers maps a railway.gov.gr stationId to the station number of its ose.mpass
// screen, by station name (research file, section 1). Stations that are not listed have no
// fallback.
var mpassStationNumbers = map[string]int{
	"6928c3c150ab5bc72b39cee6": 2,   // Πειραιάς
	"6928c3c050ab5bc72b39cd93": 462, // Λεύκα
	"6928c3c050ab5bc72b39cd95": 422, // Άγ. Ιωάννης Ρέντης / Ρέντης
	"6928c3c050ab5bc72b39cd98": 25,  // Ταύρος
	"6928c3c150ab5bc72b39d041": 24,  // Ρουφ
	"6928c3c150ab5bc72b39d076": 1,   // Αθήνα
	"6928c3c150ab5bc72b39cee5": 428, // Άγιοι Ανάργυροι
	"6928c3c150ab5bc72b39cee3": 494, // Πύργος Βασιλίσσης
	"6928c3c150ab5bc72b39cee4": 253, // Κάτω Αχαρναί
	"6928c3c150ab5bc72b39cf84": 19,  // Άνω Λιόσια
	"6928c3c150ab5bc72b39d077": 27,  // ΣΚΑ
	"6928c3c150ab5bc72b39d067": 21,  // Μεταμόρφωση
	"6928c3c150ab5bc72b39cfc7": 16,  // Ηράκλειο
	"6928c3c150ab5bc72b39d007": 18,  // Νερατζιώτισσα
	"6928c3c050ab5bc72b39cdad": 15,  // Κηφισίας
	"6928c3c150ab5bc72b39d06c": 481, // Πεντέλης
	"6928c3c050ab5bc72b39cdca": 17,  // Δουκίσσης Πλακεντίας
	"6928c3c150ab5bc72b39cff3": 14,  // Παλλήνη
	"6928c3c150ab5bc72b39cec9": 13,  // Παιανία-Κάντζα
	"6928c3c150ab5bc72b39d006": 458, // Κορωπί
	"6928c3c150ab5bc72b39cec7": 28,  // Αεροδρόμιο
	"6928c3c150ab5bc72b39cee2": 194, // Ζεφύρι
	"6928c3c150ab5bc72b39cf21": 437, // Ασπρόπυργος
	"6928c3c150ab5bc72b39cfe6": 466, // Μαγούλα
	"6928c3c150ab5bc72b39d074": 469, // Νέα Πέραμος
	"6928c3c150ab5bc72b39cf15": 12,  // Μέγαρα
	"6928c3c150ab5bc72b39cf2c": 456, // Κινέτα
	"6928c3c150ab5bc72b39cf37": 421, // Άγιοι Θεόδωροι
	"6928c3c150ab5bc72b39cf17": 6,   // Κόρινθος
	"6928c3c050ab5bc72b39cdd6": 451, // Ζευγολατιό
	"6928c3c150ab5bc72b39cf12": 20,  // Κιάτο
	"6928c3c150ab5bc72b39d06b": 91,  // Αχαρναί
	"6928c3c150ab5bc72b39cedf": 149, // Δεκέλεια
	"6928c3c050ab5bc72b39cde5": 83,  // Άγιος Στέφανος
	"6928c3c150ab5bc72b39d073": 89,  // Αφίδνες
	"6928c3c050ab5bc72b39cd9b": 555, // Σφενδάλη
	"6928c3c150ab5bc72b39cf13": 87,  // Αυλώνας
	"6928c3c150ab5bc72b39cf19": 681, // Άγιος Θωμάς
	"6928c3c050ab5bc72b39cda2": 11,  // Οινόη
	"6928c3c150ab5bc72b39cf18": 412, // Οινόφυτα
	"6928c3c150ab5bc72b39cfe5": 554, // Δήλεσι
	"6928c3c150ab5bc72b39cf4f": 605, // Άγιος Γεώργιος
	"6928c3c150ab5bc72b39cfe4": 228, // Καλοχώρι-Παντείχι
	"6928c3c150ab5bc72b39cf14": 86,  // Αυλίδα
	"6928c3c150ab5bc72b39cf16": 597, // Χαλκίδα
}

// SuburbanStop is one stop of a run with its scheduled time: the arrival, or the departure where
// the board gives no arrival (the origin).
type SuburbanStop struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	T        string  `json:"t"`
	At       int64   `json:"at"`
	Platform *string `json:"platform"`
	DelayMin *int    `json:"delay_min"`
}

// SuburbanRun is one train's run of today with every stop.
type SuburbanRun struct {
	Train    string         `json:"train"`
	From     string         `json:"from"`
	To       string         `json:"to"`
	Status   string         `json:"status"`
	DelayMin *int           `json:"delay_min"`
	Stops    []SuburbanStop `json:"stops"`
}

// Train returns today's run of a train number from the day's board. found is false for a number
// with no run today (or only cancelled ones); ok is false when the board is unavailable.
func (s *Suburban) Train(ctx context.Context, no string, now time.Time) (run *SuburbanRun, found, ok bool) {
	b, ok := s.board(ctx, now.In(gtfs.Athens).Format(time.DateOnly))
	if !ok {
		return nil, false, false
	}
	sc := b.run(strings.TrimSpace(no), now)
	if sc == nil {
		return nil, false, true
	}
	out := &SuburbanRun{Train: strings.TrimSpace(sc.TrainNumber), To: sc.Destination.NameGreek, Status: sc.Status,
		DelayMin: sc.Delay, Stops: make([]SuburbanStop, 0, len(sc.RouteStops))}
	if len(sc.RouteStops) > 0 {
		out.From = sc.RouteStops[0].NameGreek
	}
	for _, st := range sc.RouteStops {
		at, ok := boardTime(st.ScheduledArrival, st.ScheduledDeparture)
		if !ok {
			continue
		}
		out.Stops = append(out.Stops, SuburbanStop{ID: st.StationID, Name: st.NameGreek, T: at.In(gtfs.Athens).Format("15:04"),
			At: at.Unix(), Platform: st.Platform, DelayMin: st.Delay})
	}
	return out, true, true
}

// run picks the run of a train number for now: the one under way, else the next, else the last
// of the day. Cancelled runs are left out.
func (b *scheduleBoard) run(no string, now time.Time) *boardSchedule {
	var next, last *boardSchedule
	var nextAt, lastAt time.Time
	for i := range b.Schedules {
		sc := &b.Schedules[i]
		if sc.Status == "cancelled" || strings.TrimSpace(sc.TrainNumber) != no || len(sc.RouteStops) == 0 {
			continue
		}
		first, ok1 := boardTime(sc.RouteStops[0].ScheduledDeparture, sc.RouteStops[0].ScheduledArrival)
		end := sc.RouteStops[len(sc.RouteStops)-1]
		final, ok2 := boardTime(end.ScheduledArrival, end.ScheduledDeparture)
		if !ok1 || !ok2 {
			continue
		}
		if !now.Before(first) && !now.After(final) {
			return sc
		}
		if first.After(now) && (next == nil || first.Before(nextAt)) {
			next, nextAt = sc, first
		}
		if last == nil || final.After(lastAt) {
			last, lastAt = sc, final
		}
	}
	if next != nil {
		return next
	}
	return last
}
