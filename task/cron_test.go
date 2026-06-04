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

// TestCronToOnCalendarDOWRange17Monthly is the regression for #576.
// 1-7 covers all 7 unique days (1=Mon, 7=Sun, 0=Sun). Previously
// convertSingleDOW only collapsed ranges starting with 0, so this emitted a
// "Mon..Sun" DOW line; combined with the DOM=15 restriction, the DOM/DOW
// fan-out unioned a daily entry with the monthly entry and systemd fired
// daily instead of monthly.
func TestCronToOnCalendarDOWRange17Monthly(t *testing.T) {
	result, err := CronToOnCalendar("0 9 15 * 1-7")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-15 09:00:00"}, result)
}

func TestCronToOnCalendarDOWRange17(t *testing.T) {
	// 1-7 alone collapses to every-day.
	result, err := CronToOnCalendar("0 9 * * 1-7")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWRange27(t *testing.T) {
	// 2-7 covers Tue,Wed,Thu,Fri,Sat,Sun — 6 unique days (missing Monday),
	// so it must NOT collapse to every-day.
	result, err := CronToOnCalendar("0 9 * * 2-7")
	require.NoError(t, err)
	assert.Equal(t, []string{"Tue..Sun *-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWStepOneCollapses(t *testing.T) {
	// */1 expands to [0..7] which normalizes to all 7 days.
	result, err := CronToOnCalendar("0 9 * * */1")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWListAllDaysZeroSix(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 0,1,2,3,4,5,6")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWListAllDaysOneSeven(t *testing.T) {
	// 1..7 explicit list: 7 = Sunday completes the week.
	result, err := CronToOnCalendar("0 9 * * 1,2,3,4,5,6,7")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

func TestCronToOnCalendarDOWRange26(t *testing.T) {
	// 2-6 covers 5 weekdays; must NOT collapse.
	result, err := CronToOnCalendar("0 9 * * 2-6")
	require.NoError(t, err)
	assert.Equal(t, []string{"Tue..Sat *-*-* 09:00:00"}, result)
}

// TestConvertSingleDOWDayNames protects against future callers that bypass
// ValidateCronExpr: day-name ranges like MON-FRI are not numerically
// expandable, so the all-days collapse must not falsely fire.
func TestConvertSingleDOWDayNames(t *testing.T) {
	assert.NotEqual(t, "", convertSingleDOW("MON-FRI"))
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
// "every day" and the result is a single DOW-restricted entry with DOM=*.
func TestCronToOnCalendarDOMCoversAllNoSplit(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1-31 * 1")
	require.NoError(t, err)
	assert.Equal(t, []string{"Mon *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOMTrulyRestrictedFanOut verifies the OR-semantics
// fan-out still triggers when DOM is a real subset of days (1-7, not 1-31).
func TestCronToOnCalendarDOMTrulyRestrictedFanOut(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1-7 * 1")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"*-*-01..07 09:00:00",
		"Mon *-*-* 09:00:00",
	}, result)
}

// TestCronToOnCalendarDOMCoversAllNoDOW verifies that a full-range DOM with
// wildcard DOW collapses to plain every-day (no DOM restriction in output).
func TestCronToOnCalendarDOMCoversAllNoDOW(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1-31 * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOMSingleDayFanOut verifies that a single restrictive
// DOM still produces the OR-semantics fan-out.
func TestCronToOnCalendarDOMSingleDayFanOut(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1 * 1")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"*-*-01 09:00:00",
		"Mon *-*-* 09:00:00",
	}, result)
}

// --- Leading-zero normalization (#743) ---
//
// ValidateCronExpr accepts leading zeros via strconv.Atoi, but the conversion
// path previously used raw string tokens as dowNames keys, so "07"/"01-05"
// produced invalid OnCalendar output: a single value silently dropped the DOW
// constraint (fired daily instead of Sunday), and a range emitted ".." which
// systemd rejects with "Invalid argument". The conversion must normalize
// leading zeros identically to validation.

// TestCronToOnCalendarDOWLeadingZeroSingle is the headline #743 repro: "07"
// (Sunday with a leading zero) validated but produced "*-*-* 09:00:00" (daily)
// because dowNames["07"] was "" and the DOW constraint was dropped.
func TestCronToOnCalendarDOWLeadingZeroSingle(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 07")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWLeadingZeroSingleZero confirms "00" also normalizes to
// Sunday (matching plain "0").
func TestCronToOnCalendarDOWLeadingZeroSingleZero(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 00")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWLeadingZeroRange is the second #743 repro: "01-05"
// previously produced "..  *-*-* 09:00:00" which systemd rejects, because
// dowNames["01"] and dowNames["05"] were both "".
func TestCronToOnCalendarDOWLeadingZeroRange(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 01-05")
	require.NoError(t, err)
	assert.Equal(t, []string{"Mon..Fri *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWLeadingZeroSundayRange exercises the Sunday-start
// expansion path with leading zeros ("00-03"); the start==0 branch must match
// "00" as well as "0".
func TestCronToOnCalendarDOWLeadingZeroSundayRange(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 00-03")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun,Mon,Tue,Wed *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWLeadingZeroList covers a comma list with leading
// zeros in each element.
func TestCronToOnCalendarDOWLeadingZeroList(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 01,03,05")
	require.NoError(t, err)
	assert.Equal(t, []string{"Mon,Wed,Fri *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarDOWLeadingZeroListSundayDedupe confirms "00,07" (both
// Sunday with leading zeros) dedupes to a single "Sun".
func TestCronToOnCalendarDOWLeadingZeroListSundayDedupe(t *testing.T) {
	result, err := CronToOnCalendar("0 9 * * 00,07")
	require.NoError(t, err)
	assert.Equal(t, []string{"Sun *-*-* 09:00:00"}, result)
}

// TestCronToOnCalendarAllFieldsLeadingZero exercises a leading zero in every
// field at once: minute/hour/DOM/month normalize via zeroPad and DOW via
// dowName. DOM=03 and DOW=Fri are both restricted, so the result fans out
// under DOM/DOW OR-semantics (#522).
func TestCronToOnCalendarAllFieldsLeadingZero(t *testing.T) {
	result, err := CronToOnCalendar("01 02 03 04 05")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"*-04-03 02:01:00",
		"Fri *-04-* 02:01:00",
	}, result)
}

// TestCronToOnCalendarStepLeadingZero verifies a leading zero in a step value
// is stripped ("*/05" → "00/5", not "00/05").
func TestCronToOnCalendarStepLeadingZero(t *testing.T) {
	result, err := CronToOnCalendar("*/05 * * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* *:00/5:00"}, result)
}

// TestCronToOnCalendarRangeStepLeadingZero verifies leading zeros are stripped
// from both the range bounds (zero-padded) and the step.
func TestCronToOnCalendarRangeStepLeadingZero(t *testing.T) {
	result, err := CronToOnCalendar("00-30/05 * * * *")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-* *:00..30/5:00"}, result)
}

// TestCronToOnCalendarDOWCoversAllNoSplit verifies the symmetric case where
// DOW covers all 7 days. The OR collapses to "every day", so DOW is dropped
// and only the DOM restriction remains in a single entry.
func TestCronToOnCalendarDOWCoversAllNoSplit(t *testing.T) {
	result, err := CronToOnCalendar("0 9 1 * 0-6")
	require.NoError(t, err)
	assert.Equal(t, []string{"*-*-01 09:00:00"}, result)
}
