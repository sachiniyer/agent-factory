package task

import (
	"fmt"
	"sort"
	"strings"
)

// maxCalendarIntervals caps the cartesian product size to prevent
// memory explosion from complex cron expressions like "*/1 */1 * * *".
const maxCalendarIntervals = 512

func cronToLaunchdScheduleXML(cronExpr string) (string, error) {
	if isEveryMinuteCron(cronExpr) {
		if err := ValidateCronExpr(cronExpr); err != nil {
			return "", err
		}
		return "    <key>StartInterval</key>\n    <integer>60</integer>", nil
	}

	calendarXML, err := cronToCalendarIntervalXML(cronExpr)
	if err != nil {
		return "", err
	}
	return "    <key>StartCalendarInterval</key>\n" + calendarXML, nil
}

// isEveryMinuteCron reports whether cronExpr is semantically "every minute" —
// either a literal "* * * * *" or a form whose fields all cover every legal
// value (e.g. "*/1 */1 * * *"). Matching the equivalent forms here lets us
// emit StartInterval=60 instead of a single empty StartCalendarInterval dict.
func isEveryMinuteCron(cronExpr string) bool {
	fields := strings.Fields(cronExpr)
	if len(fields) != 5 {
		return false
	}
	ranges := []struct{ min, max int }{
		{0, 59}, // minute
		{0, 23}, // hour
		{1, 31}, // day-of-month
		{1, 12}, // month
		{0, 7},  // day-of-week (0 and 7 both mean Sunday)
	}
	for i, field := range fields {
		if field == "*" {
			continue
		}
		vals, err := expandCronField(field, ranges[i].min, ranges[i].max)
		if err != nil {
			return false
		}
		if i == 4 {
			vals = normalizeDOWValues(vals)
			if len(vals) < 7 {
				return false
			}
			continue
		}
		if len(vals) < (ranges[i].max - ranges[i].min + 1) {
			return false
		}
	}
	return true
}

// cronToCalendarIntervalXML converts a 5-field cron expression to a launchd
// StartCalendarInterval XML value fragment.
func cronToCalendarIntervalXML(cronExpr string) (string, error) {
	if err := ValidateCronExpr(cronExpr); err != nil {
		return "", err
	}

	fields := strings.Fields(cronExpr)

	type fieldDef struct {
		key string
		min int
		max int
	}
	defs := []fieldDef{
		{"Minute", 0, 59},
		{"Hour", 0, 23},
		{"Day", 1, 31},
		{"Month", 1, 12},
		{"Weekday", 0, 7},
	}

	type expandedField struct {
		key  string
		vals []int // nil = wildcard
	}

	var expanded []expandedField
	for i, def := range defs {
		vals, err := expandCronField(fields[i], def.min, def.max)
		if err != nil {
			return "", fmt.Errorf("failed to expand %s field: %w", def.key, err)
		}
		// Normalize weekday 7 -> 0 (both mean Sunday in cron; launchd uses 0).
		if def.key == "Weekday" && vals != nil {
			vals = normalizeDOWValues(vals)
		}
		// Collapse fields that syntactically cover every legal value
		// (e.g. "*/1" for minute expands to all 60 values) back to a
		// wildcard. Without this, the cartesian product blows past
		// maxCalendarIntervals and we reject schedules that are
		// semantically identical to "* * * * *". DOM/DOW have their
		// own collapse below for OR-semantics reasons.
		if def.key != "Day" && def.key != "Weekday" && vals != nil && len(vals) >= (def.max-def.min+1) {
			vals = nil
		}
		expanded = append(expanded, expandedField{key: def.key, vals: vals})
	}

	// When both day-of-month (DOM) and day-of-week (DOW) are non-wildcard,
	// standard cron uses OR semantics: run on matching DOM OR matching DOW.
	// We reject non-collapsible cases because launchd cannot express that
	// semantics without duplicate fires.
	domIdx, dowIdx := 2, 4

	// Detect when DOM or DOW is syntactically restricted but semantically
	// covers all possible values (e.g., DOW=0-6 or DOM=1-31). Under cron OR
	// semantics, "X OR every-day" collapses to "every day", so an
	// effective-wildcard side is irrelevant and must be dropped to a
	// wildcard. Clear only the field that covers all values — clearing both
	// would erase the surviving constraint and silently turn a monthly
	// (e.g. "0 9 15 * 1-7") or weekly (e.g. "0 9 1-31 * 1") schedule into a
	// daily one. This mirrors the systemd path in CronToOnCalendar, which
	// neutralizes DOM and DOW independently.
	dowCoversAll := expanded[dowIdx].vals != nil && len(expanded[dowIdx].vals) >= 7
	domCoversAll := expanded[domIdx].vals != nil && len(expanded[domIdx].vals) >= 31
	if dowCoversAll {
		expanded[dowIdx].vals = nil
	}
	if domCoversAll {
		expanded[domIdx].vals = nil
	}

	bothDOMandDOW := expanded[domIdx].vals != nil && expanded[dowIdx].vals != nil

	// buildCombos builds the cartesian product of the given expanded fields.
	type combo map[string]int
	buildCombos := func(fields []expandedField) ([]combo, error) {
		result := []combo{{}}
		for _, ef := range fields {
			if ef.vals == nil {
				continue // wildcard, omit from dict
			}
			var newResult []combo
			for _, existing := range result {
				for _, val := range ef.vals {
					c := make(combo)
					for k, v := range existing {
						c[k] = v
					}
					c[ef.key] = val
					newResult = append(newResult, c)
				}
			}
			if len(newResult) > maxCalendarIntervals {
				return nil, fmt.Errorf("cron expression %q expands to too many intervals (%d > %d)", cronExpr, len(newResult), maxCalendarIntervals)
			}
			result = newResult
		}
		return result, nil
	}

	if bothDOMandDOW {
		return "", fmt.Errorf("cron expression %q combines day-of-month and day-of-week; macOS launchd cannot correctly express this. Use either DOM or DOW, not both", cronExpr)
	}

	combos, err := buildCombos(expanded)
	if err != nil {
		return "", err
	}

	// Deterministic key order for output.
	keyOrder := []string{"Month", "Day", "Weekday", "Hour", "Minute"}

	var sb strings.Builder
	sb.WriteString("    <array>\n")
	for _, c := range combos {
		sb.WriteString("        <dict>\n")
		for _, key := range keyOrder {
			if val, ok := c[key]; ok {
				fmt.Fprintf(&sb, "            <key>%s</key>\n            <integer>%d</integer>\n", key, val)
			}
		}
		sb.WriteString("        </dict>\n")
	}
	sb.WriteString("    </array>")

	return sb.String(), nil
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
