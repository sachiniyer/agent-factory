package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin the fix for the defect that a per-session `tmux list-panes`
// failure on a LIVE session must not be read as a proven-empty pane tree.
//
// Before the fix, checkOrphanedProcesses consumed the best-effort
// tmux.SessionProcessTrees, which returns nil and DROPS the completeness error
// on any list-panes failure. The live-session arm read that nil as "tree
// enumerated, empty", so with inSession empty every marked process of the live
// session fell through to an escaped-process advisory — a concrete leak finding
// asserted on a read that never returned. The same dropped-error call fed
// checkRunawayChildren, where a blind session's pane-listing failure was
// swallowed (no runaway finding AND no blindness row).
//
// The fix consumes the evidence-bearing tmux.CaptureSessionProcessTrees at both
// sites, so a read that cannot answer produces a single blindness row rather
// than either false escapes or a silent "no runaway processes".

// TestPaneListingFailureReportsBlindnessNotFalseEscapes is the regression for the
// lead defect. It stages a REAL in-pane child of a real af_ tmux session — so by
// construction the child is inside the pane tree — and runs `af doctor` twice.
//
//   - Baseline: with the pane tree READABLE, the in-pane child is provably inside
//     the tree and is NOT reported as an escapee.
//   - Failure pass: `tmux ls` is delegated to the real executor (and still lists
//     the session, so it is VERIFIABLY live), but `tmux list-panes` returns a plain
//     fmt.Errorf — the live-session generic-failure form, NOT the vanished form
//     (a plain error is not an *exec.ExitError, so missingTmuxSession rejects it).
//     The same in-pane child, which changed in no way between the two passes, must
//     NOT be reported as escaped; instead a single blindness row names the
//     session, because the only evidence is that the pane tree could not be read.
//
// The contrast before the fix was the proof of the bug: the same child was NOT
// an escapee when the read succeeded and WAS when it failed, which is only
// possible if the failure branch manufactured the finding.
func TestPaneListingFailureReportsBlindnessNotFalseEscapes(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-blind-panes"
	dir := t.TempDir()
	script := filepath.Join(dir, "spawn-child")
	pidFile := filepath.Join(dir, "child.pid")
	require.NoError(t, os.WriteFile(script, []byte(
		"#!/bin/sh\nsleep 300 &\nprintf '%s\\n' \"$!\" > \"$1\"\nwait \"$!\"\nexec sleep 300\n",
	), 0o700))
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name,
		"-e", tmux.EnvMarkerSession+"="+name, script, pidFile).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	var paneChildPID int
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			return false
		}
		_, scanErr := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &paneChildPID)
		return scanErr == nil && paneChildPID > 1
	}, 5*time.Second, 10*time.Millisecond, "pane never published child pid")
	// Wait for the child's marker to be readable through proctree before doctor is
	// asked to classify it — the fork→exec window can race the environ read (see
	// TestLiveSessionTransientChildIsNotReportedAsEscaped).
	require.Eventually(t, func() bool {
		_, st := proctree.LookupEnv(paneChildPID, tmux.EnvMarkerSession)
		return st == proctree.EnvFound
	}, 10*time.Second, 10*time.Millisecond, "pane child marker never readable")

	// Baseline: with the pane tree READABLE, the in-pane child is NOT an
	// escapee — it is provably inside the tree.
	report, err := Run(testOptions(t, false, paneChildPID))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "escaped-process"),
		"baseline: a real in-pane child whose pane tree is readable is not an escapee")
	require.Empty(t, findBlindnessRows(report, name),
		"baseline: a readable pane tree must not produce a blindness row")

	// The session listing answered authoritatively in the baseline, so the
	// session is verifiably live — the failure pass below is a per-session
	// list-panes failure, not a server-wide blindness.
	require.True(t, tmuxInspectionPassed(report), "baseline: the tmux server must be readable for the contrast to mean anything")

	// Now make tmux list-panes FAIL for this one session while tmux ls still
	// succeeds (the session is live), so CaptureSessionProcessTrees returns a
	// non-nil error. The error is a plain fmt.Errorf — not an *exec.ExitError
	// with matching stderr — so it is NOT classified as
	// ErrSessionVanishedBeforeCapture; it is the live-session generic-failure
	// form. Its text deliberately coincides with the vanished string to prove
	// the classification comes from the error SHAPE, not a substring match.
	realExec := cmd.MakeExecutor()
	opts := testOptions(t, false, paneChildPID)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) > 1 && c.Args[1] == "list-panes" {
				return nil, fmt.Errorf("can't find session: %s", name)
			}
			return realExec.Output(c)
		},
	}

	report, err = Run(opts)
	require.NoError(t, err)

	// The session is still VERIFIABLY live: tmux ls (delegated to the real
	// executor) still lists it, so the failure is a per-session list-panes
	// error, not a server-wide blindness that checkTmuxInspection would FAIL.
	require.True(t, tmuxInspectionPassed(report),
		"the session must still be verifiably live, so the failure is per-session, not server-wide")

	// The lead defect: NO false escaped-process findings for the in-pane child.
	require.Empty(t, findByCheck(report, "escaped-process"),
		"a live session's real in-pane child must not be reported as having 'escaped the pane tree' "+
			"only because tmux list-panes failed for that session — a failed read must not be "+
			"rendered as a concrete, false escape finding")

	// The fix: a single blindness row names the session, in place of the row of
	// false escapes. It is advisory (Warn, not Problem) so it does not drive the
	// exit code the way an unreadable process table or session list does.
	blind := findBlindnessRows(report, name)
	require.NotEmpty(t, blind,
		"a failed pane-tree read must produce a blindness row rather than silence or false escapes")
	require.Equal(t, StatusWarn, blind[0].Status,
		"per-session pane-tree blindness is advisory — it must not count toward the exit code")
	require.False(t, blind[0].Problem,
		"a read that could not answer is UNKNOWN, not an established health failure")
	require.Contains(t, blind[0].Detail, name,
		"the blindness row must name which session was blind")
}

// TestPaneListingFailureReportsRunawayBlindnessNotSilence is the regression for
// the related defect in checkRunawayChildren: the same dropped-error call is its
// only data source, and before the fix a blind session's pane-listing failure
// was swallowed — no runaway finding AND no blindness row, so the function
// rendered the same as "no runaway processes" for a session it never inspected.
//
// With the fix, a per-session list-panes failure produces a runaway-cpu
// blindness row instead of silence. The live session is staged the same way as
// the escaped test above; the snapshot carries the in-pane child only, so no
// runaway could be reported regardless, and the only observable difference is
// whether the blindness is NAMED rather than swallowed.
func TestPaneListingFailureReportsRunawayBlindnessNotSilence(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-runaway-blind"
	dir := t.TempDir()
	script := filepath.Join(dir, "spawn-child")
	pidFile := filepath.Join(dir, "child.pid")
	require.NoError(t, os.WriteFile(script, []byte(
		"#!/bin/sh\nsleep 300 &\nprintf '%s\\n' \"$!\" > \"$1\"\nwait \"$!\"\nexec sleep 300\n",
	), 0o700))
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name,
		"-e", tmux.EnvMarkerSession+"="+name, script, pidFile).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	var paneChildPID int
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			return false
		}
		_, scanErr := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &paneChildPID)
		return scanErr == nil && paneChildPID > 1
	}, 5*time.Second, 10*time.Millisecond, "pane never published child pid")
	require.Eventually(t, func() bool {
		_, st := proctree.LookupEnv(paneChildPID, tmux.EnvMarkerSession)
		return st == proctree.EnvFound
	}, 10*time.Second, 10*time.Millisecond, "pane child marker never readable")

	realExec := cmd.MakeExecutor()
	opts := testOptions(t, false, paneChildPID)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) > 1 && c.Args[1] == "list-panes" {
				return nil, fmt.Errorf("can't find session: %s", name)
			}
			return realExec.Output(c)
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.True(t, tmuxInspectionPassed(report),
		"the session is verifiably live, so the runaway blindness is per-session, not server-wide")

	// No runaway finding is expected either way (the in-pane child is idle), so
	// the property under test is the blindness row: it must NAME the session
	// rather than render the same as a clean "no runaway processes" scan.
	rows := findCheckRows(report, "runaway-cpu")
	var blind *CheckResult
	for i := range rows {
		if strings.Contains(rows[i].Detail, "could not read the pane tree") &&
			strings.Contains(rows[i].Detail, name) {
			blind = &rows[i]
			break
		}
	}
	require.NotNil(t, blind,
		"a session whose pane tree could not be read must contribute a runaway-cpu blindness "+
			"row rather than be swallowed as silence")
	require.Equal(t, StatusWarn, blind.Status)
	require.False(t, blind.Problem,
		"per-session pane-tree blindness is advisory, not an established unhealthy run")
}

// TestReadablePaneTreeStillReportsGenuineEscape is the other half of the
// contract: the fix must not stop reporting processes that genuinely escaped a
// READABLE pane tree. A marked process outside the live session's pane tree is
// still an escapee — the fix only suppresses escapes when the tree could not be
// read, never when it was read and the process was absent from it.
func TestReadablePaneTreeStillReportsGenuineEscape(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-genuine-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	// A separately spawned process that carries the session's marker but is NOT
	// in its pane tree — a genuine escapee on a readable pane tree.
	escapee := spawnWithEnv(t, "sh", nil, map[string]string{tmux.EnvMarkerSession: name})

	report, err := Run(testOptions(t, false, escapee.PID))
	require.NoError(t, err)
	escapes := findByCheck(report, "escaped-process")
	require.Len(t, escapes, 1,
		"a marked process genuinely outside a READABLE live pane tree must still be reported as escaped")
	require.Contains(t, escapes[0].Detail, "escaped the pane tree")
	require.Empty(t, escapes[0].FixAction, "an escapee of a live session is report-only")
	require.Empty(t, findBlindnessRows(report, name),
		"a readable pane tree must not produce a blindness row for genuine escapes")
}

// TestEscapedProcessForeignHomeNotAttributedToThisInstall pins the foreign-home
// guard in the escaped-process arm. Session names are derived from repo path +
// title and do not encode AF_HOME, so on the shared default tmux server a
// surviving process from another install can carry a name that now matches one
// of OUR live sessions (temporal reuse once that install's session died). Its
// AF_HOME proves it is not ours, so it must be reported as belonging to another
// agent-factory home rather than as an escapee of our live session — the same
// attribution the dead-session (orphaned-process) arm already produces.
//
// This is the same class of false positive that the blindness fix in this file
// removed: a process that is NOT ours reported as ours. The finding stays
// advisory either way; the harm it corrects is factually wrong diagnostic text.
func TestEscapedProcessForeignHomeNotAttributedToThisInstall(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-foreign-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	// A process with our session's AF_SESSION marker but a DIFFERENT home.
	foreignHome := t.TempDir()
	foreignEscapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    foreignHome,
	})

	// Run doctor with OUR home (different from foreignHome), under --fix: the
	// escaped arm is report-only regardless.
	home := testguard.SocketTempDir(t)
	report, err := Run(testOptionsWithHome(t, home, true, foreignEscapee.PID))
	require.NoError(t, err)

	// The foreign-home process is reported under a DISTINCT check key, not the
	// escaped-process key: the escaped-process collapse renders every item in
	// that group as "N processes escaped live session pane trees", which would
	// re-attribute this foreign process to our install in the default
	// (non-verbose) CLI and JSON. Its own key keeps the foreign-home distinction
	// visible without --verbose.
	foreign := findByCheck(report, "foreign-home-process")
	require.Len(t, foreign, 1, "exactly one foreign-home-process finding is expected")
	require.Contains(t, foreign[0].Detail, "belongs to another agent-factory home",
		"foreign-home escapee must be attributed to its home, not ours: %s", foreign[0].Detail)
	require.Contains(t, foreign[0].Detail, foreignHome,
		"the finding must name the foreign home: %s", foreign[0].Detail)
	require.NotContains(t, foreign[0].Detail, "escaped the pane tree",
		"a foreign-home process must not be reported as escaping our live session: %s", foreign[0].Detail)
	require.Empty(t, foreign[0].FixAction, "the escaped arm is report-only — no kill even under --fix")
	require.Empty(t, findByCheck(report, "escaped-process"),
		"a foreign-home process must not be reported as an escapee of our live session")
	require.Empty(t, findByCheck(report, "orphaned-process"),
		"a live-session marker must not route through the orphaned-process arm")
	// No blindness row: the pane tree of our live session is READABLE here, so
	// the foreign-home guard is the only thing keeping this off the escape list.
	require.Empty(t, findBlindnessRows(report, name),
		"a readable pane tree must not produce a blindness row")
	require.True(t, alive(foreignEscapee), "the escaped arm must not kill, even under --fix")

	// The default (non-verbose) JSON output must carry the foreign-home row with
	// its accurate detail, not collapse it into the escaped-process summary that
	// would re-attribute the process to this install. foreign-home-process is a
	// collapsible class, so the default view folds the per-process findings into
	// one bounded "foreign-home-processes" row whose summary preserves the
	// foreign-home distinction; the per-process home is visible under --verbose.
	payload := BuildJSONReport(report, true, false)
	require.Zero(t, payload.Summary.Unresolved,
		"a foreign-home advisory must remain visible without failing a health probe")
	var foreignRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "foreign-home-processes" {
			foreignRow = &payload.Checks[i]
			break
		}
	}
	require.NotNil(t, foreignRow, "the default JSON output must keep the foreign-home row distinct")
	require.Contains(t, foreignRow.Detail, "another agent-factory home",
		"the default JSON row must preserve the foreign-home distinction: %s", foreignRow.Detail)
	require.Contains(t, foreignRow.Detail, "not attributed to this install",
		"the default JSON row must state the process is not ours: %s", foreignRow.Detail)
	var escapedRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "escaped-processes" {
			escapedRow = &payload.Checks[i]
			break
		}
	}
	require.Nil(t, escapedRow, "the default JSON output must not collapse a foreign-home process into an escaped-processes row")

	// Under --verbose the per-process foreign-home finding still names the
	// specific foreign home, so an operator can see which install owns it.
	verbose := BuildJSONReport(report, true, true)
	var verboseForeign *JSONCheck
	for i := range verbose.Checks {
		if verbose.Checks[i].Name == "foreign-home-process" {
			verboseForeign = &verbose.Checks[i]
			break
		}
	}
	require.NotNil(t, verboseForeign, "the verbose JSON output must list the per-process foreign-home finding")
	require.Contains(t, verboseForeign.Detail, "belongs to another agent-factory home",
		"the verbose JSON row must preserve the foreign-home distinction: %s", verboseForeign.Detail)
	require.Contains(t, verboseForeign.Detail, foreignHome,
		"the verbose JSON row must name the foreign home: %s", verboseForeign.Detail)
}

// TestEscapedProcessRelativeMarkerNotAttributedToThisInstall pins the relative
// AGENT_FACTORY_HOME case. session/tmux.afHomeDir stamps a relative
// AGENT_FACTORY_HOME unchanged into AF_HOME, so the marker is relative to the
// launching af process's frame, not the doctor's. Normalizing it in the
// doctor's frame would resolve it against the directory doctor was invoked from
// and could collapse two distinct homes onto the same doctor-relative path,
// letting a genuine foreign escapee compare equal to ours and fall through as
// an escapee of this install. The guard treats a relative marker as unproven —
// reported under the foreign-home key, never attributed to this install.
func TestEscapedProcessRelativeMarkerNotAttributedToThisInstall(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-relative-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	// A process with our session's AF_SESSION marker but a RELATIVE AF_HOME.
	relativeEscapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    ".af-relative-marker",
	})

	// Run doctor with OUR home (absolute), under --fix: the escaped arm is
	// report-only regardless.
	home := testguard.SocketTempDir(t)
	report, err := Run(testOptionsWithHome(t, home, true, relativeEscapee.PID))
	require.NoError(t, err)

	// A relative marker cannot be resolved to a specific install, but it is also
	// not provably ours, so it must not be reported as an escapee of our live
	// session.
	require.Empty(t, findByCheck(report, "escaped-process"),
		"a relative-marker process must not be attributed as an escapee of this install")
	require.Empty(t, findByCheck(report, "foreign-home-process"),
		"a relative-marker process is unproven ownership, not a confirmed foreign home: %s")
	foreign := findByCheck(report, "foreign-home-unresolved")
	require.Len(t, foreign, 1, "exactly one foreign-home-unresolved finding is expected for a relative marker")
	require.Contains(t, foreign[0].Detail, "relative path",
		"the finding must explain the marker is relative: %s", foreign[0].Detail)
	require.Contains(t, foreign[0].Detail, "not attributed to this install",
		"a relative-marker process must not be attributed to this install: %s", foreign[0].Detail)
	require.NotContains(t, foreign[0].Detail, "escaped the pane tree",
		"a relative-marker process must not be reported as escaping our live session: %s", foreign[0].Detail)
	require.Empty(t, foreign[0].FixAction, "the escaped arm is report-only — no kill even under --fix")
	require.Empty(t, findByCheck(report, "orphaned-process"),
		"a live-session marker must not route through the orphaned-process arm")
	require.Empty(t, findBlindnessRows(report, name),
		"a readable pane tree must not produce a blindness row")
	require.True(t, alive(relativeEscapee), "the escaped arm must not kill, even under --fix")

	// The default (non-verbose) JSON output must NOT collapse the unproven
	// finding into the foreign-home-processes row, whose summary ("from
	// another agent-factory home") states a definite foreign-home conclusion
	// the detector withheld for a relative marker. It gets its own
	// foreign-home-unresolved row whose summary says ownership could not be
	// resolved, so the corrected attribution survives collapsing without
	// asserting an unspecified "other" home.
	payload := BuildJSONReport(report, true, false)
	require.Zero(t, payload.Summary.Unresolved,
		"an unproven-ownership advisory must remain visible without failing a health probe")
	var unresolvedRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "foreign-home-unresolved" {
			unresolvedRow = &payload.Checks[i]
			break
		}
	}
	require.NotNil(t, unresolvedRow, "the default JSON output must keep the unproven-ownership row distinct")
	require.Contains(t, unresolvedRow.Detail, "could not be resolved to a specific install",
		"the default JSON row must state ownership is unproven, not assert another home: %s", unresolvedRow.Detail)
	require.NotContains(t, unresolvedRow.Detail, "another agent-factory home",
		"the default JSON row must not claim a confirmed foreign home for an unproven marker: %s", unresolvedRow.Detail)
	var foreignRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "foreign-home-processes" {
			foreignRow = &payload.Checks[i]
			break
		}
	}
	require.Nil(t, foreignRow, "the default JSON output must not collapse a relative-marker finding into the confirmed foreign-home row")
}

// TestEscapedProcessOwnHomeStillReportedAsEscape is the companion regression: the
// foreign-home guard must NOT suppress genuine escapes. A marked process outside
// a readable live pane tree whose AF_HOME matches our install is still an escapee
// of our session, as is a marked process carrying no AF_HOME at all (the original
// TestReadablePaneTreeStillReportsGenuineEscape shape — the guard only reroutes a
// process when AF_HOME is readable AND foreign).
func TestEscapedProcessOwnHomeStillReportedAsEscape(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-own-home-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	home := testguard.SocketTempDir(t)

	// An escapee whose AF_HOME matches our install: still ours, still escaped.
	ownEscapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
	})
	// An escapee carrying the marker but no AF_HOME at all: unreadable home is
	// not foreign home, so it stays on the genuine-escape path (the orphaned arm
	// treats unreachable AF_HOME as report-only, never as foreign).
	unmarkedEscapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
	})

	report, err := Run(testOptionsWithHome(t, home, true, ownEscapee.PID, unmarkedEscapee.PID))
	require.NoError(t, err)

	escapes := findByCheck(report, "escaped-process")
	require.Len(t, escapes, 2, "own-home and unmarked escapees of a readable pane tree must both be reported")
	for _, f := range escapes {
		require.Contains(t, f.Detail, "escaped the pane tree",
			"an own-home/unmarked escapee is still a genuine escape: %s", f.Detail)
		require.NotContains(t, f.Detail, "another agent-factory home",
			"only a proven FOREIGN home is rerouted: %s", f.Detail)
		require.Empty(t, f.FixAction, "the escaped arm is report-only even for genuine escapes")
	}
	require.True(t, alive(ownEscapee), "genuine escapee must survive --fix (report-only)")
	require.True(t, alive(unmarkedEscapee), "unmarked escapee must survive --fix (report-only)")
}

// TestEscapedProcessSameHomeCaseInsensitiveNotForeign pins the os.SameFile arm
// of the foreign-home guard. normalizeHome canonicalizes through
// filepath.EvalSymlinks, but on a case-insensitive macOS volume EvalSymlinks
// preserves the spelling of non-symlink components, so a session stamped with
// ".Agent-Factory" and a doctor run using ".agent-factory" refer to the SAME
// directory yet compare unequal as normalized strings — which would route a
// genuine same-home escapee into the foreign-home report. sameHome compares by
// filesystem identity when both paths stat, so the case difference is seen
// through and the escapee stays classified as ours (escaped-process), not
// foreign. This is only observable on a case-insensitive volume, so the test
// skips itself anywhere the two spellings do not resolve to the same inode.
func TestEscapedProcessSameHomeCaseInsensitiveNotForeign(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-case-home-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	// Our home is the short temp dir doctor runs under; the marker spells it
	// with the basename uppercased. On a case-insensitive volume that is the
	// SAME directory; on a case-sensitive one it is a non-existent path.
	ourHome := testguard.SocketTempDir(t)
	markerHome := filepath.Join(filepath.Dir(ourHome), strings.ToUpper(filepath.Base(ourHome)))

	// Skip unless the two spellings actually resolve to the same inode: this
	// test only means something on a case-insensitive volume, and skipping
	// keeps it inert on Linux/case-sensitive macOS CI.
	ai, aerr := os.Stat(ourHome)
	bi, berr := os.Stat(markerHome)
	if aerr != nil || berr != nil || !os.SameFile(ai, bi) {
		t.Skipf("case-insensitive home identity not observable (GOOS=%s, same-inode=%v)",
			runtime.GOOS, aerr == nil && berr == nil && os.SameFile(ai, bi))
	}

	escapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    markerHome,
		"AGENT_FACTORY_HOME":  ourHome,
	})

	report, err := Run(testOptionsWithHome(t, ourHome, true, escapee.PID))
	require.NoError(t, err)

	// The escapee is genuinely outside the live pane tree, but its home is OURS
	// (same inode, different spelling), so it must stay on the escaped-process
	// report and NOT be re-attributed to another install.
	require.Empty(t, findByCheck(report, "foreign-home-process"),
		"a same-home escapee spelled with different case must not be attributed to another install")
	require.Empty(t, findByCheck(report, "foreign-home-unresolved"),
		"a same-home escapee spelled with different case must not be treated as unproven")
	escapes := findByCheck(report, "escaped-process")
	require.Len(t, escapes, 1,
		"a same-home escapee must still be reported as escaping our live session: %v", escapes)
	require.Contains(t, escapes[0].Detail, "escaped the pane tree",
		"a same-home escapee is still a genuine escape: %s", escapes[0].Detail)
	require.True(t, alive(escapee), "the escaped arm must not kill, even under --fix")
}

// TestEscapedProcessInheritedMarkerForeignOwnHome_NotAttributedToThisInstall is
// the live-session counterpart of TestOrphanInheritedMarkerForeignOwnHome_NotKilled
// (#6578): a second agent-factory install launched from inside one of this home's
// LIVE sessions inherits AF_SESSION/AF_HOME beside its own AGENT_FACTORY_HOME, so
// its process carries a matching marker while belonging to the other install.
// Before the fix, the live-session escaped arm resolved foreignness only from the
// raw inherited-able AF_HOME marker — so a matching marker fell through to
// escaped-process, attributing another install's process to this install in the
// default (non-verbose) CLI and JSON output. The arm must resolve ownership the
// same way the dead-session arm does: from the process's OWN environment, not the
// marker alone.
func TestEscapedProcessInheritedMarkerForeignOwnHome_NotAttributedToThisInstall(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-inherited-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	home := testguard.SocketTempDir(t)
	foreignHome := t.TempDir()

	// A process with our LIVE session's AF_SESSION marker and OUR AF_HOME
	// (inherited from the session), but its own AGENT_FACTORY_HOME names
	// another install — the exact shape a second install launched from
	// inside one of this home's panes would carry.
	foreignEscapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  foreignHome,
	})

	report, err := Run(testOptionsWithHome(t, home, true, foreignEscapee.PID)) // Fix: true
	require.NoError(t, err)

	// The process's own environment names another home, so it must be reported
	// as foreign-home-process — NOT as escaped-process, whose collapse ("N
	// processes escaped live session pane trees") would attribute another
	// install's process to this install in the default output.
	foreign := findByCheck(report, "foreign-home-process")
	require.Len(t, foreign, 1, "exactly one foreign-home-process finding is expected")
	require.Contains(t, foreign[0].Detail, foreignHome,
		"the finding must name the foreign home the process actually belongs to: %s", foreign[0].Detail)
	require.Contains(t, foreign[0].Detail, "inherited",
		"the finding must explain the marker was inherited: %s", foreign[0].Detail)
	require.NotContains(t, foreign[0].Detail, "escaped the pane tree",
		"a foreign-home process must not be reported as escaping our live session: %s", foreign[0].Detail)
	require.Empty(t, foreign[0].FixAction, "the escaped arm is report-only — no kill even under --fix")
	require.False(t, foreign[0].Actionable, "another install's process must not drive the exit code")

	require.Empty(t, findByCheck(report, "escaped-process"),
		"a foreign install's process must not be reported as an escapee of our live session")
	require.Empty(t, findByCheck(report, "orphaned-process"),
		"a live-session marker must not route through the orphaned-process arm")
	require.True(t, alive(foreignEscapee),
		"`af doctor --fix` must not kill another install's process on an inherited marker")

	// The default (non-verbose) JSON output must carry the foreign-home row,
	// not collapse the process into the escaped-processes row that would
	// re-attribute it to this install.
	payload := BuildJSONReport(report, true, false)
	require.Zero(t, payload.Summary.Unresolved,
		"a foreign-home advisory must remain visible without failing a health probe")
	var foreignRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "foreign-home-processes" {
			foreignRow = &payload.Checks[i]
			break
		}
	}
	require.NotNil(t, foreignRow, "the default JSON output must keep the foreign-home row distinct")
	var escapedRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "escaped-processes" {
			escapedRow = &payload.Checks[i]
			break
		}
	}
	require.Nil(t, escapedRow,
		"the default JSON output must not collapse a foreign-home process into an escaped-processes row")
}

// TestEscapedProcessInheritedMarkerForeignAncestor_NotAttributedToThisInstall
// covers the ancestry half: a process whose own AGENT_FACTORY_HOME resolves to
// THIS install but which runs under another install's tmux server is that
// install's business, not an escapee of our live session — even though its
// inherited AF_HOME marker matches ours. The live arm must consult ancestry the
// same way the dead arm does.
func TestEscapedProcessInheritedMarkerForeignAncestor_NotAttributedToThisInstall(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-inherited-ancestor-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	home := testguard.SocketTempDir(t)
	foreignHome := t.TempDir()

	// Two real ancestors — mirroring the dead-arm ancestor test: another
	// install's tmux server (AGENT_FACTORY_HOME declared for the other home)
	// and a bystander that declares nothing. Since #6703 the walk revalidates
	// each recorded ancestor's identity and reads its environ for real, so
	// ancestors must be live processes whose environ answers — a synthetic
	// snapshot row would read as unverifiable rather than foreign.
	// AGENT_FACTORY_HOME="" keeps the bystander from inheriting the sandbox's
	// ambient home declaration.
	foreignServer := spawnWithEnv(t, "sh", nil, map[string]string{
		"AGENT_FACTORY_HOME": foreignHome,
	})
	bystander := spawnWithEnv(t, "sh", nil, map[string]string{
		"AGENT_FACTORY_HOME": "",
		"HOME":               t.TempDir(),
	})

	under := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home, // own env resolves to THIS install
	})
	free := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
	})

	opts := testOptionsWithHome(t, home, true, under.PID, free.PID, foreignServer.PID, bystander.PID) // Fix: true
	stageParent(&opts, under.PID, foreignServer.PID, nil)
	stageParent(&opts, free.PID, bystander.PID, nil)

	report, err := Run(opts)
	require.NoError(t, err)

	// The process under the foreign server is the other install's business.
	foreign := findByCheck(report, "foreign-home-process")
	require.Len(t, foreign, 1,
		"a process under another install's tree must be attributed to it, not escaped here")
	require.Contains(t, foreign[0].Detail, foreignHome,
		"the finding must name the home the ancestor declares: %s", foreign[0].Detail)
	require.Empty(t, foreign[0].FixAction,
		"a process under another install's tree must never be armed: %s", foreign[0].Detail)
	require.True(t, alive(under),
		"`af doctor --fix` must not kill a process under another install's tmux server")

	// The bystander ancestor declares no agent-factory home, so the process
	// beneath it is still proven by its own environment: reported as a genuine
	// escapee of our live session (not foreign, not disarmed).
	escapes := findByCheck(report, "escaped-process")
	require.Len(t, escapes, 1,
		"an own-home process under a plain ancestor must still be reported as escaped")
	require.Contains(t, escapes[0].Detail, "escaped the pane tree",
		"a genuine same-home escapee is still a genuine escape: %s", escapes[0].Detail)
	require.True(t, alive(free), "the escaped arm must not kill, even under --fix")
}

// TestEscapedProcessInheritedMarkerUnprovenHome_NotEscaped pins the unproven
// case: a process whose AF_HOME matches this install but whose own environment
// establishes no agent-factory home (no AGENT_FACTORY_HOME, no HOME to derive
// one from). The marker alone is not proof of this install, so the process must
// not be attributed as an escapee — it is reported under the non-attributing
// foreign-home-unresolved key, mirroring the dead arm's unproven row.
func TestEscapedProcessInheritedMarkerUnprovenHome_NotEscaped(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-unproven-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	home := testguard.SocketTempDir(t)

	// A process with our LIVE session's AF_SESSION marker and OUR AF_HOME
	// (inherited), but no AGENT_FACTORY_HOME and no HOME of its own — so
	// daemonProcessHome cannot establish any home from its own environment.
	unprovenEscapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
	})
	// Stub the process's environ for daemonProcessEnvLookup so its own
	// AGENT_FACTORY_HOME and HOME read as absent — the shape that makes
	// markedProcessOwnership return ownershipUnproven. The AF_HOME marker
	// is read through proctree.LookupEnv (the real /proc environ), which
	// still sees the inherited marker.
	stubProcessEnv(t, unprovenEscapee.PID, map[string]string{
		"AGENT_FACTORY_HOME": "",
		"HOME":               "",
	})

	report, err := Run(testOptionsWithHome(t, home, true, unprovenEscapee.PID)) // Fix: true
	require.NoError(t, err)

	// Ownership is unproven: the marker matches our home, but the process's
	// own environment establishes no home. It must not be attributed as an
	// escapee of our live session.
	require.Empty(t, findByCheck(report, "escaped-process"),
		"an unproven-ownership process must not be attributed as an escapee of this install")
	require.Empty(t, findByCheck(report, "foreign-home-process"),
		"an unproven-ownership process is not a confirmed foreign home")
	unproven := findByCheck(report, "foreign-home-unresolved")
	require.Len(t, unproven, 1, "exactly one foreign-home-unresolved finding is expected for unproven ownership")
	require.Contains(t, unproven[0].Detail, "unproven",
		"the finding must state ownership is unproven: %s", unproven[0].Detail)
	require.Contains(t, unproven[0].Detail, "not attributed to this install",
		"an unproven-ownership process must not be attributed to this install: %s", unproven[0].Detail)
	require.NotContains(t, unproven[0].Detail, "escaped the pane tree",
		"an unproven-ownership process must not be reported as escaping our live session: %s", unproven[0].Detail)
	require.Empty(t, unproven[0].FixAction, "the escaped arm is report-only — no kill even under --fix")
	require.True(t, alive(unprovenEscapee), "the escaped arm must not kill, even under --fix")
}

// TestEscapedProcessInheritedMarkerMatchingHomes_StillReportedAsEscape is the
// control for the live-arm ownership guard: a process whose AF_HOME matches ours
// AND whose own AGENT_FACTORY_HOME also resolves to this install is a genuine
// escapee of our live session — the guard must not suppress it. This is the
// live-arm mirror of TestOrphanInheritedMarkerMatchingHomes_StillArmed.
func TestEscapedProcessInheritedMarkerMatchingHomes_StillReportedAsEscape(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-matching-homes-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	home := testguard.SocketTempDir(t)

	// A genuine same-home escapee: AF_HOME matches ours, and its own
	// AGENT_FACTORY_HOME also resolves to this install.
	ownEscapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
	})

	report, err := Run(testOptionsWithHome(t, home, true, ownEscapee.PID)) // Fix: true
	require.NoError(t, err)

	escapes := findByCheck(report, "escaped-process")
	require.Len(t, escapes, 1, "a genuine same-home escapee must still be reported as escaped")
	require.Contains(t, escapes[0].Detail, "escaped the pane tree",
		"a same-home escapee is still a genuine escape: %s", escapes[0].Detail)
	require.Empty(t, findByCheck(report, "foreign-home-process"),
		"a same-home escapee must not be attributed to another install")
	require.Empty(t, findByCheck(report, "foreign-home-unresolved"),
		"a same-home escapee must not be treated as unproven ownership")
	require.Empty(t, escapes[0].FixAction, "the escaped arm is report-only even for genuine escapes")
	require.True(t, alive(ownEscapee), "the escaped arm must not kill, even under --fix")
}

// findBlindnessRows returns the process-leak-inspection rows that announce a
// session's pane tree could not be read. Check rows (Warn), not findings.
func findBlindnessRows(r *Report, session string) []CheckResult {
	var out []CheckResult
	for _, c := range findCheckRows(r, "process-leak-inspection") {
		if strings.Contains(c.Detail, "could not read the pane tree") &&
			strings.Contains(c.Detail, session) {
			out = append(out, c)
		}
	}
	return out
}

// tmuxInspectionPassed reports whether checkTmuxInspection recorded a Pass —
// i.e. `tmux ls` answered authoritatively, so any per-session failure below is
// NOT a server-wide blindness.
func tmuxInspectionPassed(r *Report) bool {
	for _, c := range findCheckRows(r, "tmux-inspection") {
		if c.Status == StatusPass {
			return true
		}
	}
	return false
}
