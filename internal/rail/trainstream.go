package rail

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// The live positions of railway.gov.gr's train tracker (docs/research/rail-data.md in the app
// repo): GET /api/train-stream is a server-sent event stream whose "trainPositionsUx" event
// carries every train with its GPS position about once a second (~80 KB/s). The server keeps one
// connection, only while clients ask for the trains, and serves the last snapshot.
const (
	trainIdle     = 3 * time.Minute  // no client asked this long: the stream closes
	trainStall    = 30 * time.Second // no event this long: the stream is reopened
	trainRetry    = 5 * time.Second  // the first wait after a failed connection; it doubles
	trainRetryMax = time.Minute
	// TrainFresh is how old a snapshot may be and still give live delays.
	TrainFresh = 2 * time.Minute
)

// SuburbanTrain is one train in service with its last GPS position.
type SuburbanTrain struct {
	Train      string   `json:"train"`
	Corridor   string   `json:"corridor"`
	Service    string   `json:"service"` // Suburban, Regional, InterCity, ...
	Lat        float64  `json:"lat"`
	Lon        float64  `json:"lon"`
	Course     *float64 `json:"course"` // degrees from north, null when unknown
	SpeedKmh   float64  `json:"speed_kmh"`
	DelayMin   *int     `json:"delay_min"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Next       string   `json:"next_station"`
	PositionAt int64    `json:"position_at"` // unix seconds of the GPS fix
}

// SuburbanVehicles is the body of the vehicles endpoint.
type SuburbanVehicles struct {
	UpdatedAt int64           `json:"updated_at"`
	Trains    []SuburbanTrain `json:"trains"`
}

// TrainStream keeps the last snapshot of the train-stream. Run it once; Vehicles wakes it.
type TrainStream struct {
	BaseURL string       // "" = DefaultRailwayURL
	HTTP    *http.Client // must have no Timeout: the response is a stream
	Now     func() time.Time
	Log     *slog.Logger
	// Zero values take the defaults above (tests shorten them).
	Idle, Stall, Retry time.Duration

	once  sync.Once
	wake  chan struct{}
	mu    sync.Mutex
	snap  *SuburbanVehicles
	asked time.Time
	event time.Time // the last event of any kind
}

func (s *TrainStream) init() { s.once.Do(func() { s.wake = make(chan struct{}, 1) }) }

// Vehicles returns the last snapshot, nil until the first one arrives, and keeps the stream
// open (or opens it) for the next Idle.
func (s *TrainStream) Vehicles() *SuburbanVehicles {
	s.init()
	s.mu.Lock()
	s.asked = s.now()
	snap := s.snap
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return snap
}

// Delays returns the live delay of each train of a fresh snapshot, by train number. It does
// not open the stream: without clients on the map, the boards' own delays are used.
func (s *TrainStream) Delays() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap == nil || s.now().Sub(time.Unix(s.snap.UpdatedAt, 0)) > TrainFresh {
		return nil
	}
	out := map[string]int{}
	for _, t := range s.snap.Trains {
		if t.DelayMin != nil {
			out[t.Train] = *t.DelayMin
		}
	}
	return out
}

// Run serves wake-ups until ctx ends: on one, it reads the stream until no client asked for
// Idle, reconnecting after failures.
func (s *TrainStream) Run(ctx context.Context) {
	s.init()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		retry := s.dur(s.Retry, trainRetry)
		for ctx.Err() == nil && !s.idle() {
			opened := s.now()
			err := s.stream(ctx)
			if ctx.Err() != nil || s.idle() {
				break
			}
			if s.eventSince(opened) { // a stream that sent events starts the backoff again
				retry = s.dur(s.Retry, trainRetry)
			}
			if err == nil {
				continue
			}
			s.log().Warn("train-stream: reconnecting", "err", err, "in", retry)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
			retry = min(retry*2, trainRetryMax)
		}
	}
}

func (s *TrainStream) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Sub(s.asked) >= s.dur(s.Idle, trainIdle)
}

func (s *TrainStream) eventSince(t time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.event.Before(t)
}

func (s *TrainStream) stalled(since time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.event
	if last.Before(since) {
		last = since
	}
	return s.now().Sub(last) >= s.dur(s.Stall, trainStall)
}

// stream reads one connection until it fails, stalls or no client asks any more (nil error).
func (s *TrainStream) stream(ctx context.Context) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opened := s.now()
	var why error
	var whyMu sync.Mutex
	go func() { // the watchdog: a read blocks, so only a cancel ends it
		tick := time.NewTicker(min(s.dur(s.Stall, trainStall)/4, time.Second))
		defer tick.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-tick.C:
				if s.idle() {
					cancel()
					return
				}
				if s.stalled(opened) {
					whyMu.Lock()
					why = fmt.Errorf("no event for %v", s.dur(s.Stall, trainStall))
					whyMu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	base := s.BaseURL
	if base == "" {
		base = DefaultRailwayURL
	}
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/api/train-stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/event-stream")
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if sctx.Err() != nil && ctx.Err() == nil {
			whyMu.Lock()
			defer whyMu.Unlock()
			return why
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("train-stream: HTTP %d", resp.StatusCode)
	}
	err = s.read(resp.Body)
	if sctx.Err() != nil && ctx.Err() == nil {
		whyMu.Lock()
		defer whyMu.Unlock()
		return why // nil when the watchdog closed an idle stream
	}
	if err == nil {
		err = io.ErrUnexpectedEOF // the server ended the stream
	}
	return err
}

// read parses the event stream: "event:" and "data:" lines, an empty line ends an event.
func (s *TrainStream) read(body io.Reader) error {
	r := bufio.NewReaderSize(body, 64<<10)
	var event string
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event != "" || data.Len() > 0 {
				s.dispatch(event, data.String())
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line[len("data:"):], " "))
		}
		if data.Len() > 8<<20 {
			return fmt.Errorf("train-stream: event over 8 MB")
		}
	}
}

// trainPosition is the part of a "trainPositionsUx" position we read.
type trainPosition struct {
	TrainNumber string   `json:"trainNumber"`
	ScheduleID  *string  `json:"scheduleId"`
	ServiceType *string  `json:"serviceType"`
	Origin      *string  `json:"origin"`
	Destination *string  `json:"destination"`
	NextStation *string  `json:"nextStation"`
	Delay       *int     `json:"delay"`
	Corridor    string   `json:"corridor"`
	Lat         *float64 `json:"lat"`
	Lng         *float64 `json:"lng"`
	Speed       *float64 `json:"speed"`
	Course      *float64 `json:"course"`
	HasCourse   bool     `json:"hasCourse"`
	Timestamp   string   `json:"timestamp"`
}

func (s *TrainStream) dispatch(event, data string) {
	s.mu.Lock()
	s.event = s.now()
	s.mu.Unlock()
	if event != "trainPositionsUx" {
		return
	}
	var p struct {
		Positions []trainPosition `json:"positions"`
	}
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		s.log().Warn("train-stream: bad positions event", "err", err)
		return
	}
	snap := &SuburbanVehicles{UpdatedAt: s.now().Unix(), Trains: []SuburbanTrain{}}
	for _, t := range p.Positions {
		// A locomotive with no run (parked, shunting) is not a train a passenger can take.
		if t.ScheduleID == nil || *t.ScheduleID == "" || t.Lat == nil || t.Lng == nil {
			continue
		}
		tr := SuburbanTrain{Train: strings.TrimSpace(t.TrainNumber), Corridor: t.Corridor, Service: str(t.ServiceType),
			Lat: *t.Lat, Lon: *t.Lng, DelayMin: t.Delay, From: str(t.Origin), To: str(t.Destination), Next: str(t.NextStation)}
		if t.Speed != nil {
			tr.SpeedKmh = *t.Speed
		}
		if t.HasCourse && t.Course != nil {
			c := *t.Course
			tr.Course = &c
		}
		if at, err := time.Parse(time.RFC3339, t.Timestamp); err == nil {
			tr.PositionAt = at.Unix()
		}
		snap.Trains = append(snap.Trains, tr)
	}
	sort.Slice(snap.Trains, func(i, j int) bool { return snap.Trains[i].Train < snap.Trains[j].Train })
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

// str reads an optional name; the stream pads some ("Λάρισα ").
func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func (s *TrainStream) dur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func (s *TrainStream) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *TrainStream) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
