package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
