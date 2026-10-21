package server

import (
	"context"
	"path/filepath"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
)

const (
	typicalDays  = 14             // typical speeds come from this many days of history
	typicalEvery = 24 * time.Hour // and are rebuilt this often
	// stepFallbackMS is the speed of a bin without history when the vehicle's own speed is
	// unknown; the map uses the same for vehicles without speed.
	stepFallbackMS = 1.5
)

// typicalSpeeds builds the typical-speed table (contract rev 6 `path_speeds`) from the fix
// history at start, then daily, and as soon as the GTFS feed changes (shape ids may get a new
// geometry). A failed build keeps the previous table; vehicles use a table only for its feed.
func (a *App) typicalSpeeds(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	var built time.Time
	for {
		now := a.now()
		if feed := a.gtfsVersion(); feed != "" && (a.segs.Load().Feed() != feed || now.Sub(built) >= typicalEvery) {
			start := time.Now()
			tab, err := segspeed.Build(filepath.Join(a.cfg.StateDir, "history"), gtfs.Athens, now.Add(-typicalDays*24*time.Hour), now, feed)
			if err != nil {
				a.log.Warn("typical speeds", "err", err)
			} else {
				a.segs.Store(tab)
				built = now
				a.log.Info("typical speeds built", "feed", feed, "shapes", tab.Shapes(), "bad_files", tab.BadFiles(),
					"took", time.Since(start).Round(time.Millisecond))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// stepFallback is the speed path_speeds assumes over bins without history.
func stepFallback(speed *float64) float64 {
	if speed != nil {
		return *speed
	}
	return stepFallbackMS
}
