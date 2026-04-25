//go:build darwin

package task

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countDicts returns the number of <dict> entries in a
// StartCalendarInterval XML fragment.
func countDicts(xml string) int {
	return strings.Count(xml, "<dict>")
}

// TestCronToCalendarIntervalXML_NoDoubleTrigger verifies that when DOW is
// syntactically restricted but covers all possible weekdays (e.g., 0-6),
// the schedule collapses to a single wildcard-day dict rather than
// emitting both a DOM dict and 7 DOW dicts (which would double-fire on
// the overlap day).
func TestCronToCalendarIntervalXML_NoDoubleTrigger(t *testing.T) {
	// DOW=0-6 covers every weekday. Under cron OR semantics, this is
	// equivalent to "every day at 09:00"; launchd must receive a single
	// dict with only Hour and Minute.
	xml, err := cronToCalendarIntervalXML("0 9 1 * 0-6")
	require.NoError(t, err)

	assert.Equal(t, 1, countDicts(xml), "expected a single dict, got:\n%s", xml)
	assert.Contains(t, xml, "<key>Hour</key>")
	assert.Contains(t, xml, "<integer>9</integer>")
	assert.Contains(t, xml, "<key>Minute</key>")
	assert.Contains(t, xml, "<integer>0</integer>")
	assert.NotContains(t, xml, "<key>Day</key>", "Day key must be omitted when DOW covers all")
	assert.NotContains(t, xml, "<key>Weekday</key>", "Weekday key must be omitted when DOW covers all")
}

// TestCronToCalendarIntervalXML_DOMCoversAll verifies the symmetric case
// where DOM covers all 31 possible values. Under cron OR semantics this
// also collapses to "every day".
func TestCronToCalendarIntervalXML_DOMCoversAll(t *testing.T) {
	xml, err := cronToCalendarIntervalXML("30 8 1-31 * 1")
	require.NoError(t, err)

	assert.Equal(t, 1, countDicts(xml), "expected a single dict, got:\n%s", xml)
	assert.Contains(t, xml, "<key>Hour</key>")
	assert.Contains(t, xml, "<integer>8</integer>")
	assert.Contains(t, xml, "<key>Minute</key>")
	assert.Contains(t, xml, "<integer>30</integer>")
	assert.NotContains(t, xml, "<key>Day</key>", "Day key must be omitted when DOM covers all")
	assert.NotContains(t, xml, "<key>Weekday</key>", "Weekday key must be omitted when DOM covers all")
}

// TestCronToCalendarIntervalXML_DOWStepCoversAll verifies that a step
// expression that expands to every weekday also triggers the collapse.
func TestCronToCalendarIntervalXML_DOWStepCoversAll(t *testing.T) {
	xml, err := cronToCalendarIntervalXML("0 9 15 * */1")
	require.NoError(t, err)

	assert.Equal(t, 1, countDicts(xml), "expected a single dict, got:\n%s", xml)
	assert.NotContains(t, xml, "<key>Day</key>")
	assert.NotContains(t, xml, "<key>Weekday</key>")
}

// TestCronToCalendarIntervalXML_BothRestrictedRejected verifies that when
// DOM and DOW are both restricted and neither covers all values, the
// expression is rejected. launchd cannot express the cron OR-semantics
// without double-firing on dates that match both fields, so we surface a
// clear error rather than silently scheduling duplicate runs.
func TestCronToCalendarIntervalXML_BothRestrictedRejected(t *testing.T) {
	cases := []string{
		"0 9 1 * 1",     // single DOM, single DOW
		"0 9 1,15 * 2",  // multi DOM, single DOW
		"0 9 1-5 * 1-3", // range DOM, range DOW
		"0 9 */10 * 1",  // step DOM, single DOW
	}
	for _, expr := range cases {
		expr := expr
		t.Run(expr, func(t *testing.T) {
			xml, err := cronToCalendarIntervalXML(expr)
			require.Error(t, err, "expected rejection for %q", expr)
			assert.Empty(t, xml)
			assert.Contains(t, err.Error(), "day-of-month and day-of-week")
		})
	}
}

// TestCronToCalendarIntervalXML_WildcardDOW verifies the baseline case
// where DOW is an explicit wildcard — only the DOM dict should be
// emitted (no OR-semantics merging needed).
func TestCronToCalendarIntervalXML_WildcardDOW(t *testing.T) {
	xml, err := cronToCalendarIntervalXML("0 9 1 * *")
	require.NoError(t, err)

	assert.Equal(t, 1, countDicts(xml), "expected one dict, got:\n%s", xml)
	assert.Contains(t, xml, "<key>Day</key>")
	assert.Contains(t, xml, "<integer>1</integer>")
	assert.NotContains(t, xml, "<key>Weekday</key>")
}

// TestCronToCalendarIntervalXML_DOW7NormalizesAndCoversAll verifies that
// a DOW range of 1-7 (where 7 normalizes to 0) covers all weekdays and
// triggers the collapse.
func TestCronToCalendarIntervalXML_DOW7NormalizesAndCoversAll(t *testing.T) {
	xml, err := cronToCalendarIntervalXML("0 9 15 * 1-7")
	require.NoError(t, err)

	assert.Equal(t, 1, countDicts(xml), "expected a single dict, got:\n%s", xml)
	assert.NotContains(t, xml, "<key>Day</key>")
	assert.NotContains(t, xml, "<key>Weekday</key>")
}
