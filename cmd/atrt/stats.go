package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/release"
	"github.com/angelospk/athens-transit-rt/internal/stats"
)

// stats: sum the fix history into daily statistics once (atrt serve --history does it hourly).
func statsCmd(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	hist := fs.String("history", "/var/lib/atrt/history", "fix history directory")
	out := fs.String("out", "/var/lib/atrt/stats", "output directory (days/<date>.json, index.json)")
	gtfsPath := fs.String("gtfs", "", "GTFS zip for the trips section (default: the snapshot in -state)")
	state := fs.String("state", "/var/lib/atrt", "state directory with the GTFS snapshot")
	force := fs.Bool("force", false, "rewrite days that already have a file")
	fs.Parse(args)
	loc, err := time.LoadLocation(stats.TZ)
	if err != nil {
		return err
	}
	feed, version, err := statsFeed(*gtfsPath, *state)
	if err != nil {
		return err
	}
	var plan stats.PlanFunc
	if feed == nil {
		fmt.Fprintln(os.Stderr, "stats: no GTFS feed found (-gtfs or -state): days get no trips section")
	} else {
		plan = func(date string) (*stats.Plan, error) { return stats.NewPlan(feed, version, date, loc) }
	}
	days, err := stats.Run(*hist, *out, loc, time.Now(), *force, plan)
	for _, d := range days {
		fmt.Println("wrote", d)
	}
	return err
}

// statsFeed loads the GTFS feed for the trips section and its version: a zip when given, else
// the snapshot saved in the state directory (nil feed when there is none).
func statsFeed(zip, state string) (*gtfs.Feed, string, error) {
	if zip != "" {
		f, err := gtfs.LoadZip(zip, gtfs.LoadOptions{})
		if err != nil {
			return nil, "", err
		}
		return f, f.Version(), nil
	}
	f, m, err := (&release.Fetcher{Dir: state}).LoadLocal()
	if err != nil || f == nil {
		return nil, "", err
	}
	if m != nil && m.GTFSVersion != "" {
		return f, m.GTFSVersion, nil
	}
	return f, f.Version(), nil
}
