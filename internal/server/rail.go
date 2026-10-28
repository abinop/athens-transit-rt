package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/rail"
)

const (
	stasyMaxAge      = 30
	stasyVehiclesAge = 15 // clients move the trains between the two stops on their own clock
	stasyTripAge     = 60
	suburbanStations = 3600 // the corridor table changes once in months
	suburbanMaxAge   = 60
	suburbanVehAge   = 5 // the positions come about once a second, 10 s behind
	suburbanTrainAge = 30
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

// GET /v1/rail/stasy/vehicles: the metro/tram trains running now by the timetable (no live data
// exists), each between the stop it left and the next one, with both times.
func (a *App) handleSTASYVehicles(w http.ResponseWriter, r *http.Request) {
	f, ok := a.stasy.Feed()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	writeJSON(w, http.StatusOK, stasyVehiclesAge, f.Vehicles(a.now()))
}

// GET /v1/rail/stasy/trips/{id}: one run of the timetable with every stop and its time.
func (a *App) handleSTASYTrip(w http.ResponseWriter, r *http.Request) {
	f, ok := a.stasy.Feed()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	trip, ok := f.Trip(r.PathValue("id"), a.now())
	if !ok {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_trip"})
		return
	}
	writeJSON(w, http.StatusOK, stasyTripAge, trip)
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
	liveDelays(a.trains.Delays(), dep.Departures)
	writeJSON(w, http.StatusOK, suburbanMaxAge, dep)
}

// liveDelays puts the trains' live delays (the stream, while it is open) over the board's,
// which is up to 10 minutes old.
func liveDelays(live map[string]int, deps []rail.SuburbanDeparture) {
	for i := range deps {
		if d, ok := live[deps[i].Train]; ok {
			deps[i].DelayMin = &d
		}
	}
}

// GET /v1/rail/suburban/vehicles: the Proastiakos trains in service with their GPS positions.
// The first request opens the upstream stream: 503 until its first snapshot (a few seconds).
func (a *App) handleSuburbanVehicles(w http.ResponseWriter, r *http.Request) {
	snap := a.trains.Vehicles()
	if snap == nil || a.now().Sub(time.Unix(snap.UpdatedAt, 0)) > rail.TrainFresh {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	writeJSON(w, http.StatusOK, suburbanVehAge, snap)
}

// GET /v1/rail/suburban/trains/{no}: today's run of a train with every stop and its time.
func (a *App) handleSuburbanTrain(w http.ResponseWriter, r *http.Request) {
	run, found, ok := a.suburban.Train(r.Context(), r.PathValue("no"), a.now())
	switch {
	case !ok:
		writeJSON(w, http.StatusBadGateway, unknownMaxAge, map[string]string{"error": "upstream_unavailable"})
		return
	case !found:
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_train"})
		return
	}
	if d, ok := a.trains.Delays()[run.Train]; ok {
		run.DelayMin = &d
	}
	writeJSON(w, http.StatusOK, suburbanTrainAge, run)
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
