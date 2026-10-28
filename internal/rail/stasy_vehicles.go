package rail

import (
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

// STASYPlace is a stop of a trip with its time (unix seconds): the departure, or the arrival at
// the stop a train is heading to.
type STASYPlace struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	At   int64   `json:"at"`
}

// STASYVehicle is a metro/tram train placed from the timetable (STASY has no live data): the two
// stops it runs between now, with their times, so a client moves it along on its own clock.
type STASYVehicle struct {
	Trip       string     `json:"trip"`
	Line       string     `json:"line"`
	RouteColor string     `json:"route_color"`
	Headsign   string     `json:"headsign"`
	From       STASYPlace `json:"from"`
	To         STASYPlace `json:"to"`
}

// STASYVehicles is the body of the vehicles endpoint.
type STASYVehicles struct {
	UpdatedAt  int64          `json:"updated_at"`
	Indicative bool           `json:"indicative"`
	Vehicles   []STASYVehicle `json:"vehicles"`
}

// STASYTrip is one run with every stop and its time.
type STASYTrip struct {
	Trip       string       `json:"trip"`
	Line       string       `json:"line"`
	RouteColor string       `json:"route_color"`
	Headsign   string       `json:"headsign"`
	Stops      []STASYPlace `json:"stops"`
}

// serviceDay is a day whose trips can run now: the timetable date to read (indicative: another
// date stood in for it) and the instant of its midnight.
type serviceDay struct {
	date       int32
	indicative bool
	midnight   time.Time
}

// serviceDays are today and yesterday, whose trips past midnight ("24:30") still run.
func (s *STASY) serviceDays(now time.Time) [2]serviceDay {
	local := now.In(gtfs.Athens)
	var out [2]serviceDay
	for back := range out {
		noon := time.Date(local.Year(), local.Month(), local.Day()-back, 12, 0, 0, 0, gtfs.Athens)
		date, ind := s.timetableDate(noon)
		out[back] = serviceDay{date, ind, gtfs.ServiceDay(noon)}
	}
	return out
}

func (s *STASY) place(st stasyStopTime, secs int32, midnight time.Time) STASYPlace {
	p := s.Stops[st.Stop]
	return STASYPlace{Name: p.Name, Lat: p.Lat, Lon: p.Lon, At: midnight.Add(time.Duration(secs) * time.Second).Unix()}
}

// Vehicles lists every train running now by the timetable: between the stop it left (at its
// departure) and the next one (at its arrival), or standing at a stop (both places that stop,
// from its arrival to its departure). A train at a terminus is not listed. Sorted by line, then trip.
func (s *STASY) Vehicles(now time.Time) *STASYVehicles {
	out := &STASYVehicles{UpdatedAt: now.Unix(), Vehicles: []STASYVehicle{}}
	for i, d := range s.serviceDays(now) {
		secs := int32(now.Sub(d.midnight) / time.Second)
		used := false
		for ti := range s.trips {
			t := &s.trips[ti]
			rows := s.tripStops[ti]
			if len(rows) < 2 || secs < rows[0].T || secs >= rows[len(rows)-1].A || !s.active(t.Service, d.date) {
				continue
			}
			// The first stop it has not reached yet (j >= 1: it left the first one).
			j := sort.Search(len(rows), func(k int) bool { return k > 0 && rows[k].A > secs })
			v := STASYVehicle{Trip: t.ID, Line: s.line(t.Route), RouteColor: s.color(t.Route), Headsign: t.Headsign}
			if prev := rows[j-1]; secs < prev.T {
				v.From, v.To = s.place(prev, prev.A, d.midnight), s.place(prev, prev.T, d.midnight)
			} else {
				v.From, v.To = s.place(prev, prev.T, d.midnight), s.place(rows[j], rows[j].A, d.midnight)
			}
			used = true
			out.Vehicles = append(out.Vehicles, v)
		}
		// As in Departures: yesterday marks the timetable indicative only when it runs a train now.
		if i == 0 || used {
			out.Indicative = out.Indicative || d.indicative
		}
	}
	sort.Slice(out.Vehicles, func(i, j int) bool {
		a, b := out.Vehicles[i], out.Vehicles[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Trip < b.Trip
	})
	return out
}

// Trip returns a run with its stops and times: yesterday's when it is an overnight run still
// going, else today's. ok is false for an unknown trip or one that does not run on that day.
func (s *STASY) Trip(id string, now time.Time) (*STASYTrip, bool) {
	ti, ok := s.tripIdx[id]
	if !ok || len(s.tripStops[ti]) == 0 {
		return nil, false
	}
	t := &s.trips[ti]
	rows := s.tripStops[ti]
	days := s.serviceDays(now)
	pick := -1
	if y := days[1]; s.active(t.Service, y.date) && int32(now.Sub(y.midnight)/time.Second) <= rows[len(rows)-1].A {
		pick = 1
	} else if s.active(t.Service, days[0].date) {
		pick = 0
	}
	if pick < 0 {
		return nil, false
	}
	out := &STASYTrip{Trip: t.ID, Line: s.line(t.Route), RouteColor: s.color(t.Route), Headsign: t.Headsign,
		Stops: make([]STASYPlace, 0, len(rows))}
	for i, r := range rows {
		at := r.T // the departure; at the last stop, the arrival
		if i == len(rows)-1 {
			at = r.A
		}
		out.Stops = append(out.Stops, s.place(r, at, days[pick].midnight))
	}
	return out, true
}
