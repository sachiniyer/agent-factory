package doctor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	// transientAncestorRemedy is the remedy for ancestor gaps that can be a
	// mid-scan race: the table may answer on a rerun.
	transientAncestorRemedy = "rerun `af doctor` — the process table may have raced this scan; " +
		"inspect the process manually if it persists"
	// structuralAncestorRemedy is the remedy for gaps no rerun can close: a
	// chain that loops or outruns the bound is truncated every time.
	structuralAncestorRemedy = "inspect the process's ancestor chain manually — it could not be fully verified"
)

// These tests pin #6703: on the kill path the ancestor walk must fail CLOSED.
// Ownership of a marked orphan is proven from the process's own environment
// and from a fully verified ancestor chain — every recorded ancestor must
// still name the same live process and answer its environment. Before the fix
// the walk failed open on exactly the gaps that matter: an ancestor whose
// environ read errored "declared nothing" and was skipped, and an ancestor
// that exited or was recycled between the snapshot and the walk was trusted
// from the stale record. Either way the chain came back "clean" and the kill
// armed. Each test stages one unverifiable-ancestor shape and asserts the
// process is reported as unproven — never armed — while a genuinely-ours
// orphan of the same dead session (reparented to init, ancestry verified) is
// still armed and killed.

// orphanAncestorFixture spawns the shared shape: a marked victim of a dead
// session of this home — its own AGENT_FACTORY_HOME provably ours and the
// session's death corroborated by the ambient TMUX env — and a control orphan
// of the SAME session with the same proven markers, the kill arm's positive
// control. The victim's recorded parent is corrupted per test; the control is
// reparented to init, so its ancestry is verified clean by construction.
func orphanAncestorFixture(t *testing.T, home, session string) (victim, control proctree.Process) {
	t.Helper()
	env := map[string]string{
		tmux.EnvMarkerSession: session,
		tmux.EnvMarkerHome:    home,
		"AGENT_FACTORY_HOME":  home,
		"TMUX":                ambientTmuxEnv(t),
	}
	victim = spawnWithEnv(t, "sh", nil, env)
	control = spawnWithEnv(t, "sh", nil, env)
	return victim, control
}

// stageParent records parent as child's parent in the run's snapshot, applying
// mutate (when non-nil) to the parent's snapshot entry — e.g. corrupting its
// recorded start stamp so it no longer names the live process it stood for.
// parent == 1 stages a reparented orphan: no ancestor left to interrogate.
func stageParent(opts *Options, child, parent int, mutate func(*proctree.Process)) {
	base := opts.snapshot
	opts.snapshot = func() (map[int]proctree.Process, error) {
		snap, err := base()
		if err != nil {
			return nil, err
		}
		if v, ok := snap[child]; ok {
			v.PPID = parent
			snap[child] = v
		}
		if mutate != nil {
			if a, ok := snap[parent]; ok {
				mutate(&a)
				snap[parent] = a
			}
		}
		return snap, nil
	}
}

// stubProcessEnvError makes doctor's process-environment reader fail for pid —
// the shape a denied, withheld, or raced-away ancestor environment takes.
// Other pids keep their real environ.
func stubProcessEnvError(t *testing.T, pid int, cause error) {
	t.Helper()
	prev := daemonProcessEnvLookup
	t.Cleanup(func() { daemonProcessEnvLookup = prev })
	daemonProcessEnvLookup = func(p int, key string) (string, bool, error) {
		if p != pid {
			return prev(p, key)
		}
		return "", false, cause
	}
}

func findingForPID(fs []Finding, pid int) *Finding {
	tag := fmt.Sprintf("pid %d (", pid)
	for i := range fs {
		if strings.Contains(fs[i].Detail, tag) {
			return &fs[i]
		}
	}
	return nil
}

// assertAncestorRefusal asserts the fail-closed outcome: the victim is
// reported on the non-attributing foreign-home-unresolved row as UNPROVEN —
// naming the ancestor that could not be verified, the true reason, and the
// exact remedy for that reason — with no fix armed, and stays alive; the
// control, same dead session with verified ancestry, is armed and killed.
func assertAncestorRefusal(t *testing.T, report *Report, victim, control proctree.Process, ancestorPID int, reason, remedy string) {
	t.Helper()
	orphans := findByCheck(report, "orphaned-process")

	vf := findingForPID(findByCheck(report, "foreign-home-unresolved"), victim.PID)
	require.NotNil(t, vf, "the unverifiable-ancestor victim must still be reported")
	require.Contains(t, vf.Detail, "unproven",
		"the finding must state ownership is unproven: %s", vf.Detail)
	require.Contains(t, vf.Detail, fmt.Sprintf("%d", ancestorPID),
		"the finding must name the ancestor that could not be verified: %s", vf.Detail)
	require.Contains(t, vf.Detail, reason,
		"the finding must say WHY the ancestor could not be verified: %s", vf.Detail)
	require.Empty(t, vf.FixAction,
		"an unverifiable ancestor must never arm a kill: %s", vf.Detail)
	require.Equal(t, remedy, vf.Remediation,
		"the remedy must be exactly the one true for this cause (#6716)")
	require.Nil(t, findingForPID(orphans, victim.PID),
		"an unproven process must not be counted among this install's orphans: %s", vf.Detail)
	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose ancestor it could not verify (#6703)")

	cf := findingForPID(orphans, control.PID)
	require.NotNil(t, cf, "the proven-ancestry control orphan must still be reported")
	require.NotEmpty(t, cf.FixAction,
		"a genuinely-ours orphan with a verified ancestry must still arm the kill")
	require.True(t, cf.Fixed, "fix outcome: %v", cf.FixErr)
	require.Eventually(t, func() bool { return !alive(control) }, 5*time.Second, 25*time.Millisecond,
		"a genuinely-ours orphan with a verified ancestry must still be killed by --fix")
}

// TestOrphanKillArmAncestorEnvUnreadable_NotArmed stages the #6703 headline
// case: an ancestor whose environ read FAILS. Before the fix the walk counted
// the read error as "declares nothing" and armed the kill; now an ancestor
// whose environ cannot be read is an ancestor whose home cannot be ruled out.
func TestOrphanKillArmAncestorEnvUnreadable_NotArmed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	victim, control := orphanAncestorFixture(t, home, "af_doctor-unreadable-ancestor")

	// A real ancestor that declares no home — except its environ is made
	// unreadable, so even that cannot be known. AGENT_FACTORY_HOME="" here so
	// that if the env were somehow read anyway, it would still declare
	// nothing rather than the sandbox's ambient home.
	ancestor := spawnWithEnv(t, "sh", nil, map[string]string{
		"AGENT_FACTORY_HOME": "",
		"HOME":               t.TempDir(),
	})
	stubProcessEnvError(t, ancestor.PID, proctree.ErrEnvUnreadable)

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID, ancestor.PID) // Fix: true
	stageParent(&opts, victim.PID, ancestor.PID, nil)
	stageParent(&opts, control.PID, 1, nil)
	stageParent(&opts, ancestor.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)
	assertAncestorRefusal(t, report, victim, control, ancestor.PID, "environment could not be read", transientAncestorRemedy)
}

// TestOrphanKillArmAncestorIdentityChanged_NotArmed stages the mid-scan race:
// the pid the snapshot recorded as the victim's parent no longer names the
// same process — it exited and the pid was recycled between the stat pass and
// the walk. The stale snapshot record says nothing about the process now
// running, so trusting it is trusting a different process's environ.
func TestOrphanKillArmAncestorIdentityChanged_NotArmed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	victim, control := orphanAncestorFixture(t, home, "af_doctor-recycled-ancestor")

	// A real ancestor that declares no home; the snapshot entry for it is
	// corrupted below so its recorded (pid, StartID) pair no longer matches
	// the live process — the shape PID reuse takes mid-scan.
	ancestor := spawnWithEnv(t, "sh", nil, map[string]string{
		"AGENT_FACTORY_HOME": "",
		"HOME":               t.TempDir(),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID, ancestor.PID) // Fix: true
	stageParent(&opts, victim.PID, ancestor.PID, func(a *proctree.Process) { a.StartID++ })
	stageParent(&opts, control.PID, 1, nil)
	stageParent(&opts, ancestor.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)
	assertAncestorRefusal(t, report, victim, control, ancestor.PID, "no longer names the process", transientAncestorRemedy)
}

// TestOrphanKillArmAncestorMissingFromSnapshot_NotArmed stages the other half
// of the mid-scan race: the recorded parent is absent from the snapshot
// entirely — it exited (or was reaped) between the victim's stat and its own.
// A live ancestor the snapshot cannot show is unverified the same way.
func TestOrphanKillArmAncestorMissingFromSnapshot_NotArmed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	victim, control := orphanAncestorFixture(t, home, "af_doctor-vanished-ancestor")

	// A real ancestor, deliberately left OUT of the snapshot's pid list: the
	// victim's recorded PPID names a process the scan never saw.
	ancestor := spawnWithEnv(t, "sh", nil, map[string]string{
		"AGENT_FACTORY_HOME": "",
		"HOME":               t.TempDir(),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID) // Fix: true
	stageParent(&opts, victim.PID, ancestor.PID, nil)
	stageParent(&opts, control.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)
	assertAncestorRefusal(t, report, victim, control, ancestor.PID, "missing from the process snapshot", transientAncestorRemedy)
}

// TestOrphanKillArmAncestorCycle_Unproven stages a parent chain that loops: the
// victim's recorded parent records the VICTIM as its parent. A cycle means the
// chain never reaches init — before #6704 the seen-set ended the walk as
// "clean", but a chain that cannot reach init is unverified, not clean.
func TestOrphanKillArmAncestorCycle_Unproven(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	victim, control := orphanAncestorFixture(t, home, "af_doctor-cyclic-ancestor")

	ancestor := spawnWithEnv(t, "sh", nil, map[string]string{
		"AGENT_FACTORY_HOME": "",
		"HOME":               t.TempDir(),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID, ancestor.PID) // Fix: true
	stageParent(&opts, victim.PID, ancestor.PID, nil)
	stageParent(&opts, ancestor.PID, victim.PID, nil) // the loop
	stageParent(&opts, control.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)
	assertAncestorRefusal(t, report, victim, control, victim.PID, "loops back", structuralAncestorRemedy)
	vf := findingForPID(findByCheck(report, "foreign-home-unresolved"), victim.PID)
	require.NotContains(t, vf.Detail, "ancestor pid",
		"a cycle must not name the victim's own pid as the unverified ancestor: %s", vf.Detail)
}

// TestOrphanKillArmAncestorDepthBound_Unproven stages a chain longer than the
// verification bound. The bound exists so a corrupted table cannot spin the
// walk forever — but a chain truncated by it leaves ancestors unexamined, and
// unexamined is unproven, not clean.
func TestOrphanKillArmAncestorDepthBound_Unproven(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	victim, control := orphanAncestorFixture(t, home, "af_doctor-deep-ancestor")

	a1 := spawnWithEnv(t, "sh", nil, map[string]string{"AGENT_FACTORY_HOME": "", "HOME": t.TempDir()})
	a2 := spawnWithEnv(t, "sh", nil, map[string]string{"AGENT_FACTORY_HOME": "", "HOME": t.TempDir()})

	prev := ancestorHopLimit
	ancestorHopLimit = 1
	t.Cleanup(func() { ancestorHopLimit = prev })

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID, a1.PID, a2.PID) // Fix: true
	stageParent(&opts, victim.PID, a1.PID, nil)
	stageParent(&opts, a1.PID, a2.PID, nil)
	stageParent(&opts, a2.PID, 1, nil)
	stageParent(&opts, control.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)
	assertAncestorRefusal(t, report, victim, control, a2.PID, "verification bound", structuralAncestorRemedy)
}

// TestOrphanKillArmAncestorIdentityMidRead_Unproven stages the narrower
// mid-scan race: the ancestor is still the recorded process when the walk
// first revalidates it, but is gone or recycled by the time its environment
// read returns. Believing the read anyway is trusting whatever the pid names
// NOW — identity must be re-proven after the env read, not only before it.
func TestOrphanKillArmAncestorIdentityMidRead_Unproven(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)
	victim, control := orphanAncestorFixture(t, home, "af_doctor-midread-ancestor")

	ancestor := spawnWithEnv(t, "sh", nil, map[string]string{
		"AGENT_FACTORY_HOME": "",
		"HOME":               t.TempDir(),
	})

	// The first identity revalidation passes; the one guarding the env-read
	// result fails — the shape the ancestor exiting mid-read takes.
	var identityCalls int
	prev := ancestorIdentity
	ancestorIdentity = func(a proctree.Process) (bool, error) {
		if a.PID == ancestor.PID {
			identityCalls++
			if identityCalls > 1 {
				return false, nil
			}
		}
		return prev(a)
	}
	t.Cleanup(func() { ancestorIdentity = prev })

	opts := testOptionsWithHome(t, home, true, victim.PID, control.PID, ancestor.PID) // Fix: true
	stageParent(&opts, victim.PID, ancestor.PID, nil)
	stageParent(&opts, control.PID, 1, nil)
	stageParent(&opts, ancestor.PID, 1, nil)

	report, err := Run(opts)
	require.NoError(t, err)
	assertAncestorRefusal(t, report, victim, control, ancestor.PID, "no longer names the process", transientAncestorRemedy)
}
