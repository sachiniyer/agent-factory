package task

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	cron "github.com/robfig/cron/v3"
)

// cronParser is the schedule parser used by the daemon's in-process scheduler.
// Standard 5-field cron (minute hour dom month dow) with Vixie semantics —
// including the DOM/DOW OR rule when both fields are restricted. Replacing the
// old cron→systemd-OnCalendar / launchd-plist conversion layer with direct
// evaluation through one library is the point of #782: the conversion layer
// was the source of a long line of fan-out bugs (#522 #535 #550 #555 #576
// #590 #743 #770).
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseCron parses a 5-field cron expression into an evaluatable schedule.
// ValidateCronExpr remains the user-facing gate; this applies the same
// validation first so the two can never disagree about which expressions are
// legal, then normalizes day-of-week 7 (an alias for Sunday that
// ValidateCronExpr accepts, matching Vixie cron) down to 0 because the robfig
// parser bounds day-of-week to 0-6.
func ParseCron(expr string) (cron.Schedule, error) {
	if err := ValidateCronExpr(expr); err != nil {
		return nil, err
	}
	fields := strings.Fields(expr)
	fields[4] = normalizeDOWField(fields[4])
	return cronParser.Parse(strings.Join(fields, " "))
}

// normalizeDOWField rewrites a day-of-week field that mentions 7 (Sunday
// alias) into an explicit value list with 7 mapped to 0, e.g. "5-7" →
// "0,5,6". The result is deliberately kept as an explicit list rather than
// collapsed back to "*" even when it covers all seven days: a syntactically
// restricted DOW participates in cron's DOM/DOW OR semantics, and rewriting
// it to a wildcard would change which days match when DOM is also restricted.
func normalizeDOWField(field string) string {
	if field == "*" || !strings.Contains(field, "7") {
		return field
	}
	vals, err := expandCronField(field, 0, 7)
	if err != nil || vals == nil {
		return field
	}
	vals = normalizeDOWValues(vals)
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// normalizeDOWValues maps weekday value 7 to 0 (both mean Sunday)
// and deduplicates/sorts the result.
func normalizeDOWValues(vals []int) []int {
	seen := make(map[int]bool)
	var unique []int
	for _, v := range vals {
		if v == 7 {
			v = 0
		}
		if !seen[v] {
			seen[v] = true
			unique = append(unique, v)
		}
	}
	sort.Ints(unique)
	return unique
}

// ValidateCronExpr validates a 5-field cron expression (minute hour dom month dow).
func ValidateCronExpr(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("cron expression must have exactly 5 fields, got %d", len(fields))
	}

	type fieldSpec struct {
		name string
		min  int
		max  int
	}
	specs := []fieldSpec{
		{"minute", 0, 59},
		{"hour", 0, 23},
		{"day-of-month", 1, 31},
		{"month", 1, 12},
		{"day-of-week", 0, 7},
	}

	for i, field := range fields {
		if err := validateField(field, specs[i].min, specs[i].max); err != nil {
			return fmt.Errorf("invalid %s field %q: %w", specs[i].name, field, err)
		}
	}
	return nil
}

// validateField validates a single cron field against the given min/max range.
// It handles wildcards (*), lists (1,3,5), ranges (1-5), and step values (*/5, 1-5/2).
func validateField(field string, min, max int) error {
	// Handle lists (e.g. "1,3,5")
	parts := strings.Split(field, ",")
	for _, part := range parts {
		if err := validatePart(part, min, max); err != nil {
			return err
		}
	}
	return nil
}

func validatePart(part string, min, max int) error {
	// Handle step values (e.g. "*/5" or "1-5/2")
	if idx := strings.Index(part, "/"); idx != -1 {
		step := part[idx+1:]
		part = part[:idx]
		if step == "" {
			return fmt.Errorf("empty step value")
		}
		stepVal, err := strconv.Atoi(step)
		if err != nil {
			return fmt.Errorf("invalid step value %q", step)
		}
		if stepVal <= 0 {
			return fmt.Errorf("step value must be positive, got %d", stepVal)
		}
	}

	// Wildcard
	if part == "*" {
		return nil
	}

	// Range (e.g. "1-5")
	if idx := strings.Index(part, "-"); idx != -1 {
		startStr := part[:idx]
		endStr := part[idx+1:]
		start, err := strconv.Atoi(startStr)
		if err != nil {
			return fmt.Errorf("invalid range start %q", startStr)
		}
		end, err := strconv.Atoi(endStr)
		if err != nil {
			return fmt.Errorf("invalid range end %q", endStr)
		}
		if start < min || start > max {
			return fmt.Errorf("range start %d out of bounds [%d-%d]", start, min, max)
		}
		if end < min || end > max {
			return fmt.Errorf("range end %d out of bounds [%d-%d]", end, min, max)
		}
		if start > end {
			return fmt.Errorf("range start %d is greater than end %d", start, end)
		}
		return nil
	}

	// Single number
	val, err := strconv.Atoi(part)
	if err != nil {
		return fmt.Errorf("invalid value %q", part)
	}
	if val < min || val > max {
		return fmt.Errorf("value %d out of bounds [%d-%d]", val, min, max)
	}
	return nil
}

// expandCronField expands a single cron field into all matching integer values.
// Returns nil for wildcard (*) fields, meaning "all values".
func expandCronField(field string, min, max int) ([]int, error) {
	if field == "*" {
		return nil, nil
	}

	var result []int
	parts := strings.Split(field, ",")
	for _, part := range parts {
		vals, err := expandCronPart(part, min, max)
		if err != nil {
			return nil, err
		}
		result = append(result, vals...)
	}

	// Deduplicate and sort
	seen := make(map[int]bool)
	var unique []int
	for _, v := range result {
		if !seen[v] {
			seen[v] = true
			unique = append(unique, v)
		}
	}
	sort.Ints(unique)
	return unique, nil
}

// expandCronPart expands a single cron part (number, range, or step) into values.
func expandCronPart(part string, min, max int) ([]int, error) {
	step := 1
	hasStep := false
	if idx := strings.Index(part, "/"); idx != -1 {
		hasStep = true
		stepStr := part[idx+1:]
		var err error
		step, err = strconv.Atoi(stepStr)
		if err != nil {
			return nil, fmt.Errorf("invalid step value: %s", stepStr)
		}
		part = part[:idx]
	}

	if part == "*" {
		var vals []int
		for i := min; i <= max; i += step {
			vals = append(vals, i)
		}
		return vals, nil
	}

	if idx := strings.Index(part, "-"); idx != -1 {
		start, err := strconv.Atoi(part[:idx])
		if err != nil {
			return nil, err
		}
		end, err := strconv.Atoi(part[idx+1:])
		if err != nil {
			return nil, err
		}
		var vals []int
		for i := start; i <= end; i += step {
			vals = append(vals, i)
		}
		return vals, nil
	}

	val, err := strconv.Atoi(part)
	if err != nil {
		return nil, err
	}
	// When a step is provided with a single number (e.g. "5/10"), expand from
	// that number to max by step. Without a step, return just the single value.
	if hasStep {
		var vals []int
		for i := val; i <= max; i += step {
			vals = append(vals, i)
		}
		return vals, nil
	}
	return []int{val}, nil
}
