package rail

import (
	"reflect"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

func at(hh, mm int, day time.Time) int64 {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, gtfs.Athens).Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute).Unix()
}

func TestSTASYVehicles(t *testing.T) {
	f := testSTASY(t)
	om := STASYPlace{Name: "ΟΜΟΝΟΙΑ", Lat: 37.984, Lon: 23.728}
	place := func(p STASYPlace, unix int64) STASYPlace { p.At = unix; return p }
	el := STASYPlace{Name: "ΕΛΛΗΝΙΚΟ", Lat: 37.890, Lon: 23.745}
	an := STASYPlace{Name: "ΑΝΘΟΥΠΟΛΗ", Lat: 38.020, Lon: 23.790}
	p1 := STASYPlace{Name: "ΣΥΝΤΑΓΜΑ", Lat: 37.975, Lon: 23.735}
	d := stasyNow

	t.Run("one train between two stops, with their times", func(t *testing.T) {
		got := f.Vehicles(d)
		want := []STASYVehicle{{Trip: "M2-3", Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ",
			From: place(om, at(9, 52, d)), To: place(el, at(10, 9, d))}} // left at its departure, due at its arrival
		if got.Indicative || !reflect.DeepEqual(got.Vehicles, want) {
			t.Fatalf("got %+v", got)
		}
		if got.UpdatedAt != d.Unix() {
			t.Errorf("updated_at = %d", got.UpdatedAt)
		}
	})

	t.Run("a train standing at a stop between its arrival and its departure", func(t *testing.T) {
		now := d.Add(-8*time.Minute - 30*time.Second) // 09:51:30
		for _, v := range f.Vehicles(now).Vehicles {
			if v.Trip == "M2-3" {
				if !reflect.DeepEqual(v.From, place(om, at(9, 51, d))) || !reflect.DeepEqual(v.To, place(om, at(9, 52, d))) {
					t.Fatalf("M2-3 = %+v", v)
				}
				return
			}
		}
		t.Fatal("M2-3 not listed")
	})

	t.Run("a train is gone once it arrives at its last stop", func(t *testing.T) {
		for _, v := range f.Vehicles(d.Add(9*time.Minute + 30*time.Second)).Vehicles {
			if v.Trip == "M2-3" {
				t.Fatalf("M2-3 still listed: %+v", v)
			}
		}
	})

	t.Run("every running trip of the day's services, sorted by line and trip", func(t *testing.T) {
		now := d.Add(5 * time.Minute) // 10:05
		var trips []string
		for _, v := range f.Vehicles(now).Vehicles {
			trips = append(trips, v.Trip)
		}
		// M2-2 starts at 10:07; M2-5 is a weekend trip.
		if want := []string{"M1-1", "M2-1", "M2-3", "M2-4"}; !reflect.DeepEqual(trips, want) {
			t.Fatalf("trips = %v, want %v", trips, want)
		}
		for _, v := range f.Vehicles(now).Vehicles {
			if v.Trip == "M2-4" && (!reflect.DeepEqual(v.From, place(om, at(10, 4, d))) || !reflect.DeepEqual(v.To, place(an, at(10, 21, d)))) {
				t.Errorf("M2-4 = %+v", v)
			}
			if v.Trip == "M1-1" && (v.Line != "M1" || v.From.Name != p1.Name || v.To.Name != "ΟΜΟΝΟΙΑ") {
				t.Errorf("M1-1 = %+v", v)
			}
		}
	})

	t.Run("a trip of yesterday past midnight", func(t *testing.T) {
		now := time.Date(2026, 10, 29, 0, 31, 0, 0, gtfs.Athens)
		got := f.Vehicles(now).Vehicles
		if len(got) != 1 || got[0].Trip != "M2-6" || got[0].From.At != at(24, 30, d) || got[0].To.At != at(24, 32, d) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("an expired feed is indicative", func(t *testing.T) {
		got := f.Vehicles(stasyWed)
		if !got.Indicative || len(got.Vehicles) != 1 || got.Vehicles[0].Trip != "M2-3" || got.Vehicles[0].From.At != at(9, 52, stasyWed) {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestSTASYTrip(t *testing.T) {
	f := testSTASY(t)
	d := stasyNow
	got, ok := f.Trip("M2-1", d)
	if !ok {
		t.Fatal("no trip")
	}
	want := &STASYTrip{Trip: "M2-1", Line: "M2", RouteColor: "#e30613", Headsign: "ΕΛΛΗΝΙΚΟ", Stops: []STASYPlace{
		{Name: "ΣΥΝΤΑΓΜΑ", Lat: 37.975, Lon: 23.735, At: at(10, 1, d)},
		{Name: "ΟΜΟΝΟΙΑ", Lat: 37.984, Lon: 23.728, At: at(10, 3, d)},
		{Name: "ΕΛΛΗΝΙΚΟ", Lat: 37.890, Lon: 23.745, At: at(10, 20, d)},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	// Past midnight, yesterday's overnight trip keeps yesterday's day.
	got, ok = f.Trip("M2-6", time.Date(2026, 10, 29, 0, 31, 0, 0, gtfs.Athens))
	if !ok || got.Stops[0].At != at(24, 30, d) {
		t.Fatalf("overnight: %+v", got)
	}
	if _, ok := f.Trip("nope", d); ok {
		t.Error("unknown trip found")
	}
	if _, ok := f.Trip("M2-5", d); ok { // a weekend trip on a Wednesday
		t.Error("a trip that does not run today found")
	}
}
