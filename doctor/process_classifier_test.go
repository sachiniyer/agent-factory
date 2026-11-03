package doctor

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	require.Contains(t, vf.Remediation, "inspect the process",
		"the remedy must be true for the no-marker reason: %s", vf.Remediation)
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
	require.Contains(t, unresolved[0].Remediation, "inspect the process",
		"the remedy must be true for the no-marker reason: %s", unresolved[0].Remediation)
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
	require.Contains(t, unresolved[0].Remediation, "inspect the process",
		"the remedy must be true for the missing-derived-home reason: %s", unresolved[0].Remediation)
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
