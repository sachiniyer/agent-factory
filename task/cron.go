package task

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var dowNames = map[string]string{
	"0": "Sun",
	"1": "Mon",
	"2": "Tue",
	"3": "Wed",
	"4": "Thu",
	"5": "Fri",
	"6": "Sat",
	"7": "Sun",
}

// dowName returns the systemd day-of-week name for a single numeric cron token,
// normalizing leading zeros (e.g. "07" → "Sun", "01" → "Mon") before the
// dowNames lookup. ValidateCronExpr accepts leading zeros via strconv.Atoi, so
// the conversion path must normalize identically or it emits invalid OnCalendar
// output (a missing name yields ".." which systemd rejects, or drops the DOW
// constraint so the timer fires daily). Non-numeric tokens — which only reach
// here if a caller bypasses validation — return "", preserving the prior
// raw-map-lookup behavior (#743).
func dowName(token string) string {
	v, err := strconv.Atoi(token)
	if err != nil {
		return ""
	}
	return dowNames[strconv.Itoa(v)]
}

// normalizeNum strips leading zeros from a numeric string (e.g. "05" → "5").
// Non-numeric tokens are returned unchanged. Used to normalize step values
// before emitting them to systemd, which expects bare integers (#743).
func normalizeNum(s string) string {
	v, err := strconv.Atoi(s)
	if err != nil {
		return s
	}
	return strconv.Itoa(v)
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

// CronToOnCalendar converts a 5-field cron expression to one or more systemd
// OnCalendar entries. When both day-of-month and day-of-week are restricted,
// Vixie cron uses OR semantics (match DOM OR match DOW), while a single
// systemd OnCalendar entry combines all fields with AND. In that case this
// returns two entries — one constraining DOM (DOW=*), one constraining DOW
// (DOM=*) — and the caller emits one OnCalendar= line per entry so systemd
// unions them, reproducing cron's OR semantics.
func CronToOnCalendar(cronExpr string) ([]string, error) {
	if err := ValidateCronExpr(cronExpr); err != nil {
		return nil, err
	}

	fields := strings.Fields(cronExpr)
	minuteField := fields[0]
	hourField := fields[1]
	domField := fields[2]
	monthField := fields[3]
	dowField := fields[4]

	monthPart := convertTimeField(monthField, true)
	domPart := convertTimeField(domField, true)
	hourPart := convertTimeField(hourField, false)
	minutePart := convertTimeField(minuteField, false)
	timePart := fmt.Sprintf("%s:%s:00", hourPart, minutePart)

	dowPart := ""
	if dowField != "*" {
		dowPart = convertDOW(dowField)
	}

	// A field that syntactically restricts but covers every legal value
	// (DOM=1-31, DOW=0-6, DOW=*/1, etc.) is an effective wildcard. Under
	// cron OR semantics, "X OR every-day" collapses to "every day", so an
	// effective-wildcard side must not trigger the OR fan-out. convertDOW
	// already returns "" for the DOW side; for DOM we expand and check.
	domVals, _ := expandCronField(domField, 1, 31)
	domCoversAll := domVals != nil && len(domVals) >= 31
	if domCoversAll {
		domPart = "*"
	}

	domRestricted := domField != "*" && !domCoversAll
	dowRestricted := dowPart != ""

	if domRestricted && dowRestricted {
		domOnly := fmt.Sprintf("*-%s-%s %s", monthPart, domPart, timePart)
		dowOnly := fmt.Sprintf("%s *-%s-* %s", dowPart, monthPart, timePart)
		return []string{domOnly, dowOnly}, nil
	}

	datePart := fmt.Sprintf("*-%s-%s", monthPart, domPart)
	if dowRestricted {
		return []string{fmt.Sprintf("%s %s %s", dowPart, datePart, timePart)}, nil
	}
	return []string{fmt.Sprintf("%s %s", datePart, timePart)}, nil
}

// convertTimeField converts a cron time field to OnCalendar format.
// oneIndexed should be true for month and day-of-month fields (which start at 1),
// and false for hour and minute fields (which start at 0).
func convertTimeField(field string, oneIndexed bool) string {
	if field == "*" {
		return "*"
	}

	// Handle lists (e.g. "1,3,5" → "01,03,05")
	if strings.Contains(field, ",") {
		parts := strings.Split(field, ",")
		converted := make([]string, len(parts))
		for i, p := range parts {
			converted[i] = convertTimeField(p, oneIndexed)
		}
		return strings.Join(converted, ",")
	}

	// Handle step values
	if strings.Contains(field, "/") {
		idx := strings.Index(field, "/")
		base := field[:idx]
		step := normalizeNum(field[idx+1:])
		if base == "*" {
			if oneIndexed {
				return fmt.Sprintf("01/%s", step)
			}
			return fmt.Sprintf("00/%s", step)
		}
		// Range with step: "X-Y/N" → "XX..YY/N"
		if dashIdx := strings.Index(base, "-"); dashIdx != -1 {
			start := base[:dashIdx]
			end := base[dashIdx+1:]
			return fmt.Sprintf("%s..%s/%s", zeroPad(start), zeroPad(end), step)
		}
		return fmt.Sprintf("%s/%s", zeroPad(base), step)
	}

	// Handle ranges (e.g. "1-5" → "01..05")
	if dashIdx := strings.Index(field, "-"); dashIdx != -1 {
		start := field[:dashIdx]
		end := field[dashIdx+1:]
		return fmt.Sprintf("%s..%s", zeroPad(start), zeroPad(end))
	}

	// Plain number — zero-pad
	return zeroPad(field)
}

// convertDOW converts a cron day-of-week field to systemd day names.
func convertDOW(field string) string {
	// Handle step values (e.g. "*/2" or "1-5/2") by expanding to explicit day names
	if strings.Contains(field, "/") {
		expanded, err := expandCronField(field, 0, 7)
		if err != nil {
			return ""
		}
		// Map to day names, deduplicating (since both 0 and 7 mean Sunday).
		seen := make(map[string]bool)
		var names []string
		for _, v := range expanded {
			name := dowNames[strconv.Itoa(v)]
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		// If all 7 days are covered, omit DOW entirely.
		if len(names) >= 7 {
			return ""
		}
		return strings.Join(names, ",")
	}

	// Handle lists (e.g. "1,3,5" → "Mon,Wed,Fri"). Dedupe day names so
	// "0,7" → "Sun" (not "Sun,Sun"), and short-circuit to "" if any element
	// already covers all 7 days (e.g. "0-6,7"), matching the wildcard
	// convention of omitting DOW entirely.
	if strings.Contains(field, ",") {
		parts := strings.Split(field, ",")
		seen := make(map[string]bool)
		var names []string
		for _, p := range parts {
			converted := convertSingleDOW(p)
			if converted == "" {
				// A single element covers all 7 days; so does the list.
				return ""
			}
			// A single element may itself expand to a comma-separated
			// list (e.g. "0-1" → "Sun,Mon"); split before dedupe.
			for _, n := range strings.Split(converted, ",") {
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			}
		}
		if len(names) >= 7 {
			return ""
		}
		return strings.Join(names, ",")
	}

	// Handle ranges (e.g. "1-5" → "Mon..Fri")
	// For ranges starting with 0 (Sunday), expand to comma-separated day names
	// because systemd requires Day1 < Day2 in weekly order (Mon..Sun), and
	// Sun..X is invalid.
	if strings.Contains(field, "-") {
		return convertSingleDOW(field)
	}

	// Single value
	return dowName(field)
}

// convertSingleDOW converts a single DOW element (number or range) to a name.
// Ranges covering all 7 unique days (0-6, 0-7, 1-7) return "" so the caller
// omits DOW entirely; otherwise CronToOnCalendar treats DOW as restricted and
// emits a "Mon..Sun" line that systemd normalizes to daily, fanning out under
// DOM/DOW OR-semantics to fire monthly schedules every day (#576).
// For ranges starting with 0 (Sunday) but not all-days, the range expands to
// a comma-separated list because systemd's range syntax requires Day1 < Day2
// in weekly order (Mon->Tue->...->Sun), making Sun..X invalid.
func convertSingleDOW(part string) string {
	if strings.Contains(part, "-") {
		if expanded, err := expandCronField(part, 0, 7); err == nil {
			if len(normalizeDOWValues(expanded)) >= 7 {
				return ""
			}
		}

		idx := strings.Index(part, "-")
		start := part[:idx]
		end := part[idx+1:]

		// Match leading-zero forms too ("00" as well as "0"); strconv.Atoi
		// normalizes both, but guard on err so non-numeric tokens (which
		// Atoi maps to 0) don't falsely enter the Sunday-start branch.
		if startVal, err := strconv.Atoi(start); err == nil && startVal == 0 {
			endVal, _ := strconv.Atoi(end)
			var names []string
			for i := 0; i <= endVal; i++ {
				names = append(names, dowNames[strconv.Itoa(i)])
			}
			return strings.Join(names, ",")
		}

		return fmt.Sprintf("%s..%s", dowName(start), dowName(end))
	}
	return dowName(part)
}

// zeroPad pads a numeric string to 2 digits.
func zeroPad(s string) string {
	val, err := strconv.Atoi(s)
	if err != nil {
		return s
	}
	return fmt.Sprintf("%02d", val)
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
