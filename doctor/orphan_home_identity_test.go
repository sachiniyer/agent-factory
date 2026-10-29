package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin the dead-session kill arm's ownership proof in
// checkOrphanedProcesses (and the parallel leaked-session ownership attribution
// in checkLeakedTmuxSessions) to the same standard the live-session escaped arm
// already enforces: a kill requires a PROVEN home match, decided by filesystem
// identity (os.SameFile via sameHome), with a relative AGENT_FACTORY_HOME marker
// treated as unproven ownership.
//
// Before the fix, the kill arm decided ownership with a purely lexical
// filepath.Clean(home) == filepath.Clean(ctx.opts.ConfigDir) comparison. That
// mis-decided in both directions: a same-inode home spelled differently (a
// symlinked spelling, macOS /var vs /private/var, or a case-differing spelling
// on a case-insensitive volume) was reported as "belongs to another agent-factory
// home" and left unfixed (under-kill), and a relative marker that lexically
// equalled doctor's relative ConfigDir armed killFix for a process that might
// belong to a different install launched from a different working directory
// with the same relative AGENT_FACTORY_HOME (over-kill). The escaped arm had
// been hardened against exactly these hazards; the kill arm had not.

// TestOrphanedProcessKill_SameHomeSymlinkSpelling_Armed pins the under-kill
// fix: a verified orphan whose real AF_HOME is the SAME directory as doctor's
// home but spelled through a symlink (or, on macOS, /var vs /private/var) must
// still be armed for --fix. The lexical comparison the kill arm used reported
// such an orphan as "belongs to another agent-factory home" and refused to fix
// it, attributing this install's own verified leak to a nonexistent other
// install.
//
// aliasedSocketTempHome returns (alias, canonical): alias is a symlink spelling
// of the same directory canonical points at, so the two strings compare unequal
// (filepath.Clean leaves the symlinked spelling intact) while os.SameFile sees
// through it. Doctor runs against the alias; the orphan's marker carries the
// canonical spelling — exactly the shape that defeated the lexical comparison.
func TestOrphanedProcessKill_SameHomeSymlinkSpelling_Armed(t *testing.T) {
	testguard.IsolateTmux(t)

	alias, canonical := aliasedSocketTempHome(t)
	require.NotEqual(t, alias, canonical,
		"test premise: the two spellings of the same home must differ lexically")
	ai, aerr := os.Stat(canonical)
	bi, berr := os.Stat(alias)
	require.NoError(t, aerr)
	require.NoError(t, berr)
	require.True(t, os.SameFile(ai, bi),
		"test premise: the two spellings must resolve to the same inode")

	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-dead-symlink-home",
		tmux.EnvMarkerHome:    canonical,
	})

	// Read-only pass: armed, not touched.
	report, err := Run(testOptionsWithHome(t, alias, false, orphan.PID))
	require.NoError(t, err)
	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1, "the verified orphan must be reported under orphaned-process")
	require.NotEmpty(t, findings[0].FixAction,
		"a same-inode home spelled differently must still arm the kill — the under-kill fix")
	require.Contains(t, findings[0].Detail, "af_doctor-dead-symlink-home")
	require.NotContains(t, findings[0].Detail, "another agent-factory home",
		"a same-inode home is this install, not another")
	require.Empty(t, findByCheck(report, "foreign-home-unresolved"),
		"a same-inode absolute home is not an unproven marker")
	require.True(t, alive(orphan), "read-only doctor run must not signal anything")

	// --fix pass: killed, with the outcome recorded.
	report, err = Run(testOptionsWithHome(t, alias, true, orphan.PID))
	require.NoError(t, err)
	findings = findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"a same-inode-home orphan must be killed by --fix (the under-kill fix)")
	require.Zero(t, report.UnresolvedCount())
}

// TestOrphanedProcessKill_SameHomeCaseInsensitiveNotForeign_Armed pins the
// os.SameFile arm of the under-kill fix for the dead-session kill path. On a
// case-insensitive macOS volume, EvalSymlinks preserves the spelling of
// non-symlink components, so a session stamped with ".Agent-Factory" and a
// doctor run using ".agent-factory" refer to the SAME directory yet compare
// unequal as normalized strings — which armed nothing before the fix (the
// orphan landed in the "belongs to another agent-factory home" advisory).
// sameHome compares by filesystem identity when both paths stat, so the case
// difference is seen through and the orphan is armed. Only observable on a
// case-insensitive volume, so the test skips itself where the two spellings do
// not resolve to the same inode.
func TestOrphanedProcessKill_SameHomeCaseInsensitiveNotForeign_Armed(t *testing.T) {
	testguard.IsolateTmux(t)

	ourHome := testguard.SocketTempDir(t)
	markerHome := filepath.Join(filepath.Dir(ourHome), strings.ToUpper(filepath.Base(ourHome)))

	ai, aerr := os.Stat(ourHome)
	bi, berr := os.Stat(markerHome)
	if aerr != nil || berr != nil || !os.SameFile(ai, bi) {
		t.Skipf("case-insensitive home identity not observable (GOOS=%s, same-inode=%v)",
			runtime.GOOS, aerr == nil && berr == nil && os.SameFile(ai, bi))
	}

	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-dead-case-home",
		tmux.EnvMarkerHome:    markerHome,
	})

	report, err := Run(testOptionsWithHome(t, ourHome, true, orphan.PID))
	require.NoError(t, err)

	// The orphan is a verified orphan (dead session) whose home is OURS by
	// inode, so it must be armed — not routed to a foreign-home advisory.
	require.Empty(t, findByCheck(report, "foreign-home-unresolved"),
		"a same-inode (case-differing) home is not an unproven marker")
	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixAction,
		"a case-differing spelling of the same home must still arm the kill")
	require.NotContains(t, findings[0].Detail, "another agent-factory home",
		"a same-inode home is this install, not another: %s", findings[0].Detail)
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"a case-differing same-home orphan must be killed by --fix")
	require.Zero(t, report.UnresolvedCount())
}

// TestOrphanedProcessKill_RelativeMarkerNeverArmed pins the over-kill fix. A
// relative AGENT_FACTORY_HOME marker is relative to its launching af process's
// frame, which doctor cannot recover; a foreign install launched from a
// different working directory with the same relative AGENT_FACTORY_HOME stamps
// the same string this install does. The kill arm's lexical comparison armed
// killFix whenever the relative marker string equalled doctor's relative
// ConfigDir, so a foreign install's verified orphan (looked dead here because
// its session lives on a private tmux server) could be killed from here. The
// fix treats a relative marker as unproven ownership: it is reported under the
// foreign-home-unresolved key (mirroring the escaped arm) and never armed, even
// when the string happens to match doctor's ConfigDir exactly.
//
// Staged with ConfigDir == the orphan's marker string (both relative), which is
// the exact shape that armed the kill before the fix.
func TestOrphanedProcessKill_RelativeMarkerNeverArmed(t *testing.T) {
	testguard.IsolateTmux(t)

	// A working directory this test owns, with the relative home existing
	// inside it so doctor and the spawned orphan resolve the same string to a
	// real directory — the production shape of a relative AGENT_FACTORY_HOME
	// that happens to match doctor's.
	workdir := t.TempDir()
	const rel = ".af-doctor-rel-marker"
	require.NoError(t, os.MkdirAll(filepath.Join(workdir, rel), 0o755))
	chdir(t, workdir)

	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-rel-orphan",
		tmux.EnvMarkerHome:    rel,
	})

	// ConfigDir is the SAME relative string as the orphan's marker — the case
	// that armed the kill before the fix.
	report, err := Run(testOptionsWithHome(t, rel, true, orphan.PID))
	require.NoError(t, err)

	// The relative marker is unproven ownership, reported under a distinct key
	// (mirroring the escaped arm) — never armed, never asserted as another home.
	require.Empty(t, findByCheck(report, "orphaned-process"),
		"a relative marker must never arm or count as a proven orphan")
	foreign := findByCheck(report, "foreign-home-unresolved")
	require.Len(t, foreign, 1, "a relative-marker orphan must be reported as unproven ownership")
	require.Empty(t, foreign[0].FixAction,
		"an unproven-ownership orphan must be report-only — no kill under --fix")
	require.Contains(t, foreign[0].Detail, "relative path",
		"the finding must explain the marker is relative: %s", foreign[0].Detail)
	require.Contains(t, foreign[0].Detail, rel,
		"the finding must name the relative marker: %s", foreign[0].Detail)
	require.NotContains(t, foreign[0].Detail, "another agent-factory home",
		"a relative marker is unproven, not a confirmed foreign home: %s", foreign[0].Detail)
	require.False(t, foreign[0].Actionable,
		"an unproven-ownership orphan must not drive the exit code")
	require.Zero(t, report.UnresolvedCount(),
		"an unproven-ownership orphan must not fail a health probe")
	require.True(t, alive(orphan), "a relative-marker orphan must survive --fix (the over-kill fix)")
}

// TestLeakedSession_SameHomeSymlinkSpelling_Actionable pins the parallel fix in
// checkLeakedTmuxSessions. A leaked tmux session whose AF_HOME is the same
// directory as doctor's home but spelled through a symlink must be attributed
// to THIS install (actionable, with the manual kill command), not reported as
// "created by another agent-factory home". The lexical comparison fed the
// manual `tmux kill-session` recommendation only for byte-identical spellings,
// mis-attributing a symlinked spelling of this install's own home to a
// nonexistent other install.
func TestLeakedSession_SameHomeSymlinkSpelling_Actionable(t *testing.T) {
	testguard.IsolateTmux(t)

	alias, canonical := aliasedSocketTempHome(t)
	const name = "af_doctor-leaked-symlink"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name,
		"-e", tmux.EnvMarkerHome+"="+canonical,
		"env", "-u", tmux.EnvMarkerHome, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	report, err := Run(testOptionsWithHome(t, alias, true))
	require.NoError(t, err)
	leaked := findByCheck(report, "leaked-tmux-session")
	require.Len(t, leaked, 1, "the symlinked-home leaked session must be reported")
	require.True(t, leaked[0].Actionable,
		"a same-inode (symlinked) home is this install — the leak must be actionable, not advisory")
	require.Contains(t, leaked[0].Detail, "created by this install",
		"the origin must attribute the session to this install: %s", leaked[0].Detail)
	require.NotContains(t, leaked[0].Detail, "another agent-factory home",
		"a same-inode home is not another install: %s", leaked[0].Detail)
	require.NotContains(t, leaked[0].Detail, "relative",
		"an absolute symlinked home is not an unproven relative marker: %s", leaked[0].Detail)

	// The leaked-session check is report-only under --fix (it prints a manual
	// kill command, never auto-killing), so the session must survive --fix and
	// the proven leak must drive a nonzero exit.
	require.NoError(t, exec.Command("tmux", "has-session", "-t", "="+name+":").Run(),
		"a leaked tmux session must survive `af doctor --fix`")
	require.Equal(t, 1, report.UnresolvedCount(),
		"a proven this-install leak must be the one actionable health-probe failure")
}

// TestLeakedSession_RelativeMarker_AdvisoryUnproven pins the parallel fix for a
// relative AF_HOME marker on a leaked session. The lexical comparison asserted
// "created by another agent-factory home" for any non-matching string; a
// relative marker is unproven ownership (its originating frame is not
// recoverable), so the fix reports it as such and keeps the row advisory rather
// than asserting a definitive foreign home or attributing the session to this
// install.
func TestLeakedSession_RelativeMarker_AdvisoryUnproven(t *testing.T) {
	testguard.IsolateTmux(t)

	home := testguard.SocketTempDir(t)
	const name = "af_doctor-leaked-relative"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name,
		"-e", tmux.EnvMarkerHome+"="+".af-leaked-relative",
		"env", "-u", tmux.EnvMarkerHome, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	report, err := Run(testOptionsWithHome(t, home, true))
	require.NoError(t, err)
	leaked := findByCheck(report, "leaked-tmux-session")
	require.Len(t, leaked, 1)
	require.False(t, leaked[0].Actionable,
		"a relative-marker session is unproven ownership — it must stay advisory, not actionable")
	require.Contains(t, leaked[0].Detail, "relative AGENT_FACTORY_HOME",
		"the finding must explain the marker is relative: %s", leaked[0].Detail)
	require.Contains(t, leaked[0].Detail, ".af-leaked-relative",
		"the finding must name the relative marker: %s", leaked[0].Detail)
	require.NotContains(t, leaked[0].Detail, "another agent-factory home",
		"a relative marker is unproven, not a confirmed foreign home: %s", leaked[0].Detail)
	require.NotContains(t, leaked[0].Detail, "created by this install",
		"a relative marker must not be attributed to this install: %s", leaked[0].Detail)
	require.NoError(t, exec.Command("tmux", "has-session", "-t", "="+name+":").Run(),
		"a leaked tmux session must survive `af doctor --fix`")
	require.Zero(t, report.UnresolvedCount(),
		"an unproven-ownership session must not fail a health probe")
}
