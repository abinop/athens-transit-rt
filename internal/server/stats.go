package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/stats"
)

const (
	statsIndexMaxAge = 600
	statsDayMaxAge   = 86400 // a day file never changes once written
)

func (a *App) statsDir() string { return filepath.Join(a.cfg.StateDir, "stats") }

// dailyStats sums finished days of the fix history (internal/stats), at start and then hourly.
func (a *App) dailyStats(ctx context.Context) {
	loc, err := time.LoadLocation(stats.TZ)
	if err != nil {
		a.log.Warn("stats: no time zone", "err", err)
		return
	}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		days, err := stats.Run(filepath.Join(a.cfg.StateDir, "history"), a.statsDir(), loc, a.now(), false, a.statsPlan)
		if len(days) > 0 {
			a.log.Info("stats: wrote days", "days", days)
		}
		if err != nil {
			a.log.Warn("stats", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// statsPlan is the schedule of a local day from the feed in use. It builds its own matcher: the
// world's is not safe for use from this goroutine.
func (a *App) statsPlan(date string) (*stats.Plan, error) {
	w := a.world()
	if w == nil {
		return nil, errors.New("no static feed yet")
	}
	loc, err := time.LoadLocation(stats.TZ)
	if err != nil {
		return nil, err
	}
	return stats.NewPlan(w.feed, worldVersion(w), date, loc)
}

// GET /v1/stats: the list of days with statistics.
func (a *App) handleStatsIndex(w http.ResponseWriter, r *http.Request) {
	a.serveStatsFile(w, filepath.Join(a.statsDir(), "index.json"), statsIndexMaxAge)
}

// GET /v1/stats/days/{date}: one day (YYYY-MM-DD, Europe/Athens).
func (a *App) handleStatsDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if _, _, err := stats.Bounds(date, time.UTC); err != nil {
		writeJSON(w, http.StatusBadRequest, unknownMaxAge, map[string]string{"error": "bad_date"})
		return
	}
	a.serveStatsFile(w, filepath.Join(a.statsDir(), "days", date+".json"), statsDayMaxAge)
}

func (a *App) serveStatsFile(w http.ResponseWriter, path string, maxAge int) {
	b, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "not_found"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(maxAge))
	w.Write(b)
}
