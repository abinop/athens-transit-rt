package rail

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const positionsEvent = `{"positions":[
 {"trainNumber":"1226","scheduleId":"s1","serviceType":"Suburban","origin":"Πειραιάς","destination":"Αεροδρόμιο",
  "nextStation":"Κάτω Αχαρναί","delay":3,"corridor":"PIRAIR","lat":38.0487,"lng":23.7314,"speed":80.4,
  "course":10.5,"hasCourse":true,"timestamp":"2026-10-28T16:13:56.272Z"},
 {"trainNumber":"220035","scheduleId":null,"corridor":"KIATO","lat":37.959,"lng":23.665,"speed":0.01,
  "course":75.6,"hasCourse":true,"timestamp":"2026-10-28T16:13:56.272Z"},
 {"trainNumber":"1324","scheduleId":"s2","serviceType":"Suburban","origin":"Πειραιάς","destination":"Κιάτο",
  "nextStation":null,"delay":null,"corridor":"KIATO","lat":37.99,"lng":23.6,"speed":null,"course":null,
  "hasCourse":false,"timestamp":"2026-10-28T16:13:50.000Z"}]}`

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// sse writes events the way railway.gov.gr does, then holds the connection until the client
// leaves (hold) or ends it.
func sseServer(t *testing.T, events []string, hold bool, conns *atomic.Int32, closed chan<- struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/train-stream" || r.Header.Get("Accept") != "text/event-stream" {
			http.NotFound(w, r)
			return
		}
		conns.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			io.WriteString(w, e)
		}
		w.(http.Flusher).Flush()
		if hold {
			<-r.Context().Done()
			if closed != nil {
				closed <- struct{}{}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTrainStreamSnapshot(t *testing.T) {
	var conns atomic.Int32
	events := []string{
		"event: trainHistory\ndata: {\"big\":true}\n\n",
		"event: trainPositionsUx\ndata: " + strings.ReplaceAll(positionsEvent, "\n", "") + "\n\n",
	}
	srv := sseServer(t, events, true, &conns, nil)
	s := &TrainStream{BaseURL: srv.URL, Log: quietLog()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	if s.Vehicles() != nil {
		t.Fatal("a snapshot before the first event")
	}
	var snap *SuburbanVehicles
	waitFor(t, "the first snapshot", func() bool { snap = s.Vehicles(); return snap != nil })
	if conns.Load() != 1 {
		t.Errorf("connections = %d", conns.Load())
	}
	// The parked locomotive (no scheduleId) is left out; sorted by train number.
	if len(snap.Trains) != 2 || snap.Trains[0].Train != "1226" || snap.Trains[1].Train != "1324" {
		t.Fatalf("trains = %+v", snap.Trains)
	}
	a := snap.Trains[0]
	if a.To != "Αεροδρόμιο" || a.From != "Πειραιάς" || a.Next != "Κάτω Αχαρναί" || a.Corridor != "PIRAIR" || a.Service != "Suburban" ||
		a.Lat != 38.0487 || a.Lon != 23.7314 || a.SpeedKmh != 80.4 || a.Course == nil || *a.Course != 10.5 || a.DelayMin == nil ||
		*a.DelayMin != 3 || a.PositionAt != time.Date(2026, 10, 28, 16, 13, 56, 0, time.UTC).Unix() {
		t.Errorf("1226 = %+v", a)
	}
	b := snap.Trains[1]
	if b.Course != nil || b.DelayMin != nil || b.SpeedKmh != 0 || b.Next != "" {
		t.Errorf("1324 = %+v", b)
	}
	if d := s.Delays(); len(d) != 1 || d["1226"] != 3 {
		t.Errorf("delays = %v", d)
	}
}

func TestTrainStreamClosesWhenIdle(t *testing.T) {
	var conns atomic.Int32
	closed := make(chan struct{}, 1)
	srv := sseServer(t, []string{"event: trainPositionsUx\ndata: {\"positions\":[]}\n\n"}, true, &conns, closed)
	s := &TrainStream{BaseURL: srv.URL, Log: quietLog(), Idle: 150 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Vehicles()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream stayed open with no client")
	}
	time.Sleep(100 * time.Millisecond)
	if conns.Load() != 1 {
		t.Fatalf("connections = %d: an idle stream must not reconnect", conns.Load())
	}
	// A new request opens it again.
	s.Vehicles()
	waitFor(t, "a second connection", func() bool { return conns.Load() == 2 })
}

func TestTrainStreamReconnects(t *testing.T) {
	var conns atomic.Int32
	// The server ends each stream after one event; the client opens a new one after Retry.
	srv := sseServer(t, []string{"event: trainPositionsUx\ndata: {\"positions\":[]}\n\n"}, false, &conns, nil)
	s := &TrainStream{BaseURL: srv.URL, Log: quietLog(), Retry: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Vehicles()
	waitFor(t, "a reconnection", func() bool { return conns.Load() >= 2 })
}

func TestTrainStreamStall(t *testing.T) {
	var conns atomic.Int32
	// Events stop but the connection stays open: after Stall it is reopened.
	srv := sseServer(t, []string{"event: trainPositionsUx\ndata: {\"positions\":[]}\n\n"}, true, &conns, nil)
	s := &TrainStream{BaseURL: srv.URL, Log: quietLog(), Stall: 100 * time.Millisecond, Retry: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Vehicles()
	waitFor(t, "a reconnection after a stall", func() bool { return conns.Load() >= 2 })
}

func TestTrainStreamDelaysNeedAFreshSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC)
	s := &TrainStream{Log: quietLog(), Now: func() time.Time { return now }}
	s.dispatch("trainPositionsUx", strings.ReplaceAll(positionsEvent, "\n", ""))
	if d := s.Delays(); d["1226"] != 3 {
		t.Fatalf("delays = %v", d)
	}
	now = now.Add(TrainFresh + time.Second)
	if d := s.Delays(); d != nil {
		t.Fatalf("stale delays = %v", d)
	}
}

func TestTrainStreamMultiLineData(t *testing.T) {
	s := &TrainStream{Log: quietLog()}
	body := "event: trainPositionsUx\r\ndata: {\"positions\":\r\ndata: []}\r\n\r\n"
	if err := s.read(strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if s.snap == nil || len(s.snap.Trains) != 0 {
		t.Fatalf("snap = %+v", s.snap)
	}
}

func TestTrainStreamBackoffStartsAgainAfterAWorkingStream(t *testing.T) {
	var conns atomic.Int32
	// Every stream ends after one event: each worked, so the wait stays Retry and never doubles
	// (30, 60, 120, 240 ms would give about 5 connections in 500 ms).
	srv := sseServer(t, []string{"event: trainPositionsUx\ndata: {\"positions\":[]}\n\n"}, false, &conns, nil)
	s := &TrainStream{BaseURL: srv.URL, Log: quietLog(), Retry: 30 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Vehicles()
	time.Sleep(500 * time.Millisecond)
	if n := conns.Load(); n < 9 {
		t.Fatalf("connections = %d: the backoff grew after working streams", n)
	}
}
