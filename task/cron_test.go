package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCronFieldExpandWildcard(t *testing.T) {
	vals, err := expandCronField("*", 0, 59)
	assert.NoError(t, err)
	assert.Nil(t, vals)
}

func TestCronFieldExpandSingle(t *testing.T) {
	vals, err := expandCronField("30", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{30}, vals)
}

func TestCronFieldExpandList(t *testing.T) {
	vals, err := expandCronField("1,3,5", 0, 7)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3, 5}, vals)
}

func TestCronFieldExpandRange(t *testing.T) {
	vals, err := expandCronField("1-5", 0, 7)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, vals)
}

func TestCronFieldExpandStep(t *testing.T) {
	vals, err := expandCronField("*/15", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 15, 30, 45}, vals)
}

func TestCronFieldExpandRangeWithStep(t *testing.T) {
	vals, err := expandCronField("0-10/3", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 3, 6, 9}, vals)
}

func TestCronFieldExpandSingleWithStep(t *testing.T) {
	// "5/10" means "every 10 starting at 5" — the step must be honored.
	vals, err := expandCronField("5/10", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{5, 15, 25, 35, 45, 55}, vals)
}

func TestCronFieldExpandSingleWithStepAtBoundary(t *testing.T) {
	// Starting value equal to max should yield just that value.
	vals, err := expandCronField("59/10", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{59}, vals)
}

func TestCronFieldExpandSingleWithStepOne(t *testing.T) {
	vals, err := expandCronField("5/1", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, []int{5, 6, 7, 8, 9, 10}, vals)
}

func TestCronFieldExpandListWithSingleStep(t *testing.T) {
	// A list element with a single-number step should also expand.
	vals, err := expandCronField("0,5/20", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 5, 25, 45}, vals)
}

func TestCronFieldExpandDedup(t *testing.T) {
	vals, err := expandCronField("1,1,3", 0, 7)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3}, vals)
}

// validCronCorpus collects the expression shapes ValidateCronExpr accepts,
// including every form of the day-of-week 7 (Sunday alias) that the robfig
// parser would reject without normalizeDOWField.
var validCronCorpus = []string{
	"* * * * *",
	"30 2 * * *",
	"0 9 * * 1-5",
	"*/5 * * * *",
	"0 */2 * * *",
	"15,45 8,20 * * *",
	"0 3 1 * *",
	"0 3 1,15 * *",
	"0 0 1 1 *",
	"0 12 * 6-8 *",
	"0 9 15 * 1",   // DOM and DOW both restricted (OR semantics)
	"5/10 * * * *", // single-number step
	"0-10/3 * * * *",
	"00 09 01 01 01", // leading zeros (#743)
	"0 0 * * 7",      // Sunday as 7
	"0 0 * * 07",     // Sunday as 07
	"0 0 * * 5-7",    // range ending at 7
	"0 0 * * 1,7",    // list containing 7
	"0 0 * * */7",    // step landing on 0 and 7
	"0 0 * * 7/2",    // single-with-step starting at 7
	"0 0 * * 0-7",    // full range incl. both Sunday aliases
	"0 0 1 * 5-7",    // DOM restricted + DOW range with 7 (OR semantics)
}

// TestParseCronAcceptsEverythingValidateAccepts is the agreement gate between
// the user-facing validator and the schedule evaluator (#782): anything
// ValidateCronExpr lets users save must be schedulable by ParseCron, or a
// saved task would silently never fire.
func TestParseCronAcceptsEverythingValidateAccepts(t *testing.T) {
	for _, expr := range validCronCorpus {
		require.NoError(t, ValidateCronExpr(expr), "corpus expression %q must pass ValidateCronExpr", expr)
		schedule, err := ParseCron(expr)
		require.NoError(t, err, "ParseCron must accept %q (ValidateCronExpr does)", expr)
		require.NotNil(t, schedule)
	}
}

func TestParseCronRejectsWhatValidateRejects(t *testing.T) {
	invalid := []string{
		"",
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // dom out of range
		"* * * 13 *",  // month out of range
		"* * * * 8",   // dow out of range
		"a * * * *",   // non-numeric
		"1-0 * * * *", // inverted range
		"*/0 * * * *", // zero step
		"@daily",      // descriptors are not part of the contract
	}
	for _, expr := range invalid {
		assert.Error(t, ValidateCronExpr(expr), "ValidateCronExpr must reject %q", expr)
		_, err := ParseCron(expr)
		assert.Error(t, err, "ParseCron must reject %q", expr)
	}
}

func TestParseCronDailySchedule(t *testing.T) {
	schedule, err := ParseCron("30 2 * * *")
	require.NoError(t, err)
	from := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 6, 10, 2, 30, 0, 0, time.UTC), schedule.Next(from))
}

func TestParseCronSundayAsSeven(t *testing.T) {
	schedule, err := ParseCron("0 0 * * 7")
	require.NoError(t, err)
	// 2026-06-10 is a Wednesday; the next Sunday is 2026-06-14.
	from := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC), schedule.Next(from))
}

// TestParseCronDOMDOWOrSemantics pins the Vixie OR rule: when both
// day-of-month and day-of-week are restricted, the schedule fires on days
// matching either. The old OnCalendar/plist conversion layer had to emulate
// this with unit fan-out (#522 #535 #550 #576); robfig evaluates it natively.
func TestParseCronDOMDOWOrSemantics(t *testing.T) {
	schedule, err := ParseCron("0 0 13 * 5")
	require.NoError(t, err)
	// 2026-02-01 is a Sunday. DOM alone would give Feb 13; the DOW side
	// (Friday) fires first on Feb 6 — OR semantics pick the earlier one.
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	first := schedule.Next(from)
	assert.Equal(t, time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC), first)
	// From the first Friday, the 13th (also a Friday in Feb 2026) is next.
	assert.Equal(t, time.Date(2026, 2, 13, 0, 0, 0, 0, time.UTC), schedule.Next(first))
}

func TestParseCronDOMWithDOWRangeEndingAtSeven(t *testing.T) {
	schedule, err := ParseCron("0 0 1 * 5-7")
	require.NoError(t, err)
	// 2026-02-02 is a Monday. DOW {Fri,Sat,Sun} fires Feb 6 before DOM=1
	// fires Mar 1 — and the normalized DOW must include Sunday (7→0).
	from := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC), schedule.Next(from))
	// 2026-02-08 (Sunday) must match via the normalized 7→0 alias.
	fromSat := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 2, 8, 0, 0, 0, 0, time.UTC), schedule.Next(fromSat))
}

func TestNormalizeDOWField(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"*", "*"},
		{"1-5", "1-5"}, // untouched: no 7 present
		{"7", "0"},
		{"07", "0"},
		{"5-7", "0,5,6"},
		{"1,7", "0,1"},
		{"*/7", "0"},
		{"0-7", "0,1,2,3,4,5,6"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, normalizeDOWField(tc.in), "normalizeDOWField(%q)", tc.in)
	}
}
