package telematics

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DailySchedule is OASA's timetable of today for one line code (getDailySchedule): the first
// departure of every run, "HH:MM" sorted, past midnight GTFS style ("24:15"). Go runs start at
// the line's first end, Come runs at the other one; either list may be empty.
type DailySchedule struct {
	Descr    string // "A - B - C"
	Circular bool
	Go, Come []string
}

// DailySchedule asks OASA for today's runs of a line code. An empty answer is "no data": the
// zero value and no error.
func (c *Client) DailySchedule(ctx context.Context, lineCode string) (DailySchedule, error) {
	const act = "getDailySchedule"
	if c.Pacer != nil {
		if err := c.Pacer.Wait(ctx); err != nil {
			return DailySchedule{}, err
		}
	}
	var raw struct {
		Go []struct {
			Start1 str `json:"sde_start1"`
			Circle str `json:"line_circle"`
			Descr  str `json:"line_descr"`
		} `json:"go"`
		Come []struct {
			Start2 str `json:"sde_start2"`
			Circle str `json:"line_circle"`
			Descr  str `json:"line_descr"`
		} `json:"come"`
	}
	start := time.Now()
	err := c.do(ctx, &raw, act, c.BaseURL+"?act="+act+"&line_code="+url.QueryEscape(lineCode))
	if err != nil {
		c.errors.Add(1)
	}
	if c.Pacer != nil {
		c.Pacer.Report(time.Since(start), err)
	}
	if err != nil {
		return DailySchedule{}, err
	}
	var d DailySchedule
	note := func(descr, circle str) {
		if d.Descr == "" {
			d.Descr = string(descr)
		}
		if circle == "1" {
			d.Circular = true
		}
	}
	for _, r := range raw.Go {
		note(r.Descr, r.Circle)
		if t, ok := dailyTime(string(r.Start1)); ok {
			d.Go = append(d.Go, t)
		}
	}
	for _, r := range raw.Come {
		note(r.Descr, r.Circle)
		if t, ok := dailyTime(string(r.Start2)); ok {
			d.Come = append(d.Come, t)
		}
	}
	sort.Strings(d.Go)
	sort.Strings(d.Come)
	return d, nil
}

// dailyTime turns "1900-01-01 05:30:00" into "05:30"; the small hours (before 03:00) are the
// end of the service day: "00:15" is "24:15".
func dailyTime(s string) (string, bool) {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[i+1:]
	}
	if len(s) < 5 || s[2] != ':' {
		return "", false
	}
	h, err := strconv.Atoi(s[:2])
	if err != nil {
		return "", false
	}
	if h < 3 {
		h += 24
	}
	return fmt.Sprintf("%02d%s", h, s[2:5]), true
}
