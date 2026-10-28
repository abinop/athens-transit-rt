package server

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/sched"
)

const (
	minMaxAge        = 5
	maxMaxAge        = 300
	inactiveMaxAge   = 120
	unknownMaxAge    = 60
	statusMaxAge     = 10
	gtfsRTMaxAge     = 15
	okRecentPollSecs = 5 * 60
)

type LineResponse struct {
	Line         string    `json:"line"`
	UpdatedAt    int64     `json:"updated_at"`
	NextUpdateAt int64     `json:"next_update_at"`
	Vehicles     []Vehicle `json:"vehicles"`
}

type StatusResponse struct {
	OK          bool    `json:"ok"`
	GTFSVersion string  `json:"gtfs_version"`
	GTFSExpires string  `json:"gtfs_expires"`
	UpdatedAt   int64   `json:"updated_at"`
	LinesActive int     `json:"lines_active"`
	BudgetRPS   float64 `json:"budget_rps"`
}

// Handler serves the live API of docs/CONTRACT.md.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/lines/{id}", a.handleLine)
	mux.HandleFunc("GET /v1/lines/{id}/schedule", a.handleSchedule)
	mux.HandleFunc("GET /v1/status", a.handleStatus)
	mux.HandleFunc("GET /v1/stops/{id}", a.handleStop)
	mux.HandleFunc("GET /v1/stops/{id}/arrivals", a.handleStopArrivals)
	mux.HandleFunc("GET /v1/vehicles", a.handleVehicles)
	mux.HandleFunc("GET /v1/vehicles/tiles/{z}/{x}/{y}", a.handleTile)
	mux.HandleFunc("GET /v1/gtfs-rt/{name}", a.handleGTFSRT)
	mux.HandleFunc("GET /v1/rail/stasy/departures", a.handleSTASYDepartures)
	mux.HandleFunc("GET /v1/rail/stasy/vehicles", a.handleSTASYVehicles)
	mux.HandleFunc("GET /v1/rail/stasy/trips/{id}", a.handleSTASYTrip)
	mux.HandleFunc("GET /v1/rail/suburban/stations", a.handleSuburbanStations)
	mux.HandleFunc("GET /v1/rail/suburban/stations/{id}/departures", a.handleSuburbanDepartures)
	mux.HandleFunc("GET /v1/rail/suburban/vehicles", a.handleSuburbanVehicles)
	mux.HandleFunc("GET /v1/rail/suburban/trains/{no}", a.handleSuburbanTrain)
	mux.HandleFunc("GET /v1/stats", a.handleStatsIndex)
	mux.HandleFunc("GET /v1/stats/days/{date}", a.handleStatsDay)
	mux.Handle("/mcp", a.mcpHandler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "not_found"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Add("Vary", "Accept-Encoding")
		if path.Clean(r.URL.Path) != r.URL.Path {
			// ServeMux would answer 301 with the cleaned path; such ids are never valid.
			writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "not_found"})
			return
		}
		if r.URL.Path == "/v1/vehicles" || strings.HasPrefix(r.URL.Path, "/v1/vehicles/tiles/") ||
			!acceptsGzip(r.Header.Get("Accept-Encoding")) {
			// /v1/vehicles and its tiles serve bytes compressed once per build.
			mux.ServeHTTP(w, r)
			return
		}
		zw := gzipPool.Get().(*gzip.Writer)
		zw.Reset(w)
		w.Header().Set("Content-Encoding", "gzip")
		mux.ServeHTTP(&gzipWriter{ResponseWriter: w, zw: zw}, r)
		zw.Close()
		gzipPool.Put(zw)
	})
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip (q > 0 for gzip or *).
func acceptsGzip(header string) bool {
	star := false
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		ok := true
		if k, v, found := strings.Cut(strings.TrimSpace(params), "="); found && strings.TrimSpace(k) == "q" {
			q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			ok = err == nil && q > 0
		}
		if name == "gzip" {
			return ok
		}
		star = ok
	}
	return star
}

var gzipPool = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return zw
}}

// gzipWriter compresses a response body (handlers never set Content-Length).
type gzipWriter struct {
	http.ResponseWriter
	zw *gzip.Writer
}

func (g *gzipWriter) Write(b []byte) (int, error) { return g.zw.Write(b) }

// Unwrap lets http.ResponseController reach the connection (read deadlines, flushing).
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func writeJSON(w http.ResponseWriter, status, maxAge int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	w.WriteHeader(status)
	w.Write(b)
}

func clamp(v, lo, hi int64) int64 { return max(lo, min(hi, v)) }

func (a *App) handleLine(w http.ResponseWriter, r *http.Request) {
	line := CanonicalLine(r.PathValue("id"))
	now := a.now()
	a.mu.RLock()
	known := a.known[line]
	a.mu.RUnlock()
	if !known {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_line"})
		return
	}
	a.sched.Watch(line)
	next, polled := a.sched.NextUpdate(line)
	// Read the data after the estimate: a poll published in between then shows up here,
	// instead of an old payload cached until the poll after it.
	a.mu.RLock()
	warming, d := a.warming[line], a.lines[line]
	a.mu.RUnlock()
	if warming {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	if !polled {
		// No scheduled service now: not polled. Recent results (a run ending) still show.
		resp := LineResponse{Line: line, UpdatedAt: now.Unix(), NextUpdateAt: now.Unix() + inactiveMaxAge, Vehicles: []Vehicle{}}
		if d != nil && now.Sub(d.updated) <= lineKeep {
			resp.UpdatedAt, resp.Vehicles = d.updated.Unix(), d.vehicles
		}
		writeJSON(w, http.StatusOK, inactiveMaxAge, resp)
		return
	}
	if d == nil {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	vehicles := d.vehicles
	if vehicles == nil {
		vehicles = []Vehicle{}
	}
	maxAge := clamp(int64(next.Sub(now)/time.Second), minMaxAge, maxMaxAge)
	writeJSON(w, http.StatusOK, int(maxAge), LineResponse{Line: line, UpdatedAt: d.updated.Unix(),
		NextUpdateAt: max(next.Unix(), now.Unix()+minMaxAge), Vehicles: vehicles})
}

// Status is the /v1/status body.
func (a *App) Status() StatusResponse {
	now := a.now()
	a.mu.RLock()
	lastOK, lastPublish, active, loaded, warming := a.lastOK, a.lastPublish, a.active, a.w != nil, len(a.warming)
	a.mu.RUnlock()
	st := a.sched.Stats()
	polledLines := st.LinesByTier[sched.Watched] + st.LinesByTier[sched.Dense] + st.LinesByTier[sched.Other]
	// Not ok: no good poll for 5 minutes while lines are polled, or scheduled lines whose
	// routes OASA has not given us yet while nothing is polled.
	ok := loaded && (polledLines > 0 || warming == 0) &&
		(polledLines == 0 || (!lastOK.IsZero() && now.Sub(lastOK) <= okRecentPollSecs*time.Second))
	var updated int64
	if !lastPublish.IsZero() {
		updated = lastPublish.Unix()
	}
	return StatusResponse{OK: ok, GTFSVersion: a.gtfsVersion(), GTFSExpires: a.gtfsExpires(), UpdatedAt: updated,
		LinesActive: active, BudgetRPS: a.pacer.BaseRPS()}
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, statusMaxAge, a.Status())
}

func (a *App) handleGTFSRT(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch name {
	case "vehicle_positions.pb", "trip_updates.pb", "vehicle_positions.json", "trip_updates.json":
	default:
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "not_found"})
		return
	}
	b, ok := a.gtfsRT(name)
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"feed_unavailable"}`))
		return
	}
	if name[len(name)-3:] == ".pb" {
		w.Header().Set("Content-Type", "application/x-protobuf")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", gtfsRTMaxAge))
	w.Write(b)
}
