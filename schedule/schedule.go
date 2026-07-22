// Package schedule is the canonical, UI-agnostic model behind the friendly
// task-schedule picker (#2057). It maps a small set of preset schedule shapes
// (every N minutes/hours, hourly, daily, weekly, monthly) to and from the
// 5-field cron expressions the task store persists, plus a plain-English
// Describe() for the picker preview. The TUI form and the phase-2 web modal
// both drive this model, so the two surfaces cannot disagree about what a
// picker state means — the shared test vectors in testdata/vectors.json (loaded
// by both the Go and the future web tests) pin that contract.
//
// Cron generation is the only exhaustive direction: ParseCron is best-effort
// and recognizes just the shapes Cron emits (so an existing task re-opens as
// its matching preset), falling back to Type=Custom for anything else. The
// daemon's own cron validation and scheduling (task.ValidateCronExpr /
// task.ParseCron) are unchanged — this package only shapes the INPUT, and its
// generated expressions round-trip through that validator (asserted in the
// tests).
package schedule

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Type is the preset schedule shape a Schedule takes.
type Type string

const (
	// EveryNMinutes fires every Interval minutes ("*/N * * * *").
	EveryNMinutes Type = "everyNMinutes"
	// EveryNHours fires every Interval hours on the hour ("0 */N * * *").
	EveryNHours Type = "everyNHours"
	// Hourly fires once an hour at Minute past the hour ("M * * * *").
	Hourly Type = "hourly"
	// Daily fires once a day at Hour:Minute ("M H * * *").
	Daily Type = "daily"
	// Weekly fires at Hour:Minute on each selected weekday ("M H * * <days>").
	Weekly Type = "weekly"
	// Monthly fires at Hour:Minute on DayOfMonth ("M H DOM * *").
	Monthly Type = "monthly"
	// Custom carries a raw cron expression verbatim (the advanced escape hatch).
	Custom Type = "custom"
)

// Schedule is a preset schedule plus its contextual fields. Only the fields
// relevant to Type are meaningful; the rest stay zero. Hour is 24-hour (0-23) —
// the 12-hour AM/PM presentation lives in Describe and the UI, not in the
// model. Weekdays holds time.Weekday values (Sunday=0 … Saturday=6).
type Schedule struct {
	Type       Type           `json:"type"`
	Interval   int            `json:"interval,omitempty"`
	Hour       int            `json:"hour,omitempty"`
	Minute     int            `json:"minute,omitempty"`
	Weekdays   []time.Weekday `json:"weekdays,omitempty"`
	DayOfMonth int            `json:"dayOfMonth,omitempty"`
	Raw        string         `json:"raw,omitempty"`
}

// Cron renders the schedule as a 5-field cron expression (minute hour
// day-of-month month day-of-week). Custom returns Raw verbatim. The generated
// expressions are exactly the shapes ParseCron recognizes, so
// ParseCron(s.Cron()) round-trips every preset back to s.
func (s Schedule) Cron() string {
	switch s.Type {
	case EveryNMinutes:
		return fmt.Sprintf("*/%d * * * *", s.Interval)
	case EveryNHours:
		return fmt.Sprintf("0 */%d * * *", s.Interval)
	case Hourly:
		return fmt.Sprintf("%d * * * *", s.Minute)
	case Daily:
		return fmt.Sprintf("%d %d * * *", s.Minute, s.Hour)
	case Weekly:
		return fmt.Sprintf("%d %d * * %s", s.Minute, s.Hour, weekdayField(s.Weekdays))
	case Monthly:
		return fmt.Sprintf("%d %d %d * *", s.Minute, s.Hour, s.DayOfMonth)
	default: // Custom and any unknown type
		return s.Raw
	}
}

// Describe renders the schedule as the plain-English preview line, e.g.
// "Every 15 minutes", "Every day at 3:41 PM", "Every week on Mon, Wed at
// 9:00 AM". Custom echoes its raw cron ("Custom: 41 3 * * *"). Times use a
// 12-hour clock with AM/PM.
func (s Schedule) Describe() string {
	switch s.Type {
	case EveryNMinutes:
		if s.Interval == 1 {
			return "Every minute"
		}
		return fmt.Sprintf("Every %d minutes", s.Interval)
	case EveryNHours:
		if s.Interval == 1 {
			return "Every hour"
		}
		return fmt.Sprintf("Every %d hours", s.Interval)
	case Hourly:
		return fmt.Sprintf("Every hour at :%02d", s.Minute)
	case Daily:
		return fmt.Sprintf("Every day at %s", clockTime(s.Hour, s.Minute))
	case Weekly:
		return fmt.Sprintf("Every week on %s at %s", weekdayNames(s.Weekdays), clockTime(s.Hour, s.Minute))
	case Monthly:
		return fmt.Sprintf("Every month on the %s at %s", ordinal(s.DayOfMonth), clockTime(s.Hour, s.Minute))
	default: // Custom and any unknown type
		return "Custom: " + s.Raw
	}
}

// ParseCron best-effort maps a 5-field cron expression back to a structured
// Schedule. It recognizes the shapes Cron emits — so a task saved by the
// picker re-opens as its matching preset — plus equivalent zero-padded numeric
// fields and a Sunday day-of-week alias (7). Anything else (ranges,
// multi-value minute/hour fields, month
// restrictions, step forms we don't emit, or a malformed expression) returns
// {Type: Custom, Raw: expr} with ok=false, signalling the UI to fall back to
// the raw-cron editor. It deliberately does NOT parse arbitrary cron.
func ParseCron(expr string) (Schedule, bool) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return custom(expr), false
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	// No preset restricts the month field.
	if month != "*" {
		return custom(expr), false
	}

	// every N minutes: */N * * * * (N in 1-59; see stepOfStar)
	if n, ok := stepOfStar(minute, 59); ok && hour == "*" && dom == "*" && dow == "*" {
		return Schedule{Type: EveryNMinutes, Interval: n}, true
	}
	// every N hours: 0 */N * * * (N in 1-23). Parse the minute
	// numerically so an equivalent zero-padded field selects the same preset.
	if n, ok := stepOfStar(hour, 23); ok && dom == "*" && dow == "*" {
		if m, ok := singleInt(minute, 0, 59); ok && m == 0 {
			return Schedule{Type: EveryNHours, Interval: n}, true
		}
	}
	// hourly: M * * * *
	if m, ok := singleInt(minute, 0, 59); ok && hour == "*" && dom == "*" && dow == "*" {
		return Schedule{Type: Hourly, Minute: m}, true
	}

	// The remaining presets all pin a single minute and hour.
	m, okM := singleInt(minute, 0, 59)
	h, okH := singleInt(hour, 0, 23)
	if okM && okH {
		switch {
		case dom == "*" && dow == "*":
			// daily: M H * * *
			return Schedule{Type: Daily, Hour: h, Minute: m}, true
		case dom == "*":
			// weekly: M H * * <days>
			if days, ok := weekdayList(dow); ok {
				return Schedule{Type: Weekly, Hour: h, Minute: m, Weekdays: days}, true
			}
		case dow == "*":
			// monthly: M H DOM * *
			if d, ok := singleInt(dom, 1, 31); ok {
				return Schedule{Type: Monthly, Hour: h, Minute: m, DayOfMonth: d}, true
			}
		}
	}
	return custom(expr), false
}

func custom(expr string) Schedule {
	return Schedule{Type: Custom, Raw: expr}
}

// weekdayField renders the day-of-week cron field: a sorted, de-duplicated,
// comma-separated list of weekday numbers (0-6). Empty weekdays render as "*"
// (every day) so the expression stays valid even mid-edit; the UI requires at
// least one day before it will save a weekly task.
func weekdayField(days []time.Weekday) string {
	nums := normalizeWeekdays(days)
	if len(nums) == 0 {
		return "*"
	}
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// weekdayNames renders weekdays as sorted three-letter abbreviations joined by
// ", ", e.g. [Monday, Wednesday] → "Mon, Wed". Ordered Sunday-first to match
// the cron day-of-week numbering.
func weekdayNames(days []time.Weekday) string {
	nums := normalizeWeekdays(days)
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = time.Weekday(n).String()[:3]
	}
	return strings.Join(parts, ", ")
}

// normalizeWeekdays maps each weekday into 0-6 (7 is the Sunday alias),
// de-duplicates, and sorts ascending so both the cron field and the
// description present days in a stable Sunday-first order.
func normalizeWeekdays(days []time.Weekday) []int {
	seen := make(map[int]bool, len(days))
	nums := make([]int, 0, len(days))
	for _, d := range days {
		n := int(d) % 7
		if n < 0 {
			n += 7
		}
		if !seen[n] {
			seen[n] = true
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	return nums
}

// clockTime formats a 24-hour hour/minute as a 12-hour clock time with AM/PM,
// e.g. (0,0)→"12:00 AM", (12,0)→"12:00 PM", (15,41)→"3:41 PM".
func clockTime(hour, minute int) string {
	suffix := "AM"
	h := hour
	switch {
	case h == 0:
		h = 12
	case h == 12:
		suffix = "PM"
	case h > 12:
		h -= 12
		suffix = "PM"
	}
	return fmt.Sprintf("%d:%02d %s", h, minute, suffix)
}

// ordinal renders n as an English ordinal: 1→"1st", 2→"2nd", 3→"3rd",
// 11→"11th", 21→"21st", 31→"31st".
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// stepOfStar parses a "*/N" field and returns N when it is a friendly interval,
// 1 <= N <= max. A step at or beyond the field size (e.g. "*/60" for minutes or
// "*/24" for hours) is rejected so ParseCron falls back to Custom and preserves
// the raw expression, rather than the picker clamping it (to */59 / */23) and
// silently rewriting the cron on an otherwise-untouched re-save (#2057). Any
// other shape (a bare "*", a single int, a range step, or a list) also returns
// ok=false.
func stepOfStar(field string, max int) (int, bool) {
	rest, ok := strings.CutPrefix(field, "*/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// singleInt parses a field that is a single integer within [min,max]. A field
// containing any cron metacharacter (* / , -) fails, so only a bare number
// matches.
func singleInt(field string, min, max int) (int, bool) {
	n, err := strconv.Atoi(field)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

// weekdayList parses a day-of-week field that is a comma-separated list of
// single weekday numbers (0-6, or 7 as a Sunday alias) into sorted,
// de-duplicated time.Weekday values — the shape Cron emits for a weekly
// schedule. A wildcard, range, or step form returns ok=false so the caller
// falls back to Custom.
func weekdayList(field string) ([]time.Weekday, bool) {
	if field == "*" {
		return nil, false
	}
	seen := make(map[int]bool)
	var nums []int
	for _, part := range strings.Split(field, ",") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 7 {
			return nil, false
		}
		if n == 7 {
			n = 0 // Sunday alias
		}
		if !seen[n] {
			seen[n] = true
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return nil, false
	}
	sort.Ints(nums)
	days := make([]time.Weekday, len(nums))
	for i, n := range nums {
		days[i] = time.Weekday(n)
	}
	return days, true
}
