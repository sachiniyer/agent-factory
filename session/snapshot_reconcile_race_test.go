package session

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #2100 race, and why it needs its own harness.
//
// ReconcileTabsFromData reconnects a daemon-listed tab OUTSIDE i.mu — an
// attach-only Restore, which shells out to tmux — and then re-acquires the lock
// to append it. Every other tab-add path rechecks the teardown fence in that
// second acquisition (AddShellTab/AddProcessTab in tab_spawn.go, AttachShellTab
// in instance.go); the reconcile did not, so a Kill or an archive landing in the
// window spliced a tab into a roster that was already being torn down.
//
// The window is opened by exactly one observable event: the `tmux has-session`
// probe the attach-only Restore issues for the sibling session. Hooking that
// probe is what makes the race deterministic rather than a timing test.

// reconcileRaceExec wraps nameKeyedExec with two things a race test needs: a
// hook that fires the FIRST time the sibling session's existence is probed (the
// restore→append window), and a record of whether that daemon-owned session was
// ever kill-session'd. The hook runs AFTER the probe answers, so Restore
// completes its rebind and the reconcile goes on to the append it must skip.
func reconcileRaceExec(agentName, shellTmuxName string, onRestore func()) (cmd_test.MockCmdExec, func() bool) {
	base := nameKeyedExec(map[string]bool{agentName: true, shellTmuxName: true})
	fired, killed := false, false
	hooked := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			// Match on the sibling's name, not the agent's: the sibling name embeds the
			// agent name ("<agent>__shell"), so this direction cannot alias.
			s := cmd.String()
			mine := strings.Contains(s, shellTmuxName)
			if mine && strings.Contains(s, "kill-session") {
				killed = true
			}
			err := base.RunFunc(cmd)
			if onRestore != nil && !fired && mine && strings.Contains(s, "has-session") {
				fired = true
				onRestore()
			}
			return err
		},
		OutputFunc: base.OutputFunc,
	}
	return hooked, func() bool { return killed }
}

// TestReconcileTabsFromData_TeardownRaceDropsRestoredTab is the #2100 fix: a
// Kill/Archive landing between the attach-only Restore and the append must leave
// the tab OUT of the roster, so the reconcile cannot splice a tab into a session
// teardownTabs has already snapshotted for cleanup.
//
// The three stale transitions are the three the fence covers, and they are not
// interchangeable. A daemon-side Kill clears started; the TUI's own kill is an
// OPTIMISTIC overlay that raises OpKilling and leaves started true; and archive
// deliberately keeps started true and fences with OpArchiving instead (#1195), so
// a started-only recheck — the one AttachShellTab shipped with — never fires for
// the archive path at all.
//
// It must also NOT kill the session it declines to adopt. This loop only ever
// ATTACHES (Restore("") errors instead of spawning, #1152) to a session the
// daemon spawned and owns (#960); the kill it is racing is still revertible
// (RevertKill on a failed teardown, AbortArchiveToLost on a failed move), so a
// kill-session here would destroy a live tab over an operation that gets undone.
func TestReconcileTabsFromData_TeardownRaceDropsRestoredTab(t *testing.T) {
	const agentName = "af_snap_race"
	shellName := agentName + shellTmuxSuffix

	for _, tc := range []struct {
		name  string
		stale func(t *testing.T, i *Instance)
	}{
		{
			name:  "kill clears started",
			stale: func(_ *testing.T, i *Instance) { flipStarted(i) },
		},
		{
			name: "optimistic kill raises OpKilling",
			stale: func(t *testing.T, i *Instance) {
				require.NoError(t, i.Transition(BeginKill()))
			},
		},
		{
			name: "archive raises OpArchiving over started=true",
			stale: func(t *testing.T, i *Instance) {
				require.NoError(t, i.Transition(BeginArchive()))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var inst *Instance
			mockExec, sessionKilled := reconcileRaceExec(agentName, shellName, func() {
				tc.stale(t, inst)
			})
			inst, _ = newReconcileTestInstanceWithExec(t, agentName, mockExec)

			target := []TabData{
				{ID: "agent-id", Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
				{ID: "shell-id", Name: "shell", Kind: TabKindShell, TmuxName: shellName},
			}

			changed, err := inst.ReconcileTabsFromData(target)
			require.NoError(t, err, "a tab dropped for teardown is not a reconcile failure")
			assert.False(t, changed, "a tab that was not adopted is not a change")
			assert.Equal(t, 1, inst.TabCount(),
				"a tab restored into a tearing-down session must not be appended (#2100)")
			assert.False(t, sessionKilled(),
				"the projection must not kill the daemon-owned session; the kill it raced can still be reverted")
		})
	}
}

// TestReconcileTabsFromData_NoTeardownRaceStillAdopts is the no-regression half:
// with nothing racing, the same harness must still restore the out-of-band tab,
// append it, and report the change — and a repeat reconcile must still dedupe it
// rather than appending a second copy.
func TestReconcileTabsFromData_NoTeardownRaceStillAdopts(t *testing.T) {
	const agentName = "af_snap_norace"
	shellName := agentName + shellTmuxSuffix

	mockExec, sessionKilled := reconcileRaceExec(agentName, shellName, nil)
	inst, _ := newReconcileTestInstanceWithExec(t, agentName, mockExec)

	target := []TabData{
		{ID: "agent-id", Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "shell-id", Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	}

	changed, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.True(t, changed, "adopting an out-of-band tab is a change")
	require.Equal(t, 2, inst.TabCount(), "the out-of-band tab must still be adopted")
	assert.Equal(t, shellName, inst.GetTabs()[1].tmux.SanitizedName(),
		"the adopted tab must bind to its EXACT persisted tmux session")
	assert.False(t, sessionKilled(), "nothing was torn down, so nothing may be killed")

	changedAgain, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.False(t, changedAgain, "an unchanged snapshot must not report a change")
	assert.Equal(t, 2, inst.TabCount(), "the already-present dedupe must still hold")
}
