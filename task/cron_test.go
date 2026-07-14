package task

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	cron "github.com/robfig/cron/v3"
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
	"0 0 * * 1/2",    // step expanding to include 7 (Sunday) (#1007)
	"0 0 * * 1/3",    // step expanding to include 7 (Sunday) (#1007)
	"0 0 * * 2/5",    // step expanding to include 7 (Sunday) (#1007)
	"0 0 * * 0-7/2",  // range-step ending at 7 whose expansion skips the 7 (#1064)
	"0 0 * * 4-7/2",  // range-step ending at 7, expansion [4,6] — no Sunday (#1064)
	"0 0 * * 1-7/3",  // range-step ending at 7 whose expansion lands on 7 (#1064)
	"0 0 * * 0-07/2", // leading zero on the range end bound (#1064 + #915)
	"0 0 * * 1,5-7",  // list mixing a plain value with a range ending at 7
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

// TestParseCronAcceptsAllValidatedDOWParts exhaustively enumerates every DOW
// part shape ValidateCronExpr accepts — single values, steps, ranges,
// range-steps, wildcard steps, leading-zero variants, and two-part lists —
// and asserts ParseCron schedules each one. This closes the whole class
// behind #888/#915/#1007/#1064 (DOW forms the validator accepts but robfig
// rejects or misschedules) rather than pinning individual reproductions: any
// future normalization gap for a 0-7-bounded DOW shape fails here.
func TestParseCronAcceptsAllValidatedDOWParts(t *testing.T) {
	var parts []string
	for v := 0; v <= 7; v++ {
		parts = append(parts, strconv.Itoa(v), fmt.Sprintf("0%d", v))
		for s := 1; s <= 7; s++ {
			parts = append(parts, fmt.Sprintf("%d/%d", v, s))
		}
		for b := v; b <= 7; b++ {
			parts = append(parts, fmt.Sprintf("%d-%d", v, b), fmt.Sprintf("%d-0%d", v, b))
			for s := 1; s <= 7; s++ {
				parts = append(parts, fmt.Sprintf("%d-%d/%d", v, b, s))
			}
		}
	}
	for s := 1; s <= 7; s++ {
		parts = append(parts, fmt.Sprintf("*/%d", s))
	}
	for _, part := range parts {
		for _, field := range []string{part, "1," + part, part + ",5-7"} {
			expr := "0 0 * * " + field
			require.NoError(t, ValidateCronExpr(expr), "corpus generator produced invalid %q", expr)
			_, err := ParseCron(expr)
			require.NoError(t, err, "ParseCron must accept %q (ValidateCronExpr does)", expr)
		}
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

// TestParseCronDOWStepExpandingToSevenFiresSunday is the regression for #1007:
// a DOW step expression that contains no literal "7" but expands to include it
// (e.g. "1/2" → [1,3,5,7]) must still fire on Sunday. Before the fix the
// literal-"7" guard skipped normalization, "1/2" reached robfig as [1,3,5]
// (robfig bounds DOW to 0-6), and Sunday was silently dropped.
func TestParseCronDOWStepExpandingToSevenFiresSunday(t *testing.T) {
	schedule, err := ParseCron("0 0 * * 1/2")
	require.NoError(t, err)
	// "1/2" ≡ {Mon,Wed,Fri,Sun}. 2026-06-13 is a Saturday; the next match must
	// be Sunday 2026-06-14 — proving the expanded 7 was normalized to 0 rather
	// than dropped.
	from := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC), schedule.Next(from),
		"1/2 must fire on Sunday")

	// And it agrees with the explicit 0-based equivalent across a full week.
	explicit, err := ParseCron("0 0 * * 0,1,3,5")
	require.NoError(t, err)
	cur, curExplicit := from, from
	for i := 0; i < 4; i++ {
		cur = schedule.Next(cur)
		curExplicit = explicit.Next(curExplicit)
		assert.Equal(t, curExplicit, cur, "1/2 firing %d must match 0,1,3,5", i)
	}
}

// TestParseCronDOWRangeStepEndingAtSevenFiresCorrectDays is the regression for
// #1064: a DOW range-step whose end bound is 7 but whose stepped expansion
// skips the 7 (e.g. "0-7/2" → [0,2,4,6]) evaded both normalization guards, so
// the raw "0-7/2" reached robfig and was rejected ("end of range (7) above
// maximum (6)") — a validated cron the daemon could never schedule. The
// normalized form must fire exactly the expansion's days: Sun,Tue,Thu,Sat for
// "0-7/2", and Thu,Sat only for "4-7/2" (no Sunday invented by the rewrite).
func TestParseCronDOWRangeStepEndingAtSevenFiresCorrectDays(t *testing.T) {
	sched, err := ParseCron("0 0 * * 0-7/2")
	require.NoError(t, err)
	explicit, err := ParseCron("0 0 * * 0,2,4,6")
	require.NoError(t, err)
	// Walk two weeks from a Saturday: 2026-06-13.
	from := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	cur, curExplicit := from, from
	for i := 0; i < 8; i++ {
		cur = sched.Next(cur)
		curExplicit = explicit.Next(curExplicit)
		assert.Equal(t, curExplicit, cur, "0-7/2 firing %d must match 0,2,4,6", i)
	}

	// "4-7/2" expands to {Thu,Sat}; the rewrite must not add Sunday.
	sched47, err := ParseCron("0 0 * * 4-7/2")
	require.NoError(t, err)
	want := []time.Time{
		time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC), // Thursday
		time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), // Saturday
		time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC), // Thursday
	}
	cur = from
	for i, w := range want {
		cur = sched47.Next(cur)
		assert.Equal(t, w, cur, "4-7/2 firing %d must be %s", i, w.Weekday())
	}
}

// TestParseCronDOWListWithRangeEndingAtSevenKeepsSunday pins the mixed-list
// shape from the #1064 audit: "1,5-7" must schedule Mon,Fri,Sat,Sun — the
// range's Sunday (7) mapped to 0, never dropped by rewriting "5-7" to "5-6".
func TestParseCronDOWListWithRangeEndingAtSevenKeepsSunday(t *testing.T) {
	sched, err := ParseCron("0 0 * * 1,5-7")
	require.NoError(t, err)
	// 2026-06-13 is a Saturday; the very next firing must be Sunday 2026-06-14.
	from := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC), sched.Next(from),
		"1,5-7 must fire on Sunday")
	explicit, err := ParseCron("0 0 * * 0,1,5,6")
	require.NoError(t, err)
	cur, curExplicit := from, from
	for i := 0; i < 8; i++ {
		cur = sched.Next(cur)
		curExplicit = explicit.Next(curExplicit)
		assert.Equal(t, curExplicit, cur, "1,5-7 firing %d must match 0,1,5,6", i)
	}
}

// robfigReferenceParser is a standalone robfig parser configured identically to
// the package's private cronParser (standard 5-field Vixie cron). The
// DOM/DOW-semantics matrix below compares AF's ParseCron against this parser
// directly so the two can never silently drift: robfig is the ground truth for
// the AND-when-wildcard / OR-when-both-restricted rule, and if AF's DOW
// normalization ever changes a field's wildcard-ness (as #1724's "*/1"→list
// rewrite did), these sequence comparisons fail.
var robfigReferenceParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// assertSchedulesAgreeForAYear walks both schedules forward from the same start
// and asserts they produce an identical firing sequence for at least a year of
// ticks — enough to cover every month boundary and DOM/DOW interaction.
func assertSchedulesAgreeForAYear(t *testing.T, afExpr, robfigExpr string) {
	t.Helper()
	afSched, err := ParseCron(afExpr)
	require.NoError(t, err, "ParseCron(%q)", afExpr)
	refSched, err := robfigReferenceParser.Parse(robfigExpr)
	require.NoError(t, err, "robfig.Parse(%q)", robfigExpr)

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	yearEnd := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	curAF, curRef := from, from
	ticks := 0
	for {
		curAF = afSched.Next(curAF)
		curRef = refSched.Next(curRef)
		require.Equal(t, curRef, curAF,
			"AF %q and robfig %q diverged at firing %d", afExpr, robfigExpr, ticks)
		ticks++
		if curRef.After(yearEnd) || curRef.IsZero() {
			break
		}
	}
	require.Greater(t, ticks, 0, "no firings compared for %q vs %q", afExpr, robfigExpr)
}

// TestParseCronDOMWithWildcardStepDOWFiresMonthly is the direct regression for
// #1724: "0 0 15 * */1" must fire only on the 15th of each month (AND
// semantics — DOW "*/1" is a wildcard base), not every day. Before the fix
// normalizeDOWField rewrote "*/1" into the explicit list "0,1,2,3,4,5,6",
// destroying robfig's starBit and flipping the DOM/DOW rule to OR so it fired
// daily.
func TestParseCronDOMWithWildcardStepDOWFiresMonthly(t *testing.T) {
	schedule, err := ParseCron("0 0 15 * */1")
	require.NoError(t, err)
	// AF must agree with robfig parsing the identical expression.
	assertSchedulesAgreeForAYear(t, "0 0 15 * */1", "0 0 15 * */1")

	// Concretely: from mid-January the next firing is the 15th, and each
	// subsequent firing is the 15th of the following month — never a run of
	// consecutive days.
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want := []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
	}
	cur := from
	for i, w := range want {
		cur = schedule.Next(cur)
		assert.Equal(t, w, cur, "0 0 15 * */1 firing %d must be the 15th", i)
	}
}

// TestParseCronDOMDOWMatrixMatchesRobfig sweeps a matrix of DOW shapes combined
// with a restricted DOM and confirms AF's schedule matches robfig's over a full
// year for each. Wildcard DOW forms ("*", "*/1", "*/2") must AND with DOM (fire
// only on the 15th); genuinely restricted forms ("1-5", "0,3,5", and the
// wildcard-plus-range list "*/1,5-7" which is a restricted list, not a
// wildcard) must OR with DOM. The reference expression differs from the AF
// expression only where robfig can't parse AF's input directly (the Sunday
// alias 7, which AF normalizes to 0).
func TestParseCronDOMDOWMatrixMatchesRobfig(t *testing.T) {
	cases := []struct {
		afDOW  string
		refDOW string // robfig-acceptable equivalent (7→0 already applied)
	}{
		{"*", "*"},                   // wildcard: AND → 15th only
		{"*/1", "*/1"},               // wildcard base: AND → 15th only (#1724)
		{"*/2", "*/2"},               // wildcard base: AND → 15th only
		{"1-5", "1-5"},               // restricted range: OR
		{"0,3,5", "0,3,5"},           // restricted list: OR
		{"*/1,5-7", "0,1,2,3,4,5,6"}, // wildcard-step in a list is a restricted list: OR
	}
	for _, tc := range cases {
		afExpr := "0 0 15 * " + tc.afDOW
		refExpr := "0 0 15 * " + tc.refDOW
		t.Run(tc.afDOW, func(t *testing.T) {
			assertSchedulesAgreeForAYear(t, afExpr, refExpr)
		})
	}
}

// TestParseCronBothRestrictedMatchesRobfigOrSemantics pins that when both DOM
// and DOW are genuinely restricted (DOM=15, DOW=1) AF still applies Vixie OR
// semantics — fire on the 15th OR on Mondays — identically to robfig. The
// #1724 fix must not disturb this: only pure wildcard-step DOW forms change.
func TestParseCronBothRestrictedMatchesRobfigOrSemantics(t *testing.T) {
	assertSchedulesAgreeForAYear(t, "0 0 15 * 1", "0 0 15 * 1")
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
		{"*/1", "*/1"},           // pure wildcard-step: passed through so robfig keeps starBit (#1724)
		{"*/7", "*/7"},           // pure wildcard-step passes through unchanged, never a 0-based list (#1724)
		{"7", "0"},               // bare Sunday alias
		{"07", "0"},              // leading-zero Sunday alias (#743)
		{"5-7", "0,5,6"},         // range ending at 7
		{"1,7", "0,1"},           // list containing 7
		{"7/2", "0,2,4,6"},       // step base 7 must keep its step (#888): Sun,Tue,Thu,Sat
		{"07/2", "0,2,4,6"},      // leading-zero step base 7 (#888 + #743)
		{"007/2", "0,2,4,6"},     // multi-leading-zero step base 7 (#915): numeric parse, not string compare
		{"0007/2", "0,2,4,6"},    // arbitrary leading zeros on the step base (#915)
		{"1/2", "0,1,3,5"},       // expands to [1,3,5,7]; the 7 must normalize to 0 — Sunday kept (#1007)
		{"1/3", "0,1,4"},         // expands to [1,4,7]; the 7 must normalize to 0 (#1007)
		{"2/5", "0,2"},           // expands to [2,7]; the 7 must normalize to 0 (#1007)
		{"3,7/2", "0,2,3,4,6"},   // 7-step base inside a list still keeps its step (#888)
		{"0-7", "0,1,2,3,4,5,6"}, // full range incl. both Sunday aliases (#770: explicit list, never "*")
		{"1-7", "0,1,2,3,4,5,6"}, // range to the 7 alias covers the whole week as a list (#770)
		{"0-7/2", "0,2,4,6"},     // range-step ending at 7 whose expansion skips 7 — raw "0-7/2" fails robfig (#1064)
		{"0-07/2", "0,2,4,6"},    // leading zero on the range end bound: parsed numerically, not by substring (#1064 + #915)
		{"4-7/2", "4,6"},         // range-step ending at 7 that never reaches Sunday: rewritten without inventing a 0 (#1064)
		{"1-7/3", "0,1,4"},       // range-step ending at 7 that lands on 7: Sunday mapped to 0 (#1064)
		{"1,5-7", "0,1,5,6"},     // list mixing a plain value with a range ending at 7
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, normalizeDOWField(tc.in), "normalizeDOWField(%q)", tc.in)
	}
}
