package task

import (
	"testing"

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

func TestCronToOnCalendarSimple(t *testing.T) {
	result, err := CronToOnCalendar("30 2 * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 02:30:00"}, result)
}

func TestCronToOnCalendarRange(t *testing.T) {
	result, err := CronToOnCalendar("0 9-17 * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09..17:00:00"}, result)
}

func TestCronToOnCalendarRangeWithStep(t *testing.T) {
	result, err := CronToOnCalendar("0 9-17/2 * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09..17/2:00:00"}, result)
}

func TestCronToOnCalendarList(t *testing.T) {
	result, err := CronToOnCalendar("0 1,3,5 * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 01,03,05:00:00"}, result)
}

func TestCronToOnCalendarMonthRange(t *testing.T) {
	result, err := CronToOnCalendar("0 0 1 6-8 *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-06..08-01 00:00:00"}, result)
}

func TestCronToOnCalendarDOMRange(t *testing.T) {
	result, err := CronToOnCalendar("0 0 1-15 * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-01..15 00:00:00"}, result)
}

func TestCronToOnCalendarMinuteRange(t *testing.T) {
	result, err := CronToOnCalendar("0-30/10 * * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* *:00..30/10:00"}, result)
}

func TestCronToOnCalendarDOW(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 1-5")
	require.NoError(t, err)
	assert.Equal(t, []string{"Mon..Fri *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWStepAll(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * */2")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun,Tue,Thu,Sat *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWStepRange(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 1-5/2")
	require.NoError(t, err)
	assert.Equal(t, []string{"Mon,Wed,Fri *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWStepFrom7 is a regression test for a bug where DOW
// step expressions starting at 7 (e.g. "7/2") expanded to an empty set because
// convertDOW called expandCronField with max=6, while validation treats DOW as
// 0-7 (with 7 also meaning Sunday). The empty set caused the DOW constraint to
// be silently dropped, changing the schedule to run every day.
func TestCronToOnCalendarDOWStepFrom7(t *testing.T) {
	// "7/2" should expand to just [7] (i.e. Sunday); step 2 from 7 with max=7
	// yields a single value. The result must include Sunday and not omit DOW.
	result, err := CronToOnCalendar("0 9 * * 7/2")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWSundayRange01(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 0-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun,Mon *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWSundayRange03(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 0-3")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun,Mon,Tue,Wed *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWSundayRange06(t *testing.T) {
	// 0-6 covers all 7 days, so DOW should be omitted.
	result, err := CronToOnCalendar("0 9 * * 0-6")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWSundayRange07(t *testing.T) {
	// 0-7 also covers all days (7 is Sunday in cron), so DOW should be omitted.
	result, err := CronToOnCalendar("0 9 * * 0-7")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWListDedupes is a regression test for #465: "0,7"
// both map to Sunday, which previously produced an invalid "Sun,Sun"
// OnCalendar string that systemd rejects.
func TestCronToOnCalendarDOWListDedupes(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 0,7")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWListMonWedFri(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 1,3,5")
	require.NoError(t, err)
	assert.Equal(t, []string{"Mon,Wed,Fri *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWListAllDays covers a list whose elements union to
// all 7 days. "0-6" alone is all days, so "0-6,7" must omit DOW entirely
// (previously emitted invalid ",Sun").
func TestCronToOnCalendarDOWListAllDays(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 0-6,7")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWNonSundayRange(t *testing.T) {
	// Non-zero-starting ranges should still use .. syntax.
	result, err := CronToOnCalendar("0 9 * * 2-4")
	require.NoError(t, err)
	assert.Equal(t, []string{"Tue..Thu *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarMonthStep(t *testing.T) {
	// Month is 1-indexed, so */3 should become 01/3, not 00/3.
	result, err := CronToOnCalendar("0 0 1 */3 *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-01/3-01 00:00:00"}, result)
}

func TestCronToOnCalendarDOMStep(t *testing.T) {
	// Day-of-month is 1-indexed, so */5 should become 01/5, not 00/5.
	result, err := CronToOnCalendar("0 0 */5 * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-01/5 00:00:00"}, result)
}

func TestCronToOnCalendarHourStep(t *testing.T) {
	// Hour is 0-indexed, so */4 should become 00/4.
	result, err := CronToOnCalendar("0 */4 * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 00/4:00:00"}, result)
}

func TestCronToOnCalendarMinuteStep(t *testing.T) {
	// Minute is 0-indexed, so */15 should become 00/15.
	result, err := CronToOnCalendar("*/15 * * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* *:00/15:00"}, result)
}

// TestCronToOnCalendarDOMandDOW is a regression test for #522. Vixie cron
// fires when DOM matches OR DOW matches when both are restricted; systemd's
// OnCalendar combines fields with AND, so a single entry under-fires by ~85%
// (only ~7 Monday-1st-of-month dates per year instead of ~60). Fan out into
// two entries — one constrains DOM (DOW=*), one constrains DOW (DOM=*) —
// because systemd unions multiple OnCalendar lines.
func TestCronToOnCalendarDOMandDOW(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1 * 1")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"*-*-01 09:00:00",
		"Mon *-*-* 09:00:00",
	}, result)
}

// TestCronToOnCalendarDOMandDOWPreservesMonth confirms that the month
// restriction is preserved on both split entries (only DOM/DOW are subject
// to OR semantics; month is always ANDed in cron).
func TestCronToOnCalendarDOMandDOWPreservesMonth(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1 6 1")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"*-06-01 09:00:00",
		"Mon *-06-* 09:00:00",
	}, result)
}

// TestCronToOnCalendarDOMandDOWRanges exercises the split with multi-value
// DOM and DOW fields.
func TestCronToOnCalendarDOMandDOWRanges(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1,15 * 1-5")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"*-*-01,15 09:00:00",
		"Mon..Fri *-*-* 09:00:00",
	}, result)
}

// TestCronToOnCalendarDOMandDOWStep exercises a stepped DOM combined with a
// restricted DOW.
func TestCronToOnCalendarDOMandDOWStep(t *testing.T) {
	result, err := CronToOnCalendar("0 9 */10 * 1")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"*-*-01/10 09:00:00",
		"Mon *-*-* 09:00:00",
	}, result)
}

// TestCronToOnCalendarDOMCoversAllNoSplit verifies that when DOM is
// syntactically restricted but covers every day (1-31), the OR collapses to
// "every day" and the result is a single DOW-restricted entry.
func TestCronToOnCalendarDOMCoversAllNoSplit(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1-31 * 1")
	require.NoError(t, err)
	assert.Equal(t, []string{"Mon *-*-01..31 09:00:00"}, result)
}

// TestCronToOnCalendarDOWCoversAllNoSplit verifies the symmetric case where
// DOW covers all 7 days. The OR collapses to "every day", so DOW is dropped
// and only the DOM restriction remains in a single entry.
func TestCronToOnCalendarDOWCoversAllNoSplit(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1 * 0-6")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-01 09:00:00"}, result)
}
