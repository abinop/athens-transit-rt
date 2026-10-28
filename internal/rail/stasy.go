package rail

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

const (
	// DefaultSTASYCatalog is the Growthfund CKAN dataset "Δρομολόγια Σταθερών Συγκοινωνιών
	// (ΜΕΤΡΟ, ΗΣΑΠ, ΤΡΑΜ)": the STASY timetable as loose .txt files of the GTFS format.
	DefaultSTASYCatalog = "https://catalog.growthfund.gr/api/3/action/package_show?id=dromologia-statheron-sygkinonion"

	stasyCheck = 24 * time.Hour      // the catalogue is checked once a day
	stasyRetry = 10 * time.Minute    // after a failed download
	stasyMaxN  = 20                  // a client cannot ask for more departures per line
	stasyFile  = "metadata_modified" // remembers the catalogue version of the files on disk
)

// STASYRoute is one line of the metro/tram timetable (M1-M3, T6, T7).
type STASYRoute struct {
	ID, Short, Color string
}

// STASYStop is one stop. Feeds list one entry per platform; a station name covers many.
type STASYStop struct {
	ID, Name string
	Lat, Lon float64
}

// STASYDeparture is one next departure at a stop.
type STASYDeparture struct {
	Line       string `json:"line"`
	RouteColor string `json:"route_color"`
	Headsign   string `json:"headsign"`
	T          string `json:"t"`
	InS        int32  `json:"in_s"`
}

// STASYDepartures is the body of the departures endpoint.
type STASYDepartures struct {
	Stop       string           `json:"stop"`
	Indicative bool             `json:"indicative"`
	Departures []STASYDeparture `json:"departures"`
}

// STASY is the metro/tram timetable, parsed from the STASY GTFS text files.
type STASY struct {
	Stops  []STASYStop
	Routes []STASYRoute

	stopIdx    map[string]int32
	byName     map[string][]int32 // folded stop name -> stop indices
	trips      []stasyTrip
	tripIdx    map[string]int32
	tripStops  [][]stasyStopTime    // per trip, in stop_sequence order
	byStop     map[int32][]stasyRow // stop index -> its stop_times rows
	services   []stasyService
	serviceIdx map[string]int32
	start, end int32 // yyyymmdd of the timetable's validity, 0 when unknown
}

type stasyTrip struct {
	ID       string
	Route    int32
	Service  int32
	Headsign string
}

// stasyStopTime is one stop of a trip (the trip's own list; byStop is the same rows by stop).
type stasyStopTime struct {
	Stop int32
	A, T int32 // arrival and departure, seconds after midnight of the service day
	seq  int32
}

type stasyRow struct {
	Trip int32
	Dep  int32 // seconds after midnight of the service day (>= 24 h past midnight)
}

// stasyService is a service of the feed. STASY publishes calendar_dates only (one service per
// date); calendar.txt is read too when a future feed has it.
type stasyService struct {
	days        [7]bool
	start, end  int32
	hasCalendar bool
	added       map[int32]bool
	removed     map[int32]bool
}

// NumTrips is the number of trips the timetable holds.
func (s *STASY) NumTrips() int { return len(s.trips) }

// Departures returns the next n departures per (line, headsign) at every stop whose folded
// name equals that of stop, from now on. ok is false when no stop has that name.
func (s *STASY) Departures(stop string, n int, now time.Time) (*STASYDepartures, bool) {
	ids := s.byName[Fold(stop)]
	if len(ids) == 0 {
		return nil, false
	}
	n = min(max(n, 1), stasyMaxN)
	local := now.In(gtfs.Athens)

	type group struct {
		route    int32
		headsign string
	}
	type dep struct {
		at  int32 // seconds after midnight of its own service day, as GTFS writes it
		inS int32 // seconds from now to that departure
	}
	next := map[group][]dep{}
	indicative := false
	// Two service days: a "24:30" departure of yesterday is 00:30 today, and the wall clock of
	// today cannot tell it apart from a departure of today. The instants come from gtfs.ServiceDay,
	// so the elapsed seconds stay right across a DST change.
	for i, d := range []struct {
		noon   time.Time // the calendar date, at an hour that always exists
		minDep int32     // yesterday counts only what runs past midnight
	}{
		{time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, gtfs.Athens), 0},
		{time.Date(local.Year(), local.Month(), local.Day()-1, 12, 0, 0, 0, gtfs.Athens), 24 * 3600},
	} {
		date, ind := s.timetableDate(d.noon)
		secs := int32(now.Sub(gtfs.ServiceDay(d.noon)) / time.Second)
		used := false
		for _, si := range ids {
			for _, r := range s.byStop[si] {
				t := &s.trips[r.Trip]
				if r.Dep < d.minDep || r.Dep < secs || !s.active(t.Service, date) {
					continue
				}
				used = true
				g := group{t.Route, t.Headsign}
				next[g] = append(next[g], dep{at: r.Dep, inS: r.Dep - secs})
			}
		}
		// Today's date decides the flag. Yesterday's is outside the feed more often than not, so
		// it marks the timetable indicative only when it lists a departure of its own.
		if i == 0 || used {
			indicative = indicative || ind
		}
	}
	out := &STASYDepartures{Stop: s.stopName(ids), Indicative: indicative, Departures: []STASYDeparture{}}
	for g, deps := range next {
		sort.Slice(deps, func(i, j int) bool { return deps[i].inS < deps[j].inS })
		for _, d := range deps[:min(n, len(deps))] {
			out.Departures = append(out.Departures, STASYDeparture{
				Line: s.line(g.route), RouteColor: s.color(g.route), Headsign: g.headsign,
				T: HHMM(d.at), InS: d.inS,
			})
		}
	}
	sort.Slice(out.Departures, func(i, j int) bool {
		a, b := out.Departures[i], out.Departures[j]
		if a.InS != b.InS {
			return a.InS < b.InS
		}
		return a.Line < b.Line
	})
	return out, true
}

func (s *STASY) stopName(ids []int32) string {
	// The first stop of the name in file order: "ΣΥΝΤΑΓΜΑ" for every platform of it.
	return s.Stops[ids[0]].Name
}

func (s *STASY) line(route int32) string { return s.Routes[route].Short }

// color is the feed's route_color, or the published line colour when the feed has none (it has
// none: the October 2026 files leave route_color empty).
func (s *STASY) color(route int32) string {
	if c := strings.TrimSpace(s.Routes[route].Color); c != "" {
		if !strings.HasPrefix(c, "#") {
			c = "#" + c
		}
		return c
	}
	return stasyColors[s.Routes[route].Short]
}

var stasyColors = map[string]string{"M1": "#00a651", "M2": "#e30613", "M3": "#0072bc",
	"T6": "#f39200", "T7": "#f39200"}

// timetableDate returns the date whose timetable to read: the day itself, or the same weekday
// whole weeks inside the feed when the day is outside its validity (STASY lets the feed expire
// before publishing the next). indicative reports the substitution.
func (s *STASY) timetableDate(day time.Time) (int32, bool) {
	date := yyyymmdd(day)
	if s.start == 0 || s.end == 0 || (s.start <= date && date <= s.end) {
		return date, false
	}
	d := day
	for yyyymmdd(d) > s.end {
		d = d.AddDate(0, 0, -7)
	}
	for yyyymmdd(d) < s.start {
		d = d.AddDate(0, 0, 7)
	}
	return yyyymmdd(d), true
}

// active reports whether a service runs on a date (yyyymmdd).
func (s *STASY) active(svc int32, date int32) bool {
	if svc < 0 || int(svc) >= len(s.services) {
		return false
	}
	v := &s.services[svc]
	if v.added[date] {
		return true
	}
	if v.removed[date] {
		return false
	}
	if !v.hasCalendar || date < v.start || date > v.end {
		return false
	}
	d := time.Date(int(date/10000), time.Month(date/100%100), int(date%100), 12, 0, 0, 0, gtfs.Athens)
	return v.days[(int(d.Weekday())+6)%7]
}

func yyyymmdd(d time.Time) int32 {
	d = d.In(gtfs.Athens)
	return int32(d.Year()*10000 + int(d.Month())*100 + d.Day())
}

// ParseSTASY reads the STASY GTFS text files from dir (stops, routes, trips, stop_times,
// calendar_dates, calendar when present). It streams stop_times.txt, which is ~62 MB.
func ParseSTASY(dir string) (*STASY, error) {
	s := &STASY{stopIdx: map[string]int32{}, byName: map[string][]int32{}, byStop: map[int32][]stasyRow{},
		serviceIdx: map[string]int32{}}
	routeIdx := map[string]int32{}
	if err := eachRow(filepath.Join(dir, "routes.txt"), func(t *csvTable) error {
		short := strings.TrimSpace(t.get("route_short_name"))
		if short == "" {
			short = t.get("route_id")
		}
		routeIdx[t.get("route_id")] = int32(len(s.Routes))
		s.Routes = append(s.Routes, STASYRoute{ID: t.get("route_id"), Short: short, Color: t.get("route_color")})
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRow(filepath.Join(dir, "stops.txt"), func(t *csvTable) error {
		i := int32(len(s.Stops))
		s.stopIdx[t.get("stop_id")] = i
		lat, _ := strconv.ParseFloat(strings.TrimSpace(t.get("stop_lat")), 64)
		lon, _ := strconv.ParseFloat(strings.TrimSpace(t.get("stop_lon")), 64)
		s.Stops = append(s.Stops, STASYStop{ID: t.get("stop_id"), Name: strings.TrimSpace(t.get("stop_name")), Lat: lat, Lon: lon})
		k := Fold(s.Stops[i].Name)
		s.byName[k] = append(s.byName[k], i)
		return nil
	}); err != nil {
		return nil, err
	}
	tripIdx := map[string]int32{}
	s.tripIdx = tripIdx
	if err := eachRow(filepath.Join(dir, "trips.txt"), func(t *csvTable) error {
		route, ok := routeIdx[t.get("route_id")]
		if !ok {
			return nil // a trip of a route the feed does not list
		}
		tripIdx[t.get("trip_id")] = int32(len(s.trips))
		s.trips = append(s.trips, stasyTrip{ID: t.get("trip_id"), Route: route, Service: s.service(t.get("service_id")),
			Headsign: strings.TrimSpace(t.get("trip_headsign"))})
		return nil
	}); err != nil {
		return nil, err
	}
	// calendar_dates.txt and calendar.txt: STASY publishes calendar_dates only (one service per
	// date); both are read when present, so a future feed with either works.
	if _, err := os.Stat(filepath.Join(dir, "calendar_dates.txt")); err == nil {
		if err := eachRow(filepath.Join(dir, "calendar_dates.txt"), func(t *csvTable) error {
			date, err := strconv.Atoi(strings.TrimSpace(t.get("date")))
			if err != nil {
				return fmt.Errorf("calendar_dates: bad date %q", t.get("date"))
			}
			svc := s.service(t.get("service_id"))
			v := &s.services[svc]
			switch strings.TrimSpace(t.get("exception_type")) {
			case "1":
				v.added[int32(date)] = true
			case "2":
				v.removed[int32(date)] = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "calendar.txt")); err == nil {
		if err := eachRow(filepath.Join(dir, "calendar.txt"), func(t *csvTable) error {
			v := &s.services[s.service(t.get("service_id"))]
			start, err1 := strconv.Atoi(strings.TrimSpace(t.get("start_date")))
			if err1 != nil {
				return fmt.Errorf("calendar: bad start_date")
			}
			end, err2 := strconv.Atoi(strings.TrimSpace(t.get("end_date")))
			if err2 != nil {
				return fmt.Errorf("calendar: bad end_date")
			}
			v.hasCalendar, v.start, v.end = true, int32(start), int32(end)
			for i, d := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
				v.days[i] = strings.TrimSpace(t.get(d)) == "1"
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	// STASY leaves trip_headsign empty on most trips, so the headsign is taken from the last
	// stop of the trip: that is the name a passenger reads on the platform.
	s.tripStops = make([][]stasyStopTime, len(s.trips))
	lastStop := make([]int32, len(s.trips))
	lastSeq := make([]int32, len(s.trips))
	hasStop := make([]bool, len(s.trips))
	for i := range lastSeq {
		lastSeq[i] = -1
	}
	if err := eachRow(filepath.Join(dir, "stop_times.txt"), func(t *csvTable) error {
		ti, ok := tripIdx[t.get("trip_id")]
		if !ok {
			return nil
		}
		si, ok := s.stopIdx[t.get("stop_id")]
		if !ok {
			return nil
		}
		seq := int32(-1)
		if v := strings.TrimSpace(t.get("stop_sequence")); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				seq = int32(n)
			}
		}
		if seq >= lastSeq[ti] {
			lastSeq[ti], lastStop[ti], hasStop[ti] = seq, si, true
		}
		// STASY writes both times on every row; a blank departure falls back to the arrival.
		v := t.get("departure_time")
		if strings.TrimSpace(v) == "" {
			v = t.get("arrival_time")
		}
		if strings.TrimSpace(v) == "" {
			return nil
		}
		dep, err := gtfs.ParseHMS(v)
		if err != nil {
			return fmt.Errorf("stop_times: trip %s: %w", t.get("trip_id"), err)
		}
		s.byStop[si] = append(s.byStop[si], stasyRow{Trip: ti, Dep: dep})
		// The trains stand 10-45 s at most stops: arrival and departure differ on most rows.
		arr := dep
		if v := t.get("arrival_time"); strings.TrimSpace(v) != "" {
			if a, err := gtfs.ParseHMS(v); err == nil && a <= dep {
				arr = a
			}
		}
		s.tripStops[ti] = append(s.tripStops[ti], stasyStopTime{Stop: si, A: arr, T: dep, seq: seq})
		return nil
	}); err != nil {
		return nil, err
	}
	if len(s.trips) == 0 || len(s.Stops) == 0 {
		return nil, fmt.Errorf("stasy: no trips or stops in %s", dir)
	}
	for _, st := range s.tripStops {
		sort.SliceStable(st, func(a, b int) bool { return st[a].seq < st[b].seq })
	}
	for i := range s.trips {
		if s.trips[i].Headsign == "" && hasStop[i] {
			s.trips[i].Headsign = s.Stops[lastStop[i]].Name
		}
	}
	for i := range s.services {
		v := &s.services[i]
		for d := range v.added {
			s.extend(d, d)
		}
		for d := range v.removed {
			s.extend(d, d)
		}
		if v.hasCalendar {
			s.extend(v.start, v.end)
		}
	}
	return s, nil
}

func (s *STASY) extend(start, end int32) {
	if s.start == 0 || start < s.start {
		s.start = start
	}
	if end > s.end {
		s.end = end
	}
}

func (s *STASY) service(id string) int32 {
	if i, ok := s.serviceIdx[id]; ok {
		return i
	}
	i := int32(len(s.services))
	s.serviceIdx[id] = i
	s.services = append(s.services, stasyService{added: map[int32]bool{}, removed: map[int32]bool{}})
	return i
}

// csvTable is a CSV reader that addresses columns by header name.
type csvTable struct {
	r    *csv.Reader
	cols map[string]int
	rec  []string
}

func (t *csvTable) get(name string) string {
	if i, ok := t.cols[name]; ok && i < len(t.rec) {
		return t.rec[i]
	}
	return ""
}

func eachRow(file string, fn func(*csvTable) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(bufio.NewReaderSize(f, 1<<16))
	r.ReuseRecord = true
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("%s: %w", path.Base(file), err)
	}
	cols := map[string]int{}
	for i, h := range header {
		cols[strings.TrimSpace(strings.TrimPrefix(h, "\xef\xbb\xbf"))] = i
	}
	t := &csvTable{r: r, cols: cols}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path.Base(file), err)
		}
		t.rec = rec
		if err := fn(t); err != nil {
			return err
		}
	}
}

// STASYStore keeps the timetable current: it loads the files already on disk at start, then
// checks the CKAN catalogue once a day and reloads when metadata_modified changes.
type STASYStore struct {
	CatalogURL string
	Dir        string // where the .txt files are kept
	HTTP       *http.Client
	Log        *slog.Logger
	Now        func() time.Time

	mu      sync.RWMutex
	cur     *STASY
	modif   string
	updated time.Time
}

// Feed returns the timetable, ok false until one is loaded.
func (s *STASYStore) Feed() (*STASY, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur, s.cur != nil
}

// Run loads the timetable and then refreshes it daily until ctx ends.
func (s *STASYStore) Run(ctx context.Context) {
	s.loadLocal()
	for {
		wait := stasyCheck
		if err := s.refresh(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("stasy: refresh failed", "err", err)
			wait = stasyRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *STASYStore) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *STASYStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// loadLocal parses the files of a previous run, so a restart serves departures before the first
// catalogue check finishes.
func (s *STASYStore) loadLocal() {
	f, err := ParseSTASY(s.Dir)
	if err != nil {
		return
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, stasyFile))
	s.mu.Lock()
	s.cur, s.modif, s.updated = f, strings.TrimSpace(string(b)), s.now()
	s.mu.Unlock()
	s.log().Info("stasy: timetable loaded from disk", "trips", f.NumTrips(), "modified", s.modif)
}

func (s *STASYStore) refresh(ctx context.Context) error {
	cat, err := s.catalog(ctx)
	if err != nil {
		return err
	}
	s.mu.RLock()
	same := s.cur != nil && cat.Modified != "" && s.modif == cat.Modified
	s.mu.RUnlock()
	if same {
		return nil
	}
	tmp, err := s.download(ctx, cat.files)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // gone with the replace; otherwise the failed generation
	f, err := ParseSTASY(tmp)
	if err != nil {
		return err
	}
	if err := s.replace(tmp, cat.Modified); err != nil {
		return err
	}
	s.mu.Lock()
	s.cur, s.modif, s.updated = f, cat.Modified, s.now()
	s.mu.Unlock()
	s.log().Info("stasy: timetable updated", "modified", cat.Modified, "trips", f.NumTrips())
	return nil
}

type stasyCatalog struct {
	Modified string
	files    map[string]string // file name -> URL
}

// catalog reads package_show and picks the GTFS text resources by file name.
func (s *STASYStore) catalog(ctx context.Context) (stasyCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.CatalogURL, nil)
	if err != nil {
		return stasyCatalog{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := s.client().Do(req)
	if err != nil {
		return stasyCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return stasyCatalog{}, fmt.Errorf("catalog: HTTP %d", resp.StatusCode)
	}
	var p struct {
		Success bool `json:"success"`
		Result  struct {
			MetadataModified string `json:"metadata_modified"`
			Resources        []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
				Fmt  string `json:"format"`
			} `json:"resources"`
		} `json:"result"`
	}
	b, err := readCapped(resp.Body, maxJSONBody)
	if err != nil {
		return stasyCatalog{}, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return stasyCatalog{}, err
	}
	files := map[string]string{}
	for _, r := range p.Result.Resources {
		u, err := url.Parse(strings.TrimSpace(r.URL))
		if err != nil {
			continue
		}
		name := path.Base(u.Path)
		if !stasyFileWanted[name] {
			continue
		}
		files[name] = r.URL
	}
	for _, name := range stasyRequired {
		if files[name] == "" {
			return stasyCatalog{}, fmt.Errorf("catalog: no %s resource", name)
		}
	}
	if files["calendar.txt"] == "" && files["calendar_dates.txt"] == "" {
		return stasyCatalog{}, fmt.Errorf("catalog: no calendar resource")
	}
	return stasyCatalog{Modified: p.Result.MetadataModified, files: files}, nil
}

var stasyRequired = []string{"stops.txt", "routes.txt", "trips.txt", "stop_times.txt"}

// stasyFileWanted are the GTFS tables the parser reads; other resources (agency.txt) are skipped.
var stasyFileWanted = map[string]bool{"stops.txt": true, "routes.txt": true, "trips.txt": true,
	"stop_times.txt": true, "calendar.txt": true, "calendar_dates.txt": true}

func (s *STASYStore) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 4 * time.Minute}
}

// Upstream bodies are capped: a broken or hostile source must not fill the disk or the heap.
const (
	maxJSONBody = 5 << 20   // one JSON document: the catalogue, a board, a screen
	maxFileBody = 200 << 20 // one GTFS text file
)

// errBodyTooLarge is returned for a body over its cap. Nothing of it is kept: a truncated file
// would parse as a timetable that is missing trips.
var errBodyTooLarge = errors.New("body too large")

// readCapped reads a whole body into memory, refusing one over max bytes.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errBodyTooLarge
	}
	return b, nil
}

// copyCapped copies a body to dst, refusing one over max bytes.
func copyCapped(dst io.Writer, src io.Reader, max int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, errBodyTooLarge
	}
	return n, nil
}

// download fetches the chosen files into a fresh temporary directory next to Dir. The live copy
// keeps serving the previous generation until replace moves the whole directory into place.
func (s *STASYStore) download(ctx context.Context, files map[string]string) (string, error) {
	root := filepath.Dir(s.Dir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, filepath.Base(s.Dir)+".new-")
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.downloadOne(ctx, files[name], filepath.Join(tmp, name)); err != nil {
			os.RemoveAll(tmp)
			return "", fmt.Errorf("%s: %w", name, err)
		}
	}
	return tmp, nil
}

// replace moves a downloaded generation over Dir with its catalogue version, so a failed
// download or parse never leaves files of two generations on disk.
func (s *STASYStore) replace(tmp, modified string) error {
	if err := os.WriteFile(filepath.Join(tmp, stasyFile), []byte(modified), 0o644); err != nil {
		return err
	}
	old := s.Dir + ".old"
	os.RemoveAll(old)
	backup := false
	if _, err := os.Stat(s.Dir); err == nil {
		if err := os.Rename(s.Dir, old); err != nil {
			return err
		}
		backup = true
	}
	if err := os.Rename(tmp, s.Dir); err != nil {
		if backup {
			os.Rename(old, s.Dir)
		}
		return err
	}
	os.RemoveAll(old)
	return nil
}

func (s *STASYStore) downloadOne(ctx context.Context, from, to string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", from, resp.StatusCode)
	}
	out, err := os.Create(to + ".tmp")
	if err != nil {
		return err
	}
	_, err = copyCapped(out, resp.Body, maxFileBody)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(to + ".tmp")
		return err
	}
	return os.Rename(to+".tmp", to)
}
