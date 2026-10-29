package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Two same-shape creates whose answers are both lost each keep their own
// uncertain-execution record, and their retries each rejoin their own execution
// (#6021, property 1).
func TestSameShapeUncertainCreatesKeepEachRecord(t *testing.T) {
	lose := func(t *testing.T) (*lifecycleSandbox, *Instance) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		for _, id := range []string{"exec-a", "exec-b"} {
			_, err := inst.AddProcessTabWithID(id, "deploy.sh", "")
			require.Error(t, err)
		}
		require.Equal(t, 2, sb.execs("deploy.sh"), "both commands started")
		sb.mu.Lock()
		sb.dropCreate = false
		sb.mu.Unlock()
		return sb, inst
	}

	t.Run("both retried: each joins its own execution", func(t *testing.T) {
		sb, inst := lose(t)
		a, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.NoError(t, err)
		b, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.NoError(t, err)
		assert.NotEqual(t, a.ID, b.ID, "the second retry must not join the first's execution")
		assert.Equal(t, 2, sb.execs("deploy.sh"), "no retry runs the command again")
		assert.Equal(t, sb.ids(), rosterIDs(inst))
		assert.Empty(t, inst.tabCreateIntents)
	})

	t.Run("both retried concurrently", func(t *testing.T) {
		sb, inst := lose(t)
		var wg sync.WaitGroup
		got := make([]*Tab, 2)
		for k := range got {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				tab, err := inst.AddProcessTabWithID("", "deploy.sh", "")
				assert.NoError(t, err)
				got[k] = tab
			}(k)
		}
		wg.Wait()
		require.NotNil(t, got[0])
		require.NotNil(t, got[1])
		assert.NotEqual(t, got[0].ID, got[1].ID)
		assert.Equal(t, 2, sb.execs("deploy.sh"))
		assert.Equal(t, sb.ids(), rosterIDs(inst))
	})

	t.Run("the sweep adopts both", func(t *testing.T) {
		sb, inst := lose(t)
		require.True(t, inst.ReconcileOffBoxTabs())
		assert.Equal(t, sb.ids(), rosterIDs(inst), "neither execution may drop out of the roster")
		assert.Len(t, rosterIDs(inst), 2)
	})

	t.Run("concurrent id-less creates never orphan an execution", func(t *testing.T) {
		for round := 0; round < 20; round++ {
			sb, inst := newLifecycleSandbox(t)
			sb.mu.Lock()
			sb.dropCreate = true
			sb.mu.Unlock()
			var wg sync.WaitGroup
			for k := 0; k < 2; k++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = inst.AddProcessTabWithID("", "deploy.sh", "")
				}()
			}
			wg.Wait()
			inst.ReconcileOffBoxTabs()
			assert.Equal(t, sb.ids(), rosterIDs(inst), "round %d", round)
		}
	})
}

// A sandbox close whose teardown fails does not confirm the close: the daemon's
// durable obligation survives every sweep until the sandbox says the tab is
// really gone (#6022, property 5).
func TestFailedSandboxTeardownKeepsThePendingClose(t *testing.T) {
	sb, inst := newLifecycleSandbox(t)
	tab, err := inst.AddProcessTabWithID("stuck-id", "tail -f log", "")
	require.NoError(t, err)
	sb.mu.Lock()
	sb.stuck = map[string]bool{tab.ID: true}
	sb.mu.Unlock()

	require.Error(t, inst.CloseTabByID(tab.ID), "an unconfirmed teardown is reported")
	require.Len(t, inst.PendingRemoteTabClose(), 1)

	// The sandbox dropped the row on the first close; a retry that read that as
	// "gone" would discard the obligation while the command keeps running.
	for k := 0; k < 3; k++ {
		inst.ReconcileOffBoxTabs()
		require.Len(t, inst.PendingRemoteTabClose(), 1, "sweep %d retired an unconfirmed close", k)
	}

	sb.mu.Lock()
	sb.stuck = nil
	sb.mu.Unlock()
	assert.True(t, inst.ReconcileOffBoxTabs())
	assert.Empty(t, inst.PendingRemoteTabClose(), "the sandbox's tab_gone answer retires it")
}

// The sandbox side of #6022: a conditional close that finds no row still spends
// the teardown handle a failed close retained, and answers "gone" only when it
// is confirmed.
func TestConditionalCloseSpendsRetainedTeardown(t *testing.T) {
	const agentName = "af_gone-teardown_agent"
	reapable := agentName + "__gone"
	stuck := agentName + "__stuck"
	mockExec, aliveFn, _ := cleanupExec(
		map[string]bool{agentName: true, reapable: true, stuck: true},
		map[string]bool{stuck: true})
	pty := persistPtyFactory{t: t, cmdExec: mockExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, mockExec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	data := cleanupInstanceData(t, agentName, agentName+"__shell")
	data.PendingTabCleanup = []TabCleanupData{
		{TabID: "closed-ok", TmuxName: reapable},
		{TabID: "closed-stuck", TmuxName: stuck},
	}
	inst, err := FromInstanceData(data)
	require.NoError(t, err)

	err = inst.CloseTabByIDIfGen("closed-stuck", "g")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrTabGone, "a failed teardown is not 'gone'")
	assert.Len(t, inst.PendingTabCleanup(), 2, "no handle retired yet")

	err = inst.CloseTabByIDIfGen("closed-ok", "g")
	require.ErrorIs(t, err, ErrTabGone)
	assert.False(t, aliveFn(reapable))
	assert.Equal(t, []TabCleanupData{{TabID: "closed-stuck", TmuxName: stuck}}, inst.PendingTabCleanup())

	require.ErrorIs(t, inst.CloseTabByIDIfGen("never-existed", "g"), ErrTabGone)
}
