package doctor

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin #6704: one classifier produces the ownership verdict for
// both process arms, and every report row claims only what it proved. A marked
// process the classifier cannot attribute lands on the non-attributing
// foreign-home-unresolved row — its detail names the real reason and its
// remedy is true for that reason — instead of being counted as an orphan of
// this install, an escapee of this install, or a confirmed member of some
// other install.

// TestOrphanNoHomeMarker_NotAttributed is the dead-session half of item 3: a
// process carrying a dead session's AF_SESSION marker but NO AF_HOME marker is
// attributed to nothing — the marker alone proved the session was named like
// this home's, never which install owns the process. It belongs on the
// non-attributing unresolved row, not counted among this home's orphans.
func TestOrphanNoHomeMarker_NotAttributed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	const name = "af_doctor-unmarked-orphan"
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		// No AF_HOME — nothing at the marker layer names an install.
	})
	control := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID) // Fix: true
	stageParent(&opts, control.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)

	vf := findingForPID(findByCheck(report, "foreign-home-unresolved"), victim.PID)
	require.NotNil(t, vf,
		"a marked process with no readable AF_HOME must land on the non-attributing row")
	require.Contains(t, vf.Detail, "no readable AF_HOME marker",
		"the finding must say what could not be read: %s", vf.Detail)
	require.Contains(t, vf.Detail, "unproven",
		"the finding must state ownership is unproven: %s", vf.Detail)
	require.Empty(t, vf.FixAction,
		"an unproven process must never be armed: %s", vf.Detail)
	require.Equal(t,
		"inspect the process manually — no readable AF_HOME marker names an install",
		vf.Remediation)
	require.Nil(t, findingForPID(findByCheck(report, "orphaned-process"), victim.PID),
		"a no-marker process must not be counted among this install's orphans")
	require.Nil(t, findingForPID(findByCheck(report, "escaped-process"), victim.PID),
		"a no-marker process must not be attributed as an escapee of this install")
	require.True(t, alive(victim),
		"`af doctor --fix` killed a process with no proven owning install (#6704)")

	cf := findingForPID(findByCheck(report, "orphaned-process"), control.PID)
	require.NotNil(t, cf, "the marked same-home control must still be reported")
	require.NotEmpty(t, cf.FixAction)
	require.True(t, cf.Fixed, "fix outcome: %v", cf.FixErr)
	require.Eventually(t, func() bool { return !alive(control) }, 5*time.Second, 25*time.Millisecond,
		"a genuinely-ours orphan must still be killed by --fix")
}

// TestEscapedNoHomeMarker_NotAttributed is the live-session half of item 3 —
// the pinned case the issue calls out: a process carrying a live session's
// AF_SESSION marker but no AF_HOME went straight to escaped-process, which
// attributes it to this install. A missing marker proves nothing, so it must
// land on the non-attributing row instead.
func TestEscapedNoHomeMarker_NotAttributed(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-unmarked-escape"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	home := testguard.SocketTempDir(t)

	escapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		// No AF_HOME marker. Its ambient environment inherits the sandbox's
		// AGENT_FACTORY_HOME — foreign to this run's home — the exact shape the
		// issue's pinned fixture carried.
	})

	report, err := Run(testOptionsWithHome(t, home, true, escapee.PID)) // Fix: true
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, "escaped-process"),
		"a process with no readable AF_HOME must not be attributed as an escapee of this install")
	unresolved := findByCheck(report, "foreign-home-unresolved")
	require.Len(t, unresolved, 1,
		"a marked process with no readable AF_HOME must land on the non-attributing row")
	require.Contains(t, unresolved[0].Detail, "no readable AF_HOME marker",
		"the finding must say what could not be read: %s", unresolved[0].Detail)
	require.Contains(t, unresolved[0].Detail, "unproven",
		"the finding must state ownership is unproven: %s", unresolved[0].Detail)
	require.NotContains(t, unresolved[0].Detail, "escaped the pane tree",
		"a no-marker process must not be reported as escaping our live session: %s", unresolved[0].Detail)
	require.Empty(t, unresolved[0].FixAction, "the escaped arm is report-only — no kill even under --fix")
	require.Equal(t,
		"inspect the process manually — no readable AF_HOME marker names an install",
		unresolved[0].Remediation)
	require.True(t, alive(escapee), "the escaped arm must not kill, even under --fix")
}

// TestEscapedDerivedHomeMissing_Unproven pins item 4: the HOME-derived default
// must not invent an install. A marked process whose own AGENT_FACTORY_HOME is
// unset gets its home derived as <its HOME>/.agent-factory — when that
// directory does not exist the derivation is no evidence at all: the process
// is unproven, never a confirmed member of the nonexistent "other install" and
// never counted as ours.
func TestEscapedDerivedHomeMissing_Unproven(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_doctor-derived-missing"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name+":").Run() })

	home := testguard.SocketTempDir(t)
	// bare exists but holds no .agent-factory — so the derived home
	// <bare>/.agent-factory names no install anywhere.
	bare := t.TempDir()
	derived := filepath.Join(bare, ".agent-factory")

	escapee := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: name,
		tmux.EnvMarkerHome:    home, // marker matches this install
		"AGENT_FACTORY_HOME":  "",   // unset: the HOME-derived default applies
		"HOME":                bare, // ...which points at a home that does not exist
	})

	report, err := Run(testOptionsWithHome(t, home, true, escapee.PID)) // Fix: true
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, "escaped-process"),
		"an unproven process must not be attributed as an escapee of this install")
	require.Empty(t, findByCheck(report, "foreign-home-process"),
		"a derived home that does not exist must not be reported as a confirmed foreign install")
	unresolved := findByCheck(report, "foreign-home-unresolved")
	require.Len(t, unresolved, 1,
		"a derived home that does not exist must be reported as unproven")
	require.Contains(t, unresolved[0].Detail, derived,
		"the finding must name the derived home that does not exist: %s", unresolved[0].Detail)
	require.Contains(t, unresolved[0].Detail, "does not exist",
		"the finding must say the derived home does not exist: %s", unresolved[0].Detail)
	require.Contains(t, unresolved[0].Detail, "unproven",
		"the finding must state ownership is unproven: %s", unresolved[0].Detail)
	require.Empty(t, unresolved[0].FixAction, "the escaped arm is report-only")
	require.Equal(t,
		"inspect the process manually — the home its environment would derive does not exist",
		unresolved[0].Remediation)
	require.True(t, alive(escapee), "the escaped arm must not kill, even under --fix")

	// The collapsed row must only claim what every routed reason proves: the
	// home could not be proven, and the process is not attributed — never that
	// a specific install was identified.
	payload := BuildJSONReport(report, true, false)
	var unresolvedRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "foreign-home-unresolved" {
			unresolvedRow = &payload.Checks[i]
			break
		}
	}
	require.NotNil(t, unresolvedRow,
		"the default JSON output must keep the unproven-ownership row distinct")
	require.Contains(t, unresolvedRow.Detail, "could not be proven",
		"the collapsed row must state the withheld conclusion: %s", unresolvedRow.Detail)
	require.Contains(t, unresolvedRow.Detail, "not attributed to this install",
		"the collapsed row must not attribute unproven processes: %s", unresolvedRow.Detail)
	require.NotContains(t, unresolvedRow.Detail, "another agent-factory home",
		"the collapsed row must not assert a confirmed foreign home: %s", unresolvedRow.Detail)
	require.Contains(t, unresolvedRow.Remedy, "--verbose",
		"the collapsed remedy must point at the per-process details: %s", unresolvedRow.Remedy)
}

// --- #6716: the arming verdict is never a zero value or a fallthrough ---

// TestZeroValueVerdictsAreNeverProven pins the structural guarantee the kill
// arm relies on: 0 means unknown in both enums, so a result nobody produced
// cannot read as proven-ours.
func TestZeroValueVerdictsAreNeverProven(t *testing.T) {
	var cls processOwnership
	require.NotEqual(t, ownershipThisInstall, cls.verdict,
		"the zero ownership verdict must never read as proven-ours (#6716)")
	f := nonOwningFinding(proctree.Process{PID: 4242}, "af_zero", cls, false)
	require.Equal(t, "foreign-home-unresolved", f.Check,
		"an unproduced verdict must land on the non-attributing row")
	require.Empty(t, f.FixAction, "an unproduced verdict must never arm")

	var res ancestryResult
	require.NotEqual(t, ancestryClean, res.verdict,
		"the zero ancestry verdict must never read as clean (#6716)")

	var dv sessionDeathVerdict
	require.Equal(t, deathUnproven, dv,
		"the zero session-death verdict must be unproven — never the arming value (#6716)")
	require.NotEqual(t, deathProven, dv,
		"the zero session-death verdict must never read as proven (#6716)")
}

// TestZeroValueDeathVerdict_NeverArmed drives a session-death verdict nobody
// produced (the zero value) through the kill arm's switch via the deathProof
// seam: the arm must treat it like every other non-deathProven verdict —
// report-only, never a kill.
func TestZeroValueDeathVerdict_NeverArmed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-zero-death",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	})
	control := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-zero-death",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	})

	prev := deathProof
	deathProof = func(ctx *scanContext, p proctree.Process, name string) (sessionDeathVerdict, string) {
		if p.PID == victim.PID {
			return sessionDeathVerdict(0), "" // a verdict nobody produced
		}
		return prev(ctx, p, name)
	}
	t.Cleanup(func() { deathProof = prev })

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID) // Fix: true
	stageParent(&opts, victim.PID, 1, nil)
	stageParent(&opts, control.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)

	vf := findingForPID(findByCheck(report, "orphaned-process"), victim.PID)
	require.NotNil(t, vf,
		"a death verdict nobody produced must land on the report-only row, not arm the kill")
	require.Contains(t, vf.Detail, "cannot corroborate",
		"the finding must say death could not be corroborated: %s", vf.Detail)
	require.Empty(t, vf.FixAction,
		"a zero-valued death verdict must never arm a kill (#6716)")
	require.True(t, alive(victim), "a zero-valued death verdict armed a kill (#6716)")

	cf := findingForPID(findByCheck(report, "orphaned-process"), control.PID)
	require.NotNil(t, cf, "the verified control must still be reported")
	require.NotEmpty(t, cf.FixAction)
	require.True(t, cf.Fixed, "fix outcome: %v", cf.FixErr)
	require.Eventually(t, func() bool { return !alive(control) }, 5*time.Second, 25*time.Millisecond,
		"a genuinely-ours orphan must still be killed by --fix")
}

// TestClassifierRefusesUnknownAncestryVerdict hands the classifier an
// ancestry result with a verdict nobody produced (the zero value). The
// ancestry switch must refuse it — anything other than clean is not a fully
// verified chain, so the victim must be unproven, never armed.
func TestClassifierRefusesUnknownAncestryVerdict(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	victim, control := orphanAncestorFixture(t, home, "af_doctor-unknown-ancestry")

	prev := ancestorWalk
	ancestorWalk = func(ctx *scanContext, p proctree.Process) ancestryResult {
		if p.PID == victim.PID {
			return ancestryResult{} // a verdict nobody produced
		}
		return prev(ctx, p)
	}
	t.Cleanup(func() { ancestorWalk = prev })

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID) // Fix: true
	stageParent(&opts, control.PID, 1, nil)
	stageParent(&opts, victim.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)

	vf := findingForPID(findByCheck(report, "foreign-home-unresolved"), victim.PID)
	require.NotNil(t, vf,
		"a verdict nobody produced must land on the non-attributing row, not arm the kill")
	require.Contains(t, vf.Detail, "unproven",
		"the finding must state ownership is unproven: %s", vf.Detail)
	require.Empty(t, vf.FixAction,
		"an unproduced ancestry verdict must never arm a kill: %s", vf.Detail)
	require.True(t, alive(victim),
		"a zero-valued ancestry verdict armed a kill (#6716)")

	cf := findingForPID(findByCheck(report, "orphaned-process"), control.PID)
	require.NotNil(t, cf, "the verified control must still be reported")
	require.NotEmpty(t, cf.FixAction)
	require.True(t, cf.Fixed, "fix outcome: %v", cf.FixErr)
	require.Eventually(t, func() bool { return !alive(control) }, 5*time.Second, 25*time.Millisecond,
		"a genuinely-ours orphan must still be killed by --fix")
}

// TestOrphanRelativeOwnHome_NeverArmed pins #6716 item 2: a process's own
// RELATIVE AGENT_FACTORY_HOME is resolved against its working directory —
// the directory it happens to be in now, not the frame it was set in, which
// cannot be recovered. Staged so the resolution lands exactly on this
// install's home: on the kill path even that perfect-looking resolution must
// prove nothing.
func TestOrphanRelativeOwnHome_NeverArmed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// The spawned victim inherits the test's working directory: chdir into
	// home's parent, then declare the victim's own home as the relative
	// basename — daemonProcessHome resolves it (against that cwd) back to
	// this install's home.
	chdir(t, filepath.Dir(home))
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-rel-own-home",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  filepath.Base(home),
	})
	control := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-rel-own-home",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID) // Fix: true
	stageParent(&opts, victim.PID, 1, nil)
	stageParent(&opts, control.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)

	vf := findingForPID(findByCheck(report, "foreign-home-unresolved"), victim.PID)
	require.NotNil(t, vf,
		"a relative own home must land on the non-attributing row")
	require.Contains(t, vf.Detail, filepath.Base(home),
		"the finding must name the relative own-home spelling: %s", vf.Detail)
	require.Contains(t, vf.Detail, "relative",
		"the finding must say the own home is relative: %s", vf.Detail)
	require.Contains(t, vf.Detail, "unproven",
		"the finding must state ownership is unproven: %s", vf.Detail)
	require.Empty(t, vf.FixAction,
		"a relative own home must never arm a kill: %s", vf.Detail)
	require.Equal(t,
		"inspect the process's environment manually to identify its agent-factory home",
		vf.Remediation)
	require.Nil(t, findingForPID(findByCheck(report, "orphaned-process"), victim.PID),
		"a relative-own-home process must not be counted among this install's orphans")
	require.True(t, alive(victim),
		"`af doctor --fix` killed a process on a relative own-home claim (#6716)")

	cf := findingForPID(findByCheck(report, "orphaned-process"), control.PID)
	require.NotNil(t, cf, "the absolute-own-home control must still be reported")
	require.NotEmpty(t, cf.FixAction)
	require.True(t, cf.Fixed, "fix outcome: %v", cf.FixErr)
	require.Eventually(t, func() bool { return !alive(control) }, 5*time.Second, 25*time.Millisecond,
		"a genuinely-ours orphan must still be killed by --fix")
}

// TestOrphanOwnEnvUnreadable_Unproven covers the last unproven verdict with
// an exact-remedy assertion: the marker matches but the process's own
// environment cannot be read at all.
func TestOrphanOwnEnvUnreadable_Unproven(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-env-unreadable",
		tmux.EnvMarkerHome:    home,
	})
	stubProcessEnvError(t, victim.PID, errors.New("read /proc/self/environ: permission denied"))
	control := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-env-unreadable",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID) // Fix: true
	stageParent(&opts, victim.PID, 1, nil)
	stageParent(&opts, control.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)

	vf := findingForPID(findByCheck(report, "foreign-home-unresolved"), victim.PID)
	require.NotNil(t, vf, "an unreadable own environment must land on the non-attributing row")
	require.Contains(t, vf.Detail, "environment could not be read",
		"the finding must name the true cause: %s", vf.Detail)
	require.Contains(t, vf.Detail, "unproven")
	require.Empty(t, vf.FixAction)
	require.Equal(t,
		"inspect the process manually — its environment could not be read",
		vf.Remediation)
	require.True(t, alive(victim), "an unreadable own environment must never arm a kill")

	cf := findingForPID(findByCheck(report, "orphaned-process"), control.PID)
	require.NotNil(t, cf)
	require.NotEmpty(t, cf.FixAction)
	require.True(t, cf.Fixed, "fix outcome: %v", cf.FixErr)
	require.Eventually(t, func() bool { return !alive(control) }, 5*time.Second, 25*time.Millisecond,
		"a genuinely-ours orphan must still be killed by --fix")
}

// TestOrphanedProcessUnconfirmedDeath_HonestText pins #6716 item 3: the
// orphaned-process row whose death could NOT be corroborated must not carry
// the foreign-home remedy ("run af doctor with the owning AGENT_FACTORY_HOME
// active" — the owning home IS this one), must name the server that actually
// hosted the session when the evidence names one, and the collapsed row must
// not call it an orphan "from dead sessions" when death was never confirmed.
func TestOrphanedProcessUnconfirmedDeath_HonestText(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// Matching markers but no TMUX env and no server record: the death proof
	// cannot corroborate and no evidence names a server, so the remedy must
	// say where to look rather than invent one.
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-unconfirmed-death",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
	})
	// Matching markers with a TMUX env naming a socket that cannot be listed:
	// the env's socket is the server the session ran on, so the remedy must
	// name THAT socket — not a daemon record that does not exist here.
	fakeSock := testguard.SocketPath(t, "never-created")
	socked := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-unconfirmed-death-sock",
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                fmt.Sprintf("%s,0,0", fakeSock),
	})

	report, err := Run(testOptionsWithHome(t, home, true, victim.PID, socked.PID))
	require.NoError(t, err)

	orphans := findByCheck(report, "orphaned-process")
	require.Len(t, orphans, 2)
	vf := findingForPID(orphans, victim.PID)
	require.NotNil(t, vf)
	require.Empty(t, vf.FixAction, "unconfirmed death is report-only")
	require.Equal(t,
		"find the tmux server the session ran on — from the process's TMUX env or "+
			"the daemon's server record — and verify the session is dead there "+
			"before killing the process manually",
		vf.Remediation,
		"with no TMUX env and no record the remedy must not invent a server (#6716)")

	sf := findingForPID(orphans, socked.PID)
	require.NotNil(t, sf)
	require.Empty(t, sf.FixAction, "unconfirmed death is report-only")
	require.Equal(t,
		fmt.Sprintf("verify the session is dead on tmux socket %s — "+
			"the server that hosted it — before killing the process manually", fakeSock),
		sf.Remediation,
		"when the process's own TMUX env names the hosting server the remedy must name it (#6716)")

	payload := BuildJSONReport(report, true, false)
	var orphanRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == "orphaned-processes" {
			orphanRow = &payload.Checks[i]
			break
		}
	}
	require.NotNil(t, orphanRow)
	require.NotContains(t, orphanRow.Detail, "from dead sessions",
		"the collapsed row must not claim death for rows whose death was not confirmed: %s",
		orphanRow.Detail)
	require.Contains(t, orphanRow.Detail, "reported, not killed",
		"the collapsed row must say what was actually done: %s", orphanRow.Detail)
}

// TestOrphanPaneTreeUnreadable_ReportOnly covers the last report-only row on
// the kill arm: the session is proven live on the socket the process's own
// TMUX env names, but its pane tree could not be read — so membership is
// undecidable and the process is reported, never killed. Its remedy must be
// exactly the one true for an unreadable pane tree (#6716).
func TestOrphanPaneTreeUnreadable_ReportOnly(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient server; the session lives elsewhere
	home := testguard.SocketTempDir(t)

	// A live af session on a private socket (server A) the ambient listing
	// cannot reach — the deathLiveElsewhere shape.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-pane-unreadable"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	serverAPID := tmuxServerPID(t, sockA)
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                fmt.Sprintf("%s,%d,0", sockA, serverAPID),
	})

	prev := captureSessionProcessTrees
	captureSessionProcessTrees = func(ctx *scanContext, name, socket string) ([]proctree.Process, error) {
		if name == liveName {
			return nil, errors.New("pane tree read staged to fail")
		}
		return prev(ctx, name, socket)
	}
	t.Cleanup(func() { captureSessionProcessTrees = prev })

	opts := testOptionsWithHome(t, home, true, victim.PID, serverAPID)
	stageParent(&opts, victim.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)

	vf := findingForPID(findByCheck(report, "orphaned-process"), victim.PID)
	require.NotNil(t, vf,
		"an unreadable pane tree must land on the report-only row, not on escaped-process")
	require.Contains(t, vf.Detail, "pane tree could not be read",
		"the finding must name the true cause: %s", vf.Detail)
	require.Empty(t, vf.FixAction,
		"an unreadable pane tree must never arm a kill: %s", vf.Detail)
	require.Equal(t,
		"inspect the session's pane tree on the tmux socket named above — "+
			"the process is report-only",
		vf.Remediation,
		"the pane-unreadable remedy must be exactly the true one (#6716)")
	require.Nil(t, findingForPID(findByCheck(report, "escaped-process"), victim.PID),
		"a process whose membership could not be read must not be classified escaped")
	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose pane tree could not be read (#6716)")
}
