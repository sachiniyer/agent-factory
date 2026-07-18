package schedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

// parseExpect overrides the default ParseCron expectation for a vector. Any
// field left nil falls back to the vector's own cron/schedule (see the schema
// note in testdata/vectors.json).
type parseExpect struct {
	Cron     *string   `json:"cron"`
	OK       *bool     `json:"ok"`
	Schedule *Schedule `json:"schedule"`
}

// vector is one shared test triple loaded from testdata/vectors.json. Pointer
// fields distinguish "absent" from a zero value so the loader knows which
// assertions apply.
type vector struct {
	Name     string       `json:"name"`
	Schedule *Schedule    `json:"schedule"`
	Cron     *string      `json:"cron"`
	Human    *string      `json:"human"`
	Parse    *parseExpect `json:"parse"`
}

type vectorFile struct {
	Vectors []vector `json:"vectors"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(data, &vf); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(vf.Vectors) == 0 {
		t.Fatal("no vectors loaded")
	}
	return vf.Vectors
}

// TestVectors drives the shared vector file: it is the contract the phase-2 web
// implementation must also satisfy. Every entry checks Cron(), Describe(),
// and/or ParseCron() per the rules documented in the JSON header.
func TestVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			if v.Schedule != nil && v.Cron != nil {
				if got := v.Schedule.Cron(); got != *v.Cron {
					t.Errorf("Cron() = %q, want %q", got, *v.Cron)
				}
			}
			if v.Schedule != nil && v.Human != nil {
				if got := v.Schedule.Describe(); got != *v.Human {
					t.Errorf("Describe() = %q, want %q", got, *v.Human)
				}
			}

			// ParseCron: input and expectations default to the vector, with the
			// optional parse block overriding.
			input, hasInput := "", false
			switch {
			case v.Parse != nil && v.Parse.Cron != nil:
				input, hasInput = *v.Parse.Cron, true
			case v.Cron != nil:
				input, hasInput = *v.Cron, true
			}
			if !hasInput {
				return
			}
			expectOK := true
			if v.Parse != nil && v.Parse.OK != nil {
				expectOK = *v.Parse.OK
			}
			var expected Schedule
			haveExpected := false
			switch {
			case v.Parse != nil && v.Parse.Schedule != nil:
				expected, haveExpected = *v.Parse.Schedule, true
			case v.Schedule != nil:
				expected, haveExpected = *v.Schedule, true
			}

			got, ok := ParseCron(input)
			if ok != expectOK {
				t.Errorf("ParseCron(%q) ok = %v, want %v", input, ok, expectOK)
			}
			if haveExpected && !reflect.DeepEqual(got, expected) {
				t.Errorf("ParseCron(%q) = %+v, want %+v", input, got, expected)
			}
		})
	}
}

// TestGeneratedCronValidatesThroughDaemon is the round-trip guard the issue
// calls for: every preset cron the picker can generate must pass the daemon's
// existing validator (task.ValidateCronExpr) and be schedulable by its parser
// (task.ParseCron). This is what lets the picker replace the raw-cron field
// without touching how cron is validated or persisted.
func TestGeneratedCronValidatesThroughDaemon(t *testing.T) {
	for _, v := range loadVectors(t) {
		if v.Schedule == nil || v.Schedule.Type == Custom {
			continue // custom carries arbitrary user text; only presets are guaranteed valid
		}
		expr := v.Schedule.Cron()
		t.Run(v.Name+"/"+expr, func(t *testing.T) {
			if err := task.ValidateCronExpr(expr); err != nil {
				t.Errorf("ValidateCronExpr(%q) = %v, want nil", expr, err)
			}
			if _, err := task.ParseCron(expr); err != nil {
				t.Errorf("task.ParseCron(%q) = %v, want nil", expr, err)
			}
		})
	}
}

// TestParseCronRoundTrip asserts the structural round-trip for every preset:
// ParseCron(s.Cron()) reproduces s with ok=true. This is the property that lets
// an existing task re-open as its matching preset in the picker.
func TestParseCronRoundTrip(t *testing.T) {
	presets := []Schedule{
		{Type: EveryNMinutes, Interval: 1},
		{Type: EveryNMinutes, Interval: 45},
		{Type: EveryNHours, Interval: 1},
		{Type: EveryNHours, Interval: 8},
		{Type: Hourly, Minute: 0},
		{Type: Hourly, Minute: 17},
		{Type: Daily, Hour: 0, Minute: 0},
		{Type: Daily, Hour: 23, Minute: 59},
		{Type: Weekly, Hour: 9, Minute: 30, Weekdays: []time.Weekday{time.Monday}},
		{Type: Weekly, Hour: 7, Minute: 0, Weekdays: []time.Weekday{time.Sunday, time.Saturday}},
		{Type: Monthly, Hour: 12, Minute: 0, DayOfMonth: 1},
		{Type: Monthly, Hour: 6, Minute: 15, DayOfMonth: 28},
	}
	for _, s := range presets {
		expr := s.Cron()
		got, ok := ParseCron(expr)
		if !ok {
			t.Errorf("ParseCron(%q) ok=false, want a recognized preset", expr)
		}
		if !reflect.DeepEqual(got, s) {
			t.Errorf("ParseCron(%q) = %+v, want %+v", expr, got, s)
		}
	}
}

// TestParseCronUnrecognizedFallsBackToCustom pins the best-effort contract: an
// expression that is not one of the emitted shapes returns {Custom, Raw} with
// ok=false so the UI knows to drop into the raw-cron editor.
func TestParseCronUnrecognizedFallsBackToCustom(t *testing.T) {
	for _, expr := range []string{
		"0 9 * * 1-5",    // weekday range (we emit comma lists)
		"*/7 9-17 * * *", // hour range
		"15 */3 * * *",   // minute + hour-step combination we never emit
		"0 0 1,15 * *",   // day-of-month list
		"0 0 * JAN *",    // named month
		"@daily",         // descriptor
		"0 0",            // too few fields
	} {
		got, ok := ParseCron(expr)
		if ok {
			t.Errorf("ParseCron(%q) ok=true, want false", expr)
		}
		if got.Type != Custom || got.Raw != expr {
			t.Errorf("ParseCron(%q) = %+v, want {Custom, Raw:%q}", expr, got, expr)
		}
	}
}

// TestWeekdayOrderIsSundayFirstAndDeduped verifies the day-of-week field and
// description are normalized (deduped, Sunday-first) regardless of input order.
func TestWeekdayOrderIsSundayFirstAndDeduped(t *testing.T) {
	s := Schedule{
		Type:     Weekly,
		Hour:     9,
		Minute:   0,
		Weekdays: []time.Weekday{time.Wednesday, time.Sunday, time.Monday, time.Wednesday},
	}
	if got, want := s.Cron(), "0 9 * * 0,1,3"; got != want {
		t.Errorf("Cron() = %q, want %q", got, want)
	}
	if got, want := s.Describe(), "Every week on Sun, Mon, Wed at 9:00 AM"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}
