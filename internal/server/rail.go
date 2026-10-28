package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/angelospk/athens-transit-rt/internal/rail"
)

const (
	stasyMaxAge      = 30
	suburbanStations = 3600 // the corridor table changes once in months
	suburbanMaxAge   = 60
)

// GET /v1/rail/stasy/departures?stop=ΣΥΝΤΑΓΜΑ&n=6: next metro/tram departures at a stop, from the
// STASY timetable the server loaded. 503 until the first load finishes.
func (a *App) handleSTASYDepartures(w http.ResponseWriter, r *http.Request) {
	stop := strings.TrimSpace(r.URL.Query().Get("stop"))
	if stop == "" {
		writeJSON(w, http.StatusBadRequest, unknownMaxAge, map[string]string{"error": "missing_stop"})
		return
	}
	f, ok := a.stasy.Feed()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	dep, ok := f.Departures(stop, queryN(r, 6), a.now())
	if !ok {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_stop"})
		return
	}
	writeJSON(w, http.StatusOK, stasyMaxAge, dep)
}

// GET /v1/rail/suburban/stations: the Proastiakos stations, with their corridors.
func (a *App) handleSuburbanStations(w http.ResponseWriter, r *http.Request) {
	list, ok := a.suburban.Stations(r.Context())
	if !ok {
		writeJSON(w, http.StatusBadGateway, unknownMaxAge, map[string]string{"error": "upstream_unavailable"})
		return
	}
	if list == nil {
		list = []rail.Station{}
	}
	writeJSON(w, http.StatusOK, suburbanStations, map[string]any{"stations": list})
}

// GET /v1/rail/suburban/stations/{id}/departures: next 10 trains of a station.
func (a *App) handleSuburbanDepartures(w http.ResponseWriter, r *http.Request) {
	dep, ok := a.suburban.Departures(r.Context(), r.PathValue("id"), queryN(r, 10), a.now())
	if !ok {
		writeJSON(w, http.StatusBadGateway, unknownMaxAge, map[string]string{"error": "upstream_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, suburbanMaxAge, dep)
}

// queryN reads ?n=, with a default and a hard cap so one client cannot ask for a whole day.
func queryN(r *http.Request, def int) int {
	v := r.URL.Query().Get("n")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return min(n, 20)
}
