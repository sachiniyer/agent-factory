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
	"0 0 * * 007/2",  // multi-leading-zero step base 7 (#915)
	"0 0 * * 0007/2", // even more leading zeros on the step base (#915)
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

// TestParseCronDOWStepFromSevenPreservesStep is the regression for #888: the
// Sunday alias 7 used as a step base ("7/2") must keep its step and fire
// Sun,Tue,Thu,Sat (≡ "0/2"), not collapse to Sunday-only.
func TestParseCronDOWStepFromSevenPreservesStep(t *testing.T) {
	seven, err := ParseCron("0 0 * * 7/2")
	require.NoError(t, err)
	zero, err := ParseCron("0 0 * * 0/2")
	require.NoError(t, err)

	// Walk a full week from a Saturday and confirm 7/2 fires on the same days
	// as 0/2: Sun, Tue, Thu, Sat. 2026-06-13 is a Saturday.
	from := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	want := []time.Time{
		time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC), // Sunday
		time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC), // Tuesday
		time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC), // Thursday
		time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), // Saturday
	}
	cur7, cur0 := from, from
	for _, w := range want {
		cur7 = seven.Next(cur7)
		cur0 = zero.Next(cur0)
		assert.Equal(t, w, cur7, "7/2 must fire %s", w.Weekday())
		assert.Equal(t, w, cur0, "0/2 must fire %s", w.Weekday())
	}
}

// TestParseCronDOWStepWithLeadingZeroBaseMatchesSeven is the regression for
// #915: a DOW step base of 7 written with arbitrary leading zeros ("007/2",
// "0007/2") must normalize numerically — exactly as ValidateCronExpr parses it —
// and schedule the same days as "7/2" (Sun,Tue,Thu,Sat), not collapse to
// Sunday-only. The pre-fix string-equality check matched only "7"/"07", so the
// validator accepted these forms while the scheduler silently fired them wrong,
// breaking the #782 validator↔scheduler agreement gate.
func TestParseCronDOWStepWithLeadingZeroBaseMatchesSeven(t *testing.T) {
	reference, err := ParseCron("0 0 * * 7/2")
	require.NoError(t, err)

	for _, expr := range []string{"0 0 * * 007/2", "0 0 * * 0007/2"} {
		// The validator must accept it (it parses the base numerically)...
		require.NoError(t, ValidateCronExpr(expr), "ValidateCronExpr must accept %q", expr)
		// ...and the scheduler must too.
		sched, err := ParseCron(expr)
		require.NoError(t, err, "ParseCron must accept %q", expr)

		// The first 7 firings must match "7/2" exactly (Sun,Tue,Thu,Sat,...),
		// proving the leading-zero form is not interpreted as Sunday-only.
		from := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) // a Saturday
		curRef, curTest := from, from
		for i := 0; i < 7; i++ {
			curRef = reference.Next(curRef)
			curTest = sched.Next(curTest)
			assert.Equal(t, curRef, curTest, "%q firing %d must match 7/2", expr, i)
		}
	}
}

func TestNormalizeDOWField(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"*", "*"},
		{"1-5", "1-5"},           // untouched: no 7 present
		{"0/2", "0/2"},           // untouched: no 7, step preserved for robfig
		{"*/2", "*/2"},           // untouched: no 7, step preserved for robfig
		{"7", "0"},               // bare Sunday alias
		{"07", "0"},              // leading-zero Sunday alias (#743)
		{"5-7", "0,5,6"},         // range ending at 7
		{"1,7", "0,1"},           // list containing 7
		{"*/7", "0"},             // step that only lands on 0 and 7
		{"7/2", "0,2,4,6"},       // step base 7 must keep its step (#888): Sun,Tue,Thu,Sat
		{"07/2", "0,2,4,6"},      // leading-zero step base 7 (#888 + #743)
		{"007/2", "0,2,4,6"},     // multi-leading-zero step base 7 (#915): numeric parse, not string compare
		{"0007/2", "0,2,4,6"},    // arbitrary leading zeros on the step base (#915)
		{"1/2", "1/2"},           // no 7 present; passes through untouched
		{"3,7/2", "0,2,3,4,6"},   // 7-step base inside a list still keeps its step (#888)
		{"0-7", "0,1,2,3,4,5,6"}, // full range incl. both Sunday aliases (#770: explicit list, never "*")
		{"1-7", "0,1,2,3,4,5,6"}, // range to the 7 alias covers the whole week as a list (#770)
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, normalizeDOWField(tc.in), "normalizeDOWField(%q)", tc.in)
	}
}
