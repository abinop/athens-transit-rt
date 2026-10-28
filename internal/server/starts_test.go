package server

import (
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

var feedA, feedB = &gtfs.Feed{}, &gtfs.Feed{}

func at(hhmm string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", "2026-10-28 "+hhmm, gtfs.Athens)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRunStartFromNearToAway(t *testing.T) {
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 40, 9000, at("10:00"))
	rs.observe(feedA, "v1", "040", 3, 120, 9000, at("10:02")) // still at the start (waiting, creeping)
	rs.observe(feedA, "v1", "040", 3, 900, 9000, at("10:05"))
	rs.observe(feedA, "v1", "040", 3, 2500, 9000, at("10:09")) // the same run: no second start
	got, _ := rs.of(feedA, "040", 3, at("12:00"))
	if len(got) != 1 || !got[0].Equal(at("10:02")) {
		t.Fatalf("starts %v", got)
	}
}

func TestRunStartNeedsTheStart(t *testing.T) {
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 900, 9000, at("10:05")) // first seen mid-run: when it left is unknown
	rs.observe(feedA, "v1", "040", 3, 2500, 9000, at("10:09"))
	rs.observe(feedA, "v2", "040", 3, 50, 9000, at("10:00"))
	rs.observe(feedA, "v2", "040", 3, 900, 9000, at("10:40")) // too long apart to be one departure
	if got, _ := rs.of(feedA, "040", 3, at("12:00")); len(got) != 0 {
		t.Fatalf("starts %v", got)
	}
}

func TestRunStartNextRunAndOtherShape(t *testing.T) {
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 50, 9000, at("10:00"))
	rs.observe(feedA, "v1", "040", 3, 900, 9000, at("10:03"))
	rs.observe(feedA, "v1", "040", 4, 30, 9000, at("11:00")) // back: the other direction
	rs.observe(feedA, "v1", "040", 4, 700, 9000, at("11:04"))
	rs.observe(feedA, "v1", "040", 3, 10, 9000, at("12:00")) // and its next run of the first one
	rs.observe(feedA, "v1", "040", 3, 800, 9000, at("12:02"))
	a, _ := rs.of(feedA, "040", 3, at("13:00"))
	b, _ := rs.of(feedA, "040", 4, at("13:00"))
	if len(a) != 2 || !a[1].Equal(at("12:00")) || len(b) != 1 || !b[0].Equal(at("11:00")) {
		t.Fatalf("shape 3 %v shape 4 %v", a, b)
	}
}

func TestRunStartsOfToday(t *testing.T) {
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 50, 9000, at("10:00"))
	rs.observe(feedA, "v1", "040", 3, 900, 9000, at("10:03"))
	next := at("10:00").AddDate(0, 0, 1)
	if got, _ := rs.of(feedA, "040", 3, next); len(got) != 0 {
		t.Fatalf("yesterday's starts: %v", got)
	}
	rs.observe(feedA, "v2", "040", 3, 50, 9000, next)
	if _, since := rs.of(feedA, "040", 3, next); !since.Equal(next) {
		t.Fatalf("watched since %v", since)
	}
}

func TestRunStartLoopLine(t *testing.T) {
	// A loop: its start and its end are the same place, so a bus waiting there may be put at the end.
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 4900, 5000, at("10:00"))
	rs.observe(feedA, "v1", "040", 3, 4950, 5000, at("10:02"))
	rs.observe(feedA, "v1", "040", 3, 700, 5000, at("10:05"))
	// A bus turning back at the end on the same shape did not start a run.
	rs.observe(feedA, "v2", "040", 3, 4900, 5000, at("10:00"))
	rs.observe(feedA, "v2", "040", 3, 4200, 5000, at("10:03"))
	got, _ := rs.of(feedA, "040", 3, at("12:00"))
	if len(got) != 1 || !got[0].Equal(at("10:02")) {
		t.Fatalf("starts %v", got)
	}
}

func TestRunStartShortShape(t *testing.T) {
	// 1200 m: 1000 m along is both away from the start and near the end. A bus that left counts.
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 50, 1200, at("10:00"))
	rs.observe(feedA, "v1", "040", 3, 1000, 1200, at("10:03"))
	if got, _ := rs.of(feedA, "040", 3, at("12:00")); len(got) != 1 {
		t.Fatalf("starts %v", got)
	}
}

func TestAtTerminus(t *testing.T) {
	ends := []geo.Cand{{Along: 20}, {Along: 4980}}
	if !atTerminus(ends, 5000) || atTerminus([]geo.Cand{{Along: 20}, {Along: 2500}}, 5000) || atTerminus(nil, 5000) || atTerminus([]geo.Cand{{Along: 4980}}, 5000) {
		t.Fatal("terminus")
	}
}

func TestRunStartsLateFixOfYesterday(t *testing.T) {
	rs := newRunStarts()
	today := at("00:05").AddDate(0, 0, 1)
	rs.observe(feedA, "v1", "040", 3, 50, 9000, today)
	rs.observe(feedA, "v1", "040", 3, 900, 9000, today.Add(3*time.Minute))
	rs.observe(feedA, "v2", "040", 3, 50, 9000, at("23:59")) // an old fix, polled late
	if got, since := rs.of(feedA, "040", 3, today.Add(time.Hour)); len(got) != 1 || !since.Equal(today) {
		t.Fatalf("starts %v since %v", got, since)
	}
}

func TestRunStartOldFixOfTheVehicle(t *testing.T) {
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 50, 9000, at("10:00"))
	rs.observe(feedA, "v1", "040", 3, 900, 9000, at("10:03"))
	rs.observe(feedA, "v1", "040", 3, 50, 9000, at("10:01")) // out of order: not at the start again
	rs.observe(feedA, "v1", "040", 3, 1500, 9000, at("10:04"))
	if got, _ := rs.of(feedA, "040", 3, at("12:00")); len(got) != 1 {
		t.Fatalf("starts %v", got)
	}
}

func TestRunStartsOfAnotherFeed(t *testing.T) {
	rs := newRunStarts()
	rs.observe(feedA, "v1", "040", 3, 50, 9000, at("10:00"))
	rs.observe(feedA, "v1", "040", 3, 900, 9000, at("10:03"))
	if got, _ := rs.of(feedB, "040", 3, at("12:00")); len(got) != 0 {
		t.Fatalf("shape 3 of another feed: %v", got)
	}
	rs.observe(feedB, "v2", "040", 3, 50, 9000, at("11:00"))
	if got, _ := rs.of(feedB, "040", 3, at("12:00")); len(got) != 0 {
		t.Fatalf("the old feed's starts: %v", got)
	}
}

func TestMarkSeenRightAfterTheStart(t *testing.T) {
	// Watched since 10:00: the 10:01 run left at 10:01 (seen); 10:02 had no bus, but one may have
	// left early, before 10:00: unknown.
	d := deps("10:01", "10:02")
	markSeen(d, at("00:00"), []time.Time{at("10:01")}, at("10:00"), at("11:00"))
	if got := seenOf(d); !equal(got, []string{"y", "?"}) {
		t.Fatalf("seen %v", got)
	}
}

func seenOf(deps []ScheduleDeparture) []string {
	out := make([]string, len(deps))
	for i, d := range deps {
		switch {
		case d.Seen == nil:
			out[i] = "?"
		case *d.Seen:
			out[i] = "y"
		default:
			out[i] = "n"
		}
	}
	return out
}

func deps(ts ...string) []ScheduleDeparture {
	out := make([]ScheduleDeparture, len(ts))
	for i, t := range ts {
		out[i] = ScheduleDeparture{T: t}
	}
	return out
}

func TestMarkSeen(t *testing.T) {
	day := at("00:00")
	d := deps("08:30", "10:00", "10:20", "10:40", "11:00", "12:00")
	starts := []time.Time{at("09:58"), at("10:24")}
	markSeen(d, day, starts, at("09:00"), at("11:10"))
	// 08:30: before the server watched; 10:00 left 2′ early; 10:20 4′ late; 10:40 no bus and 30′
	// gone; 11:00 only 10′ gone (may still leave); 12:00 to come.
	want := []string{"?", "y", "y", "n", "?", "?"}
	if got := seenOf(d); !equal(got, want) {
		t.Fatalf("seen %v, want %v", got, want)
	}
}

func TestMarkSeenOneStartOneRun(t *testing.T) {
	d := deps("10:00", "10:05")
	markSeen(d, at("00:00"), []time.Time{at("10:04")}, at("09:00"), at("11:00"))
	if got := seenOf(d); !equal(got, []string{"y", "n"}) {
		t.Fatalf("seen %v", got)
	}
}

func TestMarkSeenPastMidnight(t *testing.T) {
	d := deps("24:10")
	markSeen(d, at("00:00"), []time.Time{at("00:00").Add(24*time.Hour + 11*time.Minute)}, at("09:00"), at("00:00").Add(25*time.Hour))
	if got := seenOf(d); !equal(got, []string{"y"}) {
		t.Fatalf("seen %v", got)
	}
}

func TestEndMatches(t *testing.T) {
	for _, c := range []struct {
		gtfs, daily string
		want        bool
	}{
		{"ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ", "ΣΥΝΤΑΓΜΑ", true},
		{"ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ", "ΠΕΙΡΑΙΑΣ", false},
		{"Πειραιάς - Σύνταγμα (Προσωρινή λόγω έργων)", "ΣΥΝΤΑΓΜΑ", true},
		{"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ", "ΖΩΓΡΑΦΟΥ", true},
		{"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ", "ΓΑΛΑΤΣΙ", false},
		{"ΣΤ. ΚΑΤΩ ΠΑΤΗΣΙΑ", "ΣΤ. ΚΑΤΩ ΠΑΤΗΣΙΑ (ΚΥΚΛΙΚΗ)", true},
	} {
		if got := endMatches(c.gtfs, c.daily); got != c.want {
			t.Errorf("endMatches(%q, %q) = %v", c.gtfs, c.daily, got)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
