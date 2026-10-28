package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angelospk/athens-transit-rt/internal/stats"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// mcpSession connects an in-memory MCP client to the app's MCP server.
func mcpSession(t *testing.T, a *App) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := a.MCPServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callTool returns the tool's JSON text and whether it is an error result.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: content %+v", name, res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s: not text: %T", name, res.Content[0])
	}
	return tc.Text, res.IsError
}

func TestMCPTools(t *testing.T) {
	cs := mcpSession(t, newApp(t, monday1020))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" {
			t.Errorf("%s: no description", tl.Name)
		}
	}
	sort.Strings(names)
	want := []string{"find_stops", "line_now", "line_timetable", "metro_departures", "network_day", "search_lines", "stop_arrivals", "suburban_departures"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools %v, want %v", names, want)
	}
}

func TestMCPLineTools(t *testing.T) {
	a := newApp(t, monday1020)
	cs := mcpSession(t, a)

	text, isErr := callTool(t, cs, "line_timetable", map[string]any{"line": "L"})
	var sr ScheduleResponse
	if isErr || json.Unmarshal([]byte(text), &sr) != nil || sr.Source != "gtfs" || len(sr.Directions) != 1 || len(sr.Directions[0].Departures) != 6 {
		t.Fatalf("line_timetable %v %s", isErr, text)
	}

	poll(a, monday1020)
	text, isErr = callTool(t, cs, "line_now", map[string]any{"line": "L"})
	var now struct {
		Line     string `json:"line"`
		Vehicles []struct {
			NextStopID   string `json:"next_stop_id"`
			NextStopName string `json:"next_stop_name"`
		} `json:"vehicles"`
	}
	if isErr || json.Unmarshal([]byte(text), &now) != nil || now.Line != "L" || len(now.Vehicles) == 0 {
		t.Fatalf("line_now %v %s", isErr, text)
	}
	for _, v := range now.Vehicles {
		if v.NextStopID != "" && !strings.HasPrefix(v.NextStopName, "STOP ") {
			t.Errorf("vehicle next stop %q has no name: %q", v.NextStopID, v.NextStopName)
		}
	}

	if text, isErr = callTool(t, cs, "line_now", map[string]any{"line": "NOPE"}); !isErr || !strings.Contains(text, "unknown") {
		t.Fatalf("unknown line: %v %s", isErr, text)
	}
}

func TestMCPSearch(t *testing.T) {
	cs := mcpSession(t, newApp(t, monday1020))
	// Case and accents do not matter.
	text, isErr := callTool(t, cs, "search_lines", map[string]any{"query": "α1"})
	if isErr || !strings.Contains(text, `"line":"Α1"`) {
		t.Fatalf("search_lines %v %s", isErr, text)
	}
	text, isErr = callTool(t, cs, "find_stops", map[string]any{"query": "stop 3"})
	var stops []struct {
		ID   string  `json:"id"`
		Name string  `json:"name"`
		Lat  float64 `json:"lat"`
	}
	if isErr || json.Unmarshal([]byte(text), &stops) != nil || len(stops) != 1 || stops[0].ID != "S3" || stops[0].Lat == 0 {
		t.Fatalf("find_stops %v %s", isErr, text)
	}
}

// OASA's own predictions at a stop, by line number (route codes mapped through the line metadata).
func TestMCPStopArrivals(t *testing.T) {
	a := newApp(t, monday1020)
	a.stopArrivals = func(_ context.Context, code string) ([]telematics.Arrival, error) {
		if code != "S2" {
			t.Errorf("asked stop %q", code)
		}
		return []telematics.Arrival{{RouteCode: "SH", VehCode: "V9", Minutes: 4}, {RouteCode: "XX", Minutes: 9}}, nil
	}
	cs := mcpSession(t, a)
	text, isErr := callTool(t, cs, "stop_arrivals", map[string]any{"stop_id": "S2"})
	var arr []struct {
		Line    string `json:"line"`
		Minutes int    `json:"minutes"`
		Vehicle string `json:"vehicle"`
	}
	if isErr || json.Unmarshal([]byte(text), &arr) != nil || len(arr) != 2 || arr[0].Line != "L" || arr[0].Minutes != 4 || arr[0].Vehicle != "V9" || arr[1].Line != "" {
		t.Fatalf("stop_arrivals %v %s", isErr, text)
	}
	if _, isErr = callTool(t, cs, "stop_arrivals", map[string]any{"stop_id": ""}); !isErr {
		t.Fatal("empty stop id accepted")
	}
}

// The MCP endpoint over Streamable HTTP, stateless, as Claude and ChatGPT connectors call it.
func TestMCPHTTP(t *testing.T) {
	a := newApp(t, monday1020)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "serverInfo") || !strings.Contains(rec.Body.String(), "athens-transit") {
		t.Fatalf("initialize %d %s", rec.Code, rec.Body.String())
	}
}

// Stateless: a tools/call needs no session from an earlier initialize.
func TestMCPHTTPToolCall(t *testing.T) {
	a := newApp(t, monday1020)
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"line_timetable","arguments":{"line":"L"}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `\"source\":\"gtfs\"`) {
		t.Fatalf("tools/call %d %s", rec.Code, rec.Body.String())
	}
}

// Callers asking the same stop at once share one OASA call.
func TestMCPStopArrivalsConcurrent(t *testing.T) {
	a := newApp(t, monday1020)
	var calls atomic.Int32
	release := make(chan struct{})
	a.stopArrivals = func(context.Context, string) ([]telematics.Arrival, error) {
		calls.Add(1)
		<-release
		return []telematics.Arrival{{RouteCode: "SH", Minutes: 3}}, nil
	}
	cs := mcpSession(t, a)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if text, isErr := callTool(t, cs, "stop_arrivals", map[string]any{"stop_id": "S2"}); isErr {
				t.Errorf("error %s", text)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d OASA calls for 5 concurrent requests", calls.Load())
	}
}

// Queued OASA work is bounded: past the cap a caller is told at once, and work whose caller left
// is dropped, not sent, when its turn comes.
func TestQueuedBoundedAndAbandoned(t *testing.T) {
	var hits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("act") == "getStopArrivals" {
			hits.Add(1)
		}
		w.Write([]byte("[]"))
	}))
	defer fake.Close()
	a := newApp(t, monday1020)
	a.client.BaseURL = fake.URL + "/"
	for i := range maxQueued {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		if _, err := a.stopArrivalsQueued(ctx, fmt.Sprintf("S%d", i)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("job %d: %v", i, err)
		}
		cancel()
	}
	start := time.Now()
	if _, err := a.stopArrivalsQueued(context.Background(), "S9"); !errors.Is(err, errQueueFull) || time.Since(start) > time.Second {
		t.Fatalf("past the cap: %v after %v", err, time.Since(start))
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go a.sched.Run(ctx)
	for end := time.Now().Add(10 * time.Second); a.queuedN.Load() > 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("abandoned jobs not dropped: %d queued", a.queuedN.Load())
		}
	}
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.stopArrivalsQueued(wait, "S1"); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d OASA calls; the %d abandoned jobs must not be sent", n, maxQueued)
	}
}

// Behind a reverse proxy on localhost the public Host header is fine (the data is public anyway).
func TestMCPHTTPBehindLocalProxy(t *testing.T) {
	a := newApp(t, monday1020)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Host = "transit.example.org"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8095}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("behind a local proxy: %d %s", rec.Code, rec.Body.String())
	}
}

// A client that sends the headers and then stalls the body is cut off.
func TestMCPStalledBody(t *testing.T) {
	old := mcpBodyTimeout
	mcpBodyTimeout = 200 * time.Millisecond
	defer func() { mcpBodyTimeout = old }()
	srv := httptest.NewServer(newApp(t, monday1020).Handler())
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: 1000\r\n\r\n{\"jsonrpc\":")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	status, err := bufio.NewReader(conn).ReadString('\n') // the answer comes once the server gives up on the body
	if err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("stalled body: %q %v after %v", status, err, time.Since(start))
	}
}

func TestMCPToolsAreReadOnly(t *testing.T) {
	cs := mcpSession(t, newApp(t, monday1020))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s: not marked read-only", tl.Name)
		}
	}
}

// A whole body lifts the deadline: the same keep-alive connection serves the next call later.
func TestMCPKeepAliveAfterDeadline(t *testing.T) {
	old := mcpBodyTimeout
	mcpBodyTimeout = 200 * time.Millisecond
	defer func() { mcpBodyTimeout = old }()
	srv := httptest.NewServer(newApp(t, monday1020).Handler())
	defer srv.Close()
	call := func() {
		body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_lines","arguments":{"query":"L"}}}`
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(b), `\"line\":\"L\"`) {
			t.Fatalf("%d %s", res.StatusCode, b)
		}
	}
	call()
	time.Sleep(400 * time.Millisecond)
	call()
}

// The stalled-body guard also holds for clients that accept gzip (the API's gzip writer).
func TestMCPStalledBodyGzip(t *testing.T) {
	old := mcpBodyTimeout
	mcpBodyTimeout = 200 * time.Millisecond
	defer func() { mcpBodyTimeout = old }()
	srv := httptest.NewServer(newApp(t, monday1020).Handler())
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: 1000\r\n\r\n{\"jsonrpc\":")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if status, err := bufio.NewReader(conn).ReadString('\n'); err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("stalled gzip body: %q %v after %v", status, err, time.Since(start))
	}
}

// The first caller leaving does not fail the others waiting for the same stop.
func TestMCPStopArrivalsLeaderLeaves(t *testing.T) {
	a := newApp(t, monday1020)
	started, release := make(chan struct{}), make(chan struct{})
	a.stopArrivals = func(ctx context.Context, _ string) ([]telematics.Arrival, error) {
		close(started)
		select {
		case <-release:
			return []telematics.Arrival{{RouteCode: "SH", Minutes: 2}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	leader, leave := context.WithCancel(context.Background())
	go a.arrivals(leader, "S2")
	<-started
	got := make(chan error, 1)
	go func() {
		_, err := a.arrivals(context.Background(), "S2")
		got <- err
	}()
	time.Sleep(20 * time.Millisecond)
	leave()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-got; err != nil {
		t.Fatalf("waiter failed with the leader: %v", err)
	}
}

// With a valid timetable a line that ran none of its trips is the worst, not skipped; with an
// expired one it may not exist any more and is left out.
func TestSummarizeDayMissedAll(t *testing.T) {
	d := stats.Day{Trips: &stats.Trips{ByLine: []stats.TripLine{{Line: "Z", TripCount: stats.TripCount{Planned: 20, Missed: 20}}}}}
	if s := summarizeDay(d); len(s.MostMissed) != 1 || s.MostMissed[0].Line != "Z" {
		t.Fatalf("valid timetable: %+v", s.MostMissed)
	}
	d.Trips.GTFSExpired = true
	if s := summarizeDay(d); len(s.MostMissed) != 0 {
		t.Fatalf("expired timetable: %+v", s.MostMissed)
	}
}
