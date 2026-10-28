package server

import (
	"errors"
	"net/http"
	"sort"
	"time"
)

// staleArrivalsMax: when OASA or the request queue cannot answer now, a stop's last good
// predictions are served up to this old (with their own updated_at) instead of an error.
const staleArrivalsMax = 2 * time.Minute

// PredictedArrival is one of OASA's own predictions for a stop (getStopArrivals).
type PredictedArrival struct {
	Line      string `json:"line"` // "" when the route code is not in OASA's line list
	RouteCode string `json:"route_code"`
	Vehicle   string `json:"vehicle"`
	Minutes   int    `json:"minutes"`
}

type StopArrivalsResponse struct {
	StopID    string             `json:"stop_id"`
	StopName  string             `json:"stop_name"`
	UpdatedAt int64              `json:"updated_at"`
	Arrivals  []PredictedArrival `json:"arrivals"`
}

// GET /v1/stops/{id}/arrivals: OASA's own predictions for a stop (getStopArrivals), soonest
// first. They take background slots of the shared OASA budget (at most one in bgEvery while
// lines are due), cached stopArrivalsTTL per stop; Cache-Control lasts until that cache expires.
func (a *App) handleStopArrivals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wd := a.world()
	if wd == nil {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	idx := wd.feed.StopIndex(id)
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_stop"})
		return
	}
	e, err := a.arrivals(r.Context(), id)
	maxAge := int(clamp(int64((stopArrivalsTTL-a.now().Sub(e.at))/time.Second), minMaxAge, int64(stopArrivalsTTL/time.Second)))
	if err != nil {
		stale, ok := a.cachedArrivals(id, staleArrivalsMax)
		switch {
		case ok:
			e, maxAge = stale, minMaxAge
		case errors.Is(err, errQueueFull):
			writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "busy"})
			return
		default:
			writeJSON(w, http.StatusBadGateway, minMaxAge, map[string]string{"error": "upstream_unavailable"})
			return
		}
	}
	lineOf := a.routeLines()
	out := make([]PredictedArrival, 0, len(e.list))
	for _, ar := range e.list {
		out = append(out, PredictedArrival{Line: lineOf[ar.RouteCode], RouteCode: ar.RouteCode, Vehicle: ar.VehCode, Minutes: ar.Minutes})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Minutes < out[j].Minutes })
	writeJSON(w, http.StatusOK, maxAge, StopArrivalsResponse{StopID: id, StopName: wd.feed.Stops[idx].Name,
		UpdatedAt: e.at.Unix(), Arrivals: out})
}
