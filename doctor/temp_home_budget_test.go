package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
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

	assert.True(t, sweep.truncated(), "a sweep that stopped partway through 10 directories is truncated")
	assert.Equal(t, 10, sweep.offered, "offered counts every first-level directory, including the unvisited ones")
	assert.Len(t, sweep.candidates, 4, "each empty first-level directory contributes exactly one candidate")
	// 3, not 4. The fourth directory was recorded as a candidate and then the
	// budget stopped the sweep before its own children were read — so whether
	// it holds nested homes is unknown, and an entry we did not finish is not
	// one we visited. Counting it would overstate coverage by exactly the
	// entry the sweep gave up on.
	assert.Equal(t, 3, sweep.visited)
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

	sweep := candidateTempHomes(root, 4)
	assert.LessOrEqual(t, len(sweep.candidates), 4, "the budget bounds candidates, second-level ones included")
	assert.Equal(t, 3, sweep.offered)
	assert.True(t, sweep.truncated())
}

// TestCandidateTempHomesBoundsAHugeSingleDirectory is the P1 from #3568's
// review, and it is the case the budget exists for.
//
// The bound used to be checked only BETWEEN first-level entries, so one
// directory was always expanded in full once started. A single /tmp/TestFoo
// holding a million children therefore produced a million candidates despite a
// 50,000 budget — and because it was the only first-level entry, visited(1)
// equalled offered(1), so truncated() was false and nothing was reported
// either. Unbounded work AND silence: both halves of #3466 back at once,
// through the exact directory shape that made the sweep expensive.
//
// The earlier version of this test asserted the overshoot was intentional
// ("the entry being expanded when the budget ran out is still expanded
// fully"). That was the defect written down as a requirement: harmless for the
// five children it used, unbounded for a million.
func TestCandidateTempHomesBoundsAHugeSingleDirectory(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "TestFoo", fmt.Sprintf("%03d", i)), 0755))
	}

	sweep := candidateTempHomes(root, 10)

	assert.LessOrEqual(t, len(sweep.candidates), 10,
		"one first-level directory must not be able to blow past the budget by having many children")
	assert.True(t, sweep.truncated(),
		"a directory that was only PARTLY expanded means the sweep did not see everything, "+
			"and a sweep that stops without saying so is the whole defect")
	assert.Equal(t, 0, sweep.visited,
		"a partly expanded entry is not a visited one; counting it would make visited==offered and hide the truncation")
}

// TestCandidateTempHomesUnreadableTempDirIsNotAnEmptyOne: a temp dir that
// cannot be listed has told us NOTHING, which is not the same as telling us it
// holds no homes. Reporting the latter is this repo's signature failure.
func TestCandidateTempHomesUnreadableTempDirIsNotAnEmptyOne(t *testing.T) {
	sweep := candidateTempHomes(filepath.Join(t.TempDir(), "does-not-exist"), 100)

	assert.True(t, sweep.unreadable)
	assert.True(t, sweep.truncated(), "an unreadable temp dir must not present as a completed sweep")
	assert.Empty(t, sweep.candidates)
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
	// 3, not 4: the fourth directory was appended as a candidate and then the
	// budget stopped the sweep before its children were read, so it is not one
	// the run FINISHED — only entries expanded in full count.
	assert.Contains(t, notices[0].Detail, "3 of the 13 directories",
		"the notice must name how far it got, so the reader can calibrate the counts below it")
	assert.Contains(t, notices[0].Detail, "a 4-candidate budget",
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

// TestEveryAbortedScanIsMarkedIncomplete covers the second P2 from #3568's
// review: the completeness signal was set ONLY when the candidate budget bit,
// while two other paths abandon the check just as thoroughly.
//
// Both leave summary.incomplete empty in the old code, so a probe following the
// help text this PR added — "unresolved == 0 AND incomplete is empty means
// healthy" — reads a scan that never ran as a completed one.
func TestEveryAbortedScanIsMarkedIncomplete(t *testing.T) {
	t.Run("an unreadable temp dir", func(t *testing.T) {
		opts := macLikeTempHomeOptions(t, filepath.Join(t.TempDir(), "gone"), false)
		ctx, err := newScanContext(opts)
		require.NoError(t, err)
		report := &Report{}
		checkStaleTempHomes(ctx, report)

		assert.Equal(t, []string{"stale-temp-home"}, report.Incomplete,
			"doctor could not list the temp dir at all; that is not a clean bill of health")
	})

	t.Run("an unreadable tmux session list", func(t *testing.T) {
		root := t.TempDir()
		makeOldTempAFHome(t, root, "abandoned")
		opts := macLikeTempHomeOptions(t, root, false)
		opts.Exec = cmd_test.MockCmdExec{
			RunFunc:    func(*exec.Cmd) error { return nil },
			OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, fmt.Errorf("tmux will not answer") },
		}
		ctx, err := newScanContext(opts)
		require.NoError(t, err)
		report := &Report{}
		checkStaleTempHomes(ctx, report)

		require.Empty(t, findingsFor(report, "stale-temp-home"),
			"precondition: this path assesses no home at all")
		assert.Equal(t, []string{"stale-temp-home"}, report.Incomplete,
			"the check returned before enumerating anything, so the run did not look")
	})
}

// TestScanWideAdvisoryIsNotCollapsedIntoAPhantomHome is the first P2 from
// #3568's review, and it was a fabrication rather than a formatting slip.
//
// When liveTmuxHomes fails, checkStaleTempHomes files ONE scan-wide advisory
// explaining that nothing could be assessed. Filed under the per-home
// stale-temp-home slug, the collapse rewrote it as "1 agent-factory home
// abandoned under the temp dir" — inventing a home that was never found, and
// deleting the reason the scan did no work.
func TestScanWideAdvisoryIsNotCollapsedIntoAPhantomHome(t *testing.T) {
	root := t.TempDir()
	makeOldTempAFHome(t, root, "abandoned")
	opts := macLikeTempHomeOptions(t, root, false)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, fmt.Errorf("tmux will not answer") },
	}
	ctx, err := newScanContext(opts)
	require.NoError(t, err)
	report := &Report{}
	checkStaleTempHomes(ctx, report)

	rows := renderRows(report, false, false)
	for _, row := range rows {
		assert.NotContains(t, row.detail, "abandoned under the temp dir",
			"no home was assessed, so no row may claim one was found")
	}
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].detail, "no temp home could be assessed",
		"the reason the scan did no work must survive to the default rendering")
}

// TestSweepStopsReadingAHugeDirectoryNotJustRecordingIt is the second-round P1
// from #3568's review.
//
// Bounding the candidates RECORDED is not the same as bounding the work. The
// first fix still called os.ReadDir, which reads every entry and filename-sorts
// them before returning, so a directory with millions of children cost memory
// and time proportional to all of them — and could hang or be OOM-killed before
// the incompleteness notice was ever emitted.
//
// The candidate count cannot show this: from the outside, a sweep that reads a
// million entries and records ten looks exactly like one that read ten. So this
// counts the READ, through the scanDirBatch seam that exists for the purpose.
func TestSweepStopsReadingAHugeDirectoryNotJustRecordingIt(t *testing.T) {
	root := t.TempDir()
	const children = 500
	for i := 0; i < children; i++ {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "TestFoo", fmt.Sprintf("%04d", i)), 0755))
	}

	prevBatch := tempHomeChildBatch
	tempHomeChildBatch = 32
	t.Cleanup(func() { tempHomeChildBatch = prevBatch })

	read := 0
	prev := scanDirBatch
	scanDirBatch = func(f *os.File, n int) ([]os.DirEntry, error) {
		entries, err := prev(f, n)
		read += len(entries)
		return entries, err
	}
	t.Cleanup(func() { scanDirBatch = prev })

	sweep := candidateTempHomes(root, 10)

	assert.LessOrEqual(t, len(sweep.candidates), 10)
	assert.Less(t, read, children,
		"the sweep must stop READING the directory, not just stop recording candidates from it")
	assert.True(t, sweep.truncated())
}

// TestUnlistableFirstLevelDirectoryIsIncomplete is the second-round P2: a
// first-level directory whose own listing fails hides everything nested inside
// it. Counting it as visited made it indistinguishable from one assessed in
// full, so a temp dir where EVERY entry failed this way reported a complete run
// having looked at nothing.
func TestUnlistableFirstLevelDirectoryIsIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can list a 0000 directory, so the condition cannot be staged")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	require.NoError(t, os.MkdirAll(filepath.Join(locked, "nested"), 0755))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	sweep := candidateTempHomes(root, 0)

	assert.Equal(t, 1, sweep.offered)
	assert.Equal(t, 1, sweep.unlistable)
	assert.Equal(t, 0, sweep.visited,
		"a directory we could not read is not one we assessed")
	assert.True(t, sweep.truncated(),
		"if this reads complete, a temp dir of unreadable directories reports a clean run having seen nothing")
}

// TestUnlistableDirectoryReachesTheReport is the end-to-end half: the sweep
// noticing is worthless if the run still prints as complete.
func TestUnlistableDirectoryReachesTheReport(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can list a 0000 directory, so the condition cannot be staged")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	require.NoError(t, os.MkdirAll(filepath.Join(locked, "nested"), 0755))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	opts := macLikeTempHomeOptions(t, root, false)
	ctx, err := newScanContext(opts)
	require.NoError(t, err)
	report := &Report{}
	checkStaleTempHomes(ctx, report)

	assert.Equal(t, []string{"stale-temp-home"}, report.Incomplete)
	notices := findingsFor(report, "temp-home-scan")
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail, "could not be listed at all",
		"the notice must say WHY it did not see everything, not just that it did not")
}

// TestTruncationNoticeReportsBothProgressQuantities is the third second-round
// finding. When the budget runs out partway through one huge directory,
// visited is 0 — so a notice phrased only in first-level terms says the run
// "inspected 0 of the 1 directories" right after inspecting thousands of paths
// inside it, which is the wrong progress measurement in precisely the case the
// notice exists to explain.
func TestTruncationNoticeReportsBothProgressQuantities(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 40; i++ {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "TestFoo", fmt.Sprintf("%03d", i)), 0755))
	}
	sweep := candidateTempHomes(root, 10)
	require.True(t, sweep.truncated())
	require.Equal(t, 0, sweep.visited, "precondition: the budget ran out inside the only first-level directory")

	detail := tempHomeSweepTruncationDetail(root, sweep, 10)

	assert.Contains(t, detail, "0 of the 1 directories")
	assert.Contains(t, detail, "10 candidate directories",
		"the candidate count is what actually shows the work done; without it the notice reads as having done nothing")
	assert.Contains(t, detail, "a 10-candidate budget")
}

// TestSweepStopsReadingADirectoryFullOfFiles is the third-round P2, and it is
// the case my earlier regression test could not see.
//
// The candidate budget bounds what is RECORDED. A first-level directory holding
// a million ordinary files and no subdirectories contributes no candidates at
// all, so that budget never fires and the directory is read to EOF however
// small the limit is. My earlier test populated only subdirectories, so every
// entry it read became a candidate and the two quantities moved together —
// which is exactly why it could not distinguish "bounded work" from "bounded
// recording".
func TestSweepStopsReadingADirectoryFullOfFiles(t *testing.T) {
	root := t.TempDir()
	noisy := filepath.Join(root, "TestFoo")
	require.NoError(t, os.MkdirAll(noisy, 0755))
	const files = 400
	for i := 0; i < files; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(noisy, fmt.Sprintf("f%04d", i)), nil, 0644))
	}

	withSmallReadBudget(t, 64)
	read := countDirReads(t)

	sweep := candidateTempHomes(root, 50000)

	assert.Empty(t, sweep.candidates[1:],
		"precondition: a directory of plain files yields no candidates beyond itself, "+
			"so the candidate budget can never stop this read")
	assert.Less(t, *read, files,
		"the read budget must stop a directory of files; the candidate budget cannot see it at all")
	assert.True(t, sweep.truncated())
}

// TestSweepStopsReadingAHugeTempRoot is the third-round P1: the temp ROOT was
// still read with os.ReadDir, which reads and sorts every entry before
// returning — and the root is read before a single candidate exists, so no
// candidate budget can bound it. A root with millions of entries could hang or
// OOM the process before any incompleteness notice was emitted.
func TestSweepStopsReadingAHugeTempRoot(t *testing.T) {
	root := t.TempDir()
	const entries = 400
	fillTempDir(t, root, entries)

	withSmallReadBudget(t, 64)
	read := countDirReads(t)

	sweep := candidateTempHomes(root, 50000)

	assert.Less(t, *read, entries, "the root listing itself must be bounded")
	assert.True(t, sweep.rootPartial)
	assert.True(t, sweep.truncated())
	assert.Less(t, sweep.offered, entries,
		"offered can only count directories we got as far as naming")
}

// TestTruncationNoticeDoesNotFabricateADenominator: when the root was only
// partly listed, offered is a LOWER BOUND. Printing it as a total would invent
// a denominator — the same error as inventing a finding, one level up.
func TestTruncationNoticeDoesNotFabricateADenominator(t *testing.T) {
	root := t.TempDir()
	fillTempDir(t, root, 400)
	withSmallReadBudget(t, 64)

	sweep := candidateTempHomes(root, 50000)
	require.True(t, sweep.rootPartial)

	detail := tempHomeSweepTruncationDetail(root, sweep, 50000)
	assert.Contains(t, detail, "at least")
	assert.Contains(t, detail, "too large to list in full")
}

// TestCompleteSweepStillReportsAnExactDenominator is the twin: an ordinary temp
// root must not be described with the hedge.
func TestCompleteSweepStillReportsAnExactDenominator(t *testing.T) {
	root := t.TempDir()
	fillTempDir(t, root, 12)

	sweep := candidateTempHomes(root, 4)
	require.True(t, sweep.truncated())
	require.False(t, sweep.rootPartial, "precondition: the root itself was listed in full")

	detail := tempHomeSweepTruncationDetail(root, sweep, 4)
	assert.NotContains(t, detail, "at least",
		"the denominator is exact here; hedging it would understate what doctor knows")
	assert.Contains(t, detail, "of the 12 directories")
}

func withSmallReadBudget(t *testing.T, n int) {
	t.Helper()
	prevBudget, prevBatch := tempHomeReadBudget, tempHomeChildBatch
	tempHomeReadBudget, tempHomeChildBatch = n, 16
	t.Cleanup(func() { tempHomeReadBudget, tempHomeChildBatch = prevBudget, prevBatch })
}

// countDirReads returns a counter of every directory entry the sweep actually
// reads, through the seam that exists so this quantity is observable at all.
func countDirReads(t *testing.T) *int {
	t.Helper()
	read := 0
	prev := scanDirBatch
	scanDirBatch = func(f *os.File, n int) ([]os.DirEntry, error) {
		entries, err := prev(f, n)
		read += len(entries)
		return entries, err
	}
	t.Cleanup(func() { scanDirBatch = prev })
	return &read
}

// ---- fourth review round on #3568 ----

// TestNegativeLimitAlsoDisablesTheReadBudget: Options.MaxTempHomeCandidates
// documents that a negative value disables the bound and accepts an unbounded
// sweep. The read budget was armed unconditionally, so a caller that opted out
// of one bound was silently truncated by another it had never been told about —
// the documented escape hatch did not exist.
func TestNegativeLimitAlsoDisablesTheReadBudget(t *testing.T) {
	root := t.TempDir()
	noisy := filepath.Join(root, "TestFoo")
	require.NoError(t, os.MkdirAll(filepath.Join(noisy, "home"), 0755))
	const files = 300
	for i := 0; i < files; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(noisy, fmt.Sprintf("f%04d", i)), nil, 0644))
	}

	withSmallReadBudget(t, 32)

	sweep := candidateTempHomes(root, -1)

	assert.False(t, sweep.truncated(),
		"a caller that disabled the bound asked for a COMPLETE sweep and must get one")
	assert.False(t, sweep.hitReadBudget)
	assert.Equal(t, 1, sweep.visited)
	assert.Contains(t, sweep.candidates, filepath.Join(noisy, "home"),
		"the nested home behind those files must still be found")
}

// TestTruncationNoticeNamesTheBudgetThatActuallyFired: a directory of plain
// files exhausts the READ budget while recording a single candidate, so
// blaming the candidate budget points the reader at a limit that never came
// near firing.
func TestTruncationNoticeNamesTheBudgetThatActuallyFired(t *testing.T) {
	root := t.TempDir()
	noisy := filepath.Join(root, "TestFoo")
	require.NoError(t, os.MkdirAll(noisy, 0755))
	for i := 0; i < 300; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(noisy, fmt.Sprintf("f%04d", i)), nil, 0644))
	}

	withSmallReadBudget(t, 32)
	sweep := candidateTempHomes(root, 50000)

	require.True(t, sweep.hitReadBudget)
	require.False(t, sweep.hitCandidateLimit,
		"precondition: one candidate against a 50000 limit — the candidate budget cannot be what stopped this")

	detail := tempHomeSweepTruncationDetail(root, sweep, 50000)
	assert.Contains(t, detail, "read budget")
	assert.NotContains(t, detail, "50000-candidate budget",
		"naming a bound that never fired sends the reader to the wrong number")
}

// TestRootThatCannotBeListedIsUnreadableNotPartial: os.Open succeeding and the
// first ReadDir failing is an unreadable temp dir, not a large one. Reporting
// "too large to list in full" would hide a configuration fault (TempDir naming
// a regular file) behind advice to clean /tmp.
func TestRootThatCannotBeListedIsUnreadableNotPartial(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0644))

	sweep := candidateTempHomes(file, 50000)

	assert.True(t, sweep.unreadable, "nothing was listed, so this is an unreadable root")
	assert.False(t, sweep.rootPartial, "and it is not a partially read one")

	report := &Report{}
	reportTempHomeSweepTruncation(report, file, sweep, 50000)
	notices := findingsFor(report, "temp-home-scan")
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail, "could not be listed at all")
	assert.NotContains(t, notices[0].Detail, "too large to list in full")
}

// TestChildrenAreSortedOnACompleteExpansion pins that an ORDINARY, untruncated
// scan is reproducible. os.ReadDir sorted; File.ReadDir does not, so streaming
// made candidate order depend on filesystem order — changing --verbose and
// verbose JSON between runs on machines where nothing was truncated at all.
//
// The seam returns entries in reverse order rather than trusting the
// filesystem to hand back something unsorted, so this stages the condition
// deterministically instead of hoping for it.
func TestChildrenAreSortedOnACompleteExpansion(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "TestFoo")
	for _, name := range []string{"aaa", "bbb", "ccc", "ddd", "eee"} {
		require.NoError(t, os.MkdirAll(filepath.Join(parent, name), 0755))
	}

	prev := scanDirBatch
	scanDirBatch = func(f *os.File, n int) ([]os.DirEntry, error) {
		entries, err := prev(f, n)
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
		return entries, err
	}
	t.Cleanup(func() { scanDirBatch = prev })

	sweep := candidateTempHomes(root, 50000)
	require.False(t, sweep.truncated(), "precondition: nothing here is truncated")

	children := sweep.candidates[1:] // [0] is the parent itself
	assert.True(t, sort.StringsAreSorted(children),
		"a complete scan must produce a reproducible candidate order, got %v", children)
}

// TestRootTruncationStillAssessesTheNamesAlreadyRead is the worst of the
// fourth-round findings. When the temp root exhausted the read budget, the
// loop broke immediately — so a huge root reported an incomplete scan and then
// assessed NOTHING, not even the first-level names it had already paid to
// read. Those names are in hand and each may itself be an abandoned home.
func TestRootTruncationStillAssessesTheNamesAlreadyRead(t *testing.T) {
	root := t.TempDir()
	fillTempDir(t, root, 300)

	withSmallReadBudget(t, 32)
	sweep := candidateTempHomes(root, 50000)

	require.True(t, sweep.rootPartial, "precondition: the root itself was cut short")
	assert.NotEmpty(t, sweep.candidates,
		"the first-level names already read must still be assessed; expansion stops, assessment does not")
	assert.GreaterOrEqual(t, len(sweep.candidates), sweep.offered,
		"every name the root listing produced should have become a candidate")
	assert.Zero(t, sweep.visited,
		"none of them was expanded, so none counts as finished")
}
