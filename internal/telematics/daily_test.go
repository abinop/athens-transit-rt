package telematics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// A trimmed getDailySchedule answer of line 608 (code 960): "go" rows depart from the start
// (sde_start1), "come" rows from the far end (sde_start2); a null start is no departure.
const dailyFixture = `{"go":[
 {"line_id":"608","sdd_code":"3","sdc_code":"60","sdd_line1":"960","sde_start1":"1900-01-01 05:30:00","sde_start2":"1900-01-01 06:20:00","line_circle":"0","line_descr":"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ"},
 {"line_id":"608","sdd_code":"1","sdc_code":"60","sdd_line1":"960","sde_start1":"1900-01-01 05:00:00","sde_start2":"1900-01-01 00:00:00","line_circle":"0","line_descr":"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ"},
 {"line_id":"608","sdd_code":"9","sdc_code":"60","sdd_line1":"960","sde_start1":"1900-01-01 00:15:00","sde_start2":null,"line_circle":"0","line_descr":"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ"},
 {"line_id":"608","sdd_code":"8","sdc_code":"60","sdd_line1":"960","sde_start1":null,"sde_start2":"1900-01-01 23:00:00","line_circle":"0","line_descr":"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ"}
],"come":[
 {"line_id":"608","sdd_code":"5","sdc_code":"60","sdd_line2":"960","sde_start1":null,"sde_start2":"1900-01-01 05:55:00","line_circle":"0","line_descr":"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ"},
 {"line_id":"608","sdd_code":"6","sdc_code":"60","sdd_line2":"960","sde_start1":"1900-01-01 04:00:00","sde_start2":null,"line_circle":"0","line_descr":"ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ"}
]}`

func TestDailySchedule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("act") != "getDailySchedule" || q.Get("line_code") != "960" {
			http.Error(w, "bad query "+r.URL.RawQuery, 400)
			return
		}
		w.Write([]byte(dailyFixture))
	}))
	defer srv.Close()
	got, err := New(srv.URL, nil).DailySchedule(context.Background(), "960")
	if err != nil {
		t.Fatal(err)
	}
	want := DailySchedule{
		Descr:    "ΓΑΛΑΤΣΙ - ΑΚΑΔΗΜΙΑ - ΝΕΚΡ. ΖΩΓΡΑΦΟΥ",
		Circular: false,
		// Sorted; a run after midnight belongs to the evening before ("24:15", GTFS style).
		Go:   []string{"05:00", "05:30", "24:15"},
		Come: []string{"05:55"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestDailyScheduleEmptyAndCircular(t *testing.T) {
	answers := map[string]string{
		"1": `{"go":[],"come":[]}`,
		"2": `{"go":[{"sde_start1":"1900-01-01 07:30:00","line_circle":"1","line_descr":"ΠΛΑΤΕΙΑ ΚΑΝΙΓΓΟΣ - ΓΚΥΖH (ΚΥΚΛΙΚΗ)"}],"come":null}`,
		"3": ``,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(answers[r.URL.Query().Get("line_code")]))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	for code, want := range map[string]DailySchedule{
		"1": {},
		"2": {Descr: "ΠΛΑΤΕΙΑ ΚΑΝΙΓΓΟΣ - ΓΚΥΖH (ΚΥΚΛΙΚΗ)", Circular: true, Go: []string{"07:30"}},
		"3": {},
	} {
		got, err := c.DailySchedule(context.Background(), code)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v want %+v", code, got, want)
		}
	}
}

func TestDailyScheduleHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", 503)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, nil).DailySchedule(context.Background(), "960"); err == nil {
		t.Fatal("503 accepted")
	}
}
