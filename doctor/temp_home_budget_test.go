package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover #3466: the temp-home sweep is bounded, and a bounded sweep
// says so instead of printing a smaller problem count that reads as a healthier
// machine.
//
// The failure being guarded against is not "doctor is slow". It is that the
// SHORT output and the COMPLETE output are indistinguishable, so a reader
// calibrates on the wrong number. Every assertion below is about that
// distinguishability, which is why each truncation test has a completeness twin
// asserting the notice does NOT appear — a warning that fires unconditionally
// carries no information either.

// fillTempDir creates n empty directories that sort BEFORE anything beginning
// with "z", so a test can place a home the sweep must not reach behind them.
// os.ReadDir returns entries sorted, so this ordering is deterministic.
func fillTempDir(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		require.NoError(t, os.MkdirAll(filepath.Join(root, fmt.Sprintf("a%04d", i)), 0755))
	}
}

func TestCandidateTempHomesStopsAtTheBudget(t *testing.T) {
	root := t.TempDir()
	fillTempDir(t, root, 10)

	sweep := candidateTempHomes(root, 4)

	assert.True(t, sweep.truncated(), "a sweep that stopped at 4 of 10 directories is truncated")
	assert.Equal(t, 10, sweep.offered, "offered counts every first-level directory, including the unvisited ones")
	assert.Equal(t, 4, sweep.visited)
	assert.Len(t, sweep.candidates, 4, "each empty first-level directory contributes exactly one candidate")
}

// TestCandidateTempHomesCompleteSweepIsNotTruncated is the twin: the bound must
// not report truncation when it never bit. Without this, a notice that always
// fires would pass every truncation test above while telling users nothing.
func TestCandidateTempHomesCompleteSweepIsNotTruncated(t *testing.T) {
	root := t.TempDir()
	fillTempDir(t, root, 10)

	sweep := candidateTempHomes(root, 500)

	assert.False(t, sweep.truncated())
	assert.Equal(t, 10, sweep.offered)
	assert.Equal(t, 10, sweep.visited)
}

// TestCandidateTempHomesUnboundedWhenLimitDisabled keeps the escape hatch real:
// a caller that asks for an unbounded sweep gets one.
func TestCandidateTempHomesUnboundedWhenLimitDisabled(t *testing.T) {
	root := t.TempDir()
	fillTempDir(t, root, 10)

	for _, limit := range []int{0, -1} {
		sweep := candidateTempHomes(root, limit)
		assert.False(t, sweep.truncated(), "limit %d must not bound the sweep", limit)
		assert.Equal(t, 10, sweep.visited, "limit %d", limit)
	}
}

// TestCandidateTempHomesCountsSecondLevelAgainstTheBudget pins that the bound
// is on WORK, not on first-level entries. A single first-level directory
// holding many subdirectories is exactly the shape (Go's /tmp/TestFoo/001,
// /tmp/TestFoo/002 …) that made the sweep expensive, so it has to count.
func TestCandidateTempHomesCountsSecondLevelAgainstTheBudget(t *testing.T) {
	root := t.TempDir()
	for _, parent := range []string{"a", "b", "c"} {
		for i := 0; i < 5; i++ {
			require.NoError(t, os.MkdirAll(filepath.Join(root, parent, fmt.Sprintf("%03d", i)), 0755))
		}
	}

	// "a" alone yields 6 candidates (itself + 5 children), already past a
	// budget of 4, so the sweep stops after that one first-level entry.
	sweep := candidateTempHomes(root, 4)
	assert.Equal(t, 1, sweep.visited, "the bound is checked at a first-level boundary, after expanding one entry")
	assert.Equal(t, 3, sweep.offered)
	assert.Len(t, sweep.candidates, 6, "the entry being expanded when the budget ran out is still expanded fully")
	assert.True(t, sweep.truncated())
}

// stageTruncatedSweep puts a genuinely stale AF home BEHIND enough filler
// directories that the given budget cannot reach it. The home is real — old
// enough, marked, and with a takeable lock — so the only reason doctor does not
// report it is that it never looked.
func stageTruncatedSweep(t *testing.T, budget int) (Options, *Report) {
	t.Helper()
	root := t.TempDir()
	fillTempDir(t, root, 12)
	makeOldTempAFHome(t, root, "zz-abandoned-home")
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	opts := macLikeTempHomeOptions(t, root, false)
	opts.MaxTempHomeCandidates = budget
	ctx, err := newScanContext(opts)
	require.NoError(t, err)
	report := &Report{}
	checkStaleTempHomes(ctx, report)
	return ctx.opts, report
}

func findingsFor(r *Report, check string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

// TestTruncatedSweepReportsThatItDidNotFinish is the load-bearing test for
// #3466. The staged home is unreachable within the budget, so doctor reports
// nothing about it — and that silence must be accompanied by a notice, or the
// run is a smaller problem count masquerading as a healthier box.
func TestTruncatedSweepReportsThatItDidNotFinish(t *testing.T) {
	_, report := stageTruncatedSweep(t, 4)

	require.Empty(t, findingsFor(report, "stale-temp-home"),
		"precondition: the budget must actually prevent the home from being assessed, "+
			"or this test proves nothing about truncation")

	notices := findingsFor(report, "temp-home-scan")
	require.Len(t, notices, 1, "a sweep that stopped early must say so exactly once")
	assert.Equal(t, StatusWarn, notices[0].Severity)
	assert.False(t, notices[0].Actionable,
		"not finishing is an UNKNOWN, not a proven unhealthy condition; it must not flip the exit code")
	assert.Contains(t, notices[0].Detail, "did NOT assess every temp home")
	assert.Contains(t, notices[0].Detail, "4 of the 13 directories",
		"the notice must name how far it got, so the reader can calibrate the counts below it")
	assert.Contains(t, notices[0].Detail, "its 4-candidate budget",
		"the notice must name the CONFIGURED budget, not the count it happened to stop on")
	assert.Equal(t, []string{"stale-temp-home"}, report.Incomplete)
}

// TestTruncationNoticeIsNotCollapsedAwayIsTheWholePoint: the notice must not
// carry the stale-temp-home slug, because those findings collapse into a single
// summary row — which would fold the warning that the scan was incomplete into
// the very count it exists to qualify.
func TestTruncationNoticeIsNotCollapsedAway(t *testing.T) {
	_, report := stageTruncatedSweep(t, 4)

	rows := renderRows(report, false, false)
	var found bool
	for _, row := range rows {
		if row.name == "temp-home-scan" {
			found = true
		}
	}
	assert.True(t, found, "the truncation notice must survive the default (collapsed) rendering")
	assert.False(t, collapsibleFinding("temp-home-scan"),
		"if this ever becomes collapsible the notice disappears into the row it qualifies")
}

// TestCompleteSweepAssessesTheHomeAndReportsNothingIncomplete is the twin of the
// truncation test above, on identical staging. It proves two things at once:
// the home really was findable (so the truncation test's silence was caused by
// the budget and not by broken staging), and the notice does not fire when the
// sweep finished.
func TestCompleteSweepAssessesTheHomeAndReportsNothingIncomplete(t *testing.T) {
	_, report := stageTruncatedSweep(t, 500)

	require.Len(t, findingsFor(report, "stale-temp-home"), 1,
		"with a budget large enough to reach it, the very same home IS assessed")
	assert.Empty(t, findingsFor(report, "temp-home-scan"),
		"a sweep that finished must not claim it was cut short")
	assert.Empty(t, report.Incomplete)
}

func TestSummaryLineNamesAnIncompleteRun(t *testing.T) {
	complete := &Report{}
	assert.NotContains(t, summaryLine(complete, renderRows(complete, false, false), false), "INCOMPLETE")

	incomplete := &Report{}
	incomplete.markIncomplete("stale-temp-home")
	line := summaryLine(incomplete, renderRows(incomplete, false, false), false)
	assert.Contains(t, line, "INCOMPLETE")
	assert.Contains(t, line, "stale-temp-home", "the summary must name WHICH check did not finish")
	assert.Contains(t, line, "lower bound")
}

func TestMarkIncompleteDoesNotRepeatACheck(t *testing.T) {
	r := &Report{}
	r.markIncomplete("stale-temp-home")
	r.markIncomplete("stale-temp-home")
	assert.Equal(t, []string{"stale-temp-home"}, r.Incomplete)
}

// TestStaleTempHomeFindingsCollapse covers the other half of #3466: 51 one-line
// WARN rows, one per directory, buried every other finding in the report.
func TestStaleTempHomeFindingsCollapse(t *testing.T) {
	report := &Report{}
	for i := 0; i < 7; i++ {
		report.addActionableFinding(Finding{
			Check:     "stale-temp-home",
			Detail:    fmt.Sprintf("home %d is abandoned", i),
			FixAction: "remove it",
			Severity:  StatusWarn,
		})
	}

	collapsed := renderRows(report, false, false)
	require.Len(t, collapsed, 1, "the default view collapses them into one row")
	assert.Equal(t, "stale-temp-homes", collapsed[0].name)
	assert.Contains(t, collapsed[0].detail, "7 agent-factory homes")
	assert.Contains(t, collapsed[0].detail, "7 safe to remove")
	assert.Contains(t, collapsed[0].remediation, "--verbose")

	verbose := renderRows(report, false, true)
	assert.Len(t, verbose, 7, "--verbose still shows every home, so nothing is lost by collapsing")
}

// TestUnresolvedCountSurvivesCollapsing guards the trap the process checks
// already document: collapsing changes ROWS, never the number of underlying
// actionable issues, so the exit code cannot drift from what was found.
func TestUnresolvedCountSurvivesCollapsing(t *testing.T) {
	report := &Report{}
	for i := 0; i < 7; i++ {
		report.addActionableFinding(Finding{
			Check: "stale-temp-home", Detail: fmt.Sprintf("home %d", i),
			FixAction: "remove it", Severity: StatusWarn,
		})
	}
	assert.Equal(t, 7, report.UnresolvedCount(),
		"seven abandoned homes are seven issues however many rows they render as")
}

func TestJSONSummaryCarriesIncomplete(t *testing.T) {
	complete := BuildJSONReport(&Report{}, false, false)
	assert.Empty(t, complete.Summary.Incomplete, "a complete run carries no incomplete list")

	r := &Report{}
	r.markIncomplete("stale-temp-home")
	got := BuildJSONReport(r, false, false)
	assert.Equal(t, []string{"stale-temp-home"}, got.Summary.Incomplete)
	assert.Zero(t, got.Summary.Unresolved,
		"a truncated sweep establishes nothing unhealthy, so a probe reading only "+
			"unresolved would call this box clean — which is why incomplete exists")
}

// TestDefaultBudgetDoesNotTruncateAnOrdinaryTempDir pins the default as a
// safety valve rather than routine behavior: a healthy temp dir must produce
// byte-identical output to an unbounded run.
func TestDefaultBudgetDoesNotTruncateAnOrdinaryTempDir(t *testing.T) {
	root := t.TempDir()
	fillTempDir(t, root, 200)

	sweep := candidateTempHomes(root, defaultMaxTempHomeCandidates)
	assert.False(t, sweep.truncated(),
		"200 directories is an ordinary temp dir and must not trip a bound meant for pathological ones")
	assert.GreaterOrEqual(t, defaultMaxTempHomeCandidates, 50000,
		"lowering this reduces what doctor assesses on a real machine that works today: "+
			"at 20000 the reporting box in #3466 truncated at 15194 of 21762 directories and "+
			"found 28 abandoned homes instead of 49")
}

func TestTruncationNoticeNamesTheTempDir(t *testing.T) {
	opts, report := stageTruncatedSweep(t, 4)
	notices := findingsFor(report, "temp-home-scan")
	require.Len(t, notices, 1)
	assert.True(t, strings.Contains(notices[0].Detail, opts.TempDir),
		"the notice must name the directory it gave up on, not just complain abstractly")
	assert.Contains(t, notices[0].Remediation, opts.TempDir)
}

// TestCollapsingPreservesDeclaredSeverity guards a regression this PR
// introduced and then fixed: collapsing must change how many ROWS a class
// occupies, never what it says about the machine.
//
// stale-temp-home findings declare StatusWarn — an abandoned directory is not a
// broken machine — but the collapse rule inherited from the process checks
// escalates any group with a fixable member to FAIL. On the reporting box that
// turned 49 WARN rows into one FAIL row purely by being rendered differently.
func TestCollapsingPreservesDeclaredSeverity(t *testing.T) {
	report := &Report{}
	report.addActionableFinding(Finding{
		Check: "stale-temp-home", Detail: "provably free", FixAction: "remove it",
		fix: func() error { return nil }, Severity: StatusWarn,
	})
	report.addAdvisoryFinding(Finding{
		Check: "stale-temp-home", Detail: "cannot prove it is unused", Severity: StatusWarn,
	})

	rows := renderRows(report, false, false)
	require.Len(t, rows, 1)
	assert.Equal(t, StatusWarn, rows[0].status,
		"a fixable abandoned home is still a WARN; collapsing must not escalate it to FAIL")
	assert.True(t, rows[0].actionable, "the proven one is still actionable, which is a separate axis from severity")
}

// TestCollapsingStillEscalatesAFailedFix keeps the other direction honest: a fix
// that was attempted and failed is a real failure and must show as one.
func TestCollapsingStillEscalatesAFailedFix(t *testing.T) {
	report := &Report{}
	report.addActionableFinding(Finding{
		Check: "stale-temp-home", Detail: "provably free", FixAction: "remove it",
		Severity: StatusWarn, FixErr: assertAnError(),
	})

	rows := renderRows(report, false, true)
	require.Len(t, rows, 1)
	assert.Equal(t, StatusFail, rows[0].status)
}

// TestProcessCollapseSeverityUnchanged pins that the severity rule above did not
// disturb the checks it was originally written for: they declare no Severity, so
// they keep deriving FAIL from having something fixable.
func TestProcessCollapseSeverityUnchanged(t *testing.T) {
	report := &Report{}
	report.addActionableFinding(Finding{
		Check: "orphaned-process", Detail: "pid 1 is orphaned", FixAction: "kill it",
		fix: func() error { return nil },
	})

	rows := renderRows(report, false, false)
	require.Len(t, rows, 1)
	assert.Equal(t, StatusFail, rows[0].status,
		"orphaned-process declares no severity and must keep the fixable-implies-FAIL rule")
}

func assertAnError() error { return fmt.Errorf("permission denied") }
