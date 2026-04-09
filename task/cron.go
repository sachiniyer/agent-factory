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
	step := ""
	if idx := strings.Index(part, "/"); idx != -1 {
		step = part[idx+1:]
		part = part[:idx]
	}

	if step != "" {
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

// CronToOnCalendar converts a 5-field cron expression to systemd OnCalendar format.
func CronToOnCalendar(cronExpr string) (string, error) {
	if err := ValidateCronExpr(cronExpr); err != nil {
		return "", err
	}

	fields := strings.Fields(cronExpr)
	minuteField := fields[0]
	hourField := fields[1]
	domField := fields[2]
	monthField := fields[3]
	dowField := fields[4]

	// Convert day-of-week
	dowPart := ""
	if dowField != "*" {
		dowPart = convertDOW(dowField)
	}

	// Convert month
	monthPart := convertTimeField(monthField)

	// Convert day-of-month
	domPart := convertTimeField(domField)

	// Convert hour
	hourPart := convertTimeField(hourField)

	// Convert minute
	minutePart := convertTimeField(minuteField)

	// Build the date part: YEAR-MONTH-DAY
	datePart := fmt.Sprintf("*-%s-%s", monthPart, domPart)

	// Build the time part: HOUR:MIN:SEC
	timePart := fmt.Sprintf("%s:%s:00", hourPart, minutePart)

	// Combine
	if dowPart != "" {
		return fmt.Sprintf("%s %s %s", dowPart, datePart, timePart), nil
	}
	return fmt.Sprintf("%s %s", datePart, timePart), nil
}

// convertTimeField converts a cron time field (minute or hour) to OnCalendar format.
func convertTimeField(field string) string {
	if field == "*" {
		return "*"
	}

	// Handle lists (e.g. "1,3,5" → "01,03,05")
	if strings.Contains(field, ",") {
		parts := strings.Split(field, ",")
		converted := make([]string, len(parts))
		for i, p := range parts {
			converted[i] = convertTimeField(p)
		}
		return strings.Join(converted, ",")
	}

	// Handle step values
	if strings.Contains(field, "/") {
		idx := strings.Index(field, "/")
		base := field[:idx]
		step := field[idx+1:]
		if base == "*" {
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
		expanded, err := expandCronField(field, 0, 6)
		if err != nil {
			return ""
		}
		// If all 7 days are covered, omit DOW entirely.
		if len(expanded) >= 7 {
			return ""
		}
		names := make([]string, len(expanded))
		for i, v := range expanded {
			names[i] = dowNames[strconv.Itoa(v)]
		}
		return strings.Join(names, ",")
	}

	// Handle lists (e.g. "1,3,5" → "Mon,Wed,Fri")
	if strings.Contains(field, ",") {
		parts := strings.Split(field, ",")
		names := make([]string, len(parts))
		for i, p := range parts {
			names[i] = convertSingleDOW(p)
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
	return dowNames[field]
}

// convertSingleDOW converts a single DOW element (number or range) to a name.
// For ranges starting with 0 (Sunday), it expands to a comma-separated list
// because systemd's range syntax requires Day1 < Day2 in weekly order
// (Mon->Tue->...->Sun), making Sun..X invalid.
func convertSingleDOW(part string) string {
	if strings.Contains(part, "-") {
		idx := strings.Index(part, "-")
		start := part[:idx]
		end := part[idx+1:]

		// For ranges starting with Sunday (0), expand to comma-separated names.
		if start == "0" {
			endVal, _ := strconv.Atoi(end)
			// 0-6 or 0-7 covers all 7 days (7 is also Sunday in cron).
			// Return empty to signal the caller to omit DOW entirely.
			if endVal >= 6 {
				return ""
			}
			var names []string
			for i := 0; i <= endVal; i++ {
				names = append(names, dowNames[strconv.Itoa(i)])
			}
			return strings.Join(names, ",")
		}

		return fmt.Sprintf("%s..%s", dowNames[start], dowNames[end])
	}
	return dowNames[part]
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
	if idx := strings.Index(part, "/"); idx != -1 {
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
	return []int{val}, nil
}
