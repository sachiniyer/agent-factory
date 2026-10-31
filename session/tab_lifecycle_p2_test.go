package session

import (
	"sync"
	"testing"
	"time"

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

// A retry of a request shape that has an intent never mints a new id, even when
// every matching intent is already being retried: it joins that attempt, so the
// command cannot run a second time (#6030, property 1).
func TestRetryJoinsInFlightIntentWhenNoneIsFree(t *testing.T) {
	sb, inst := newLifecycleSandbox(t)
	sb.mu.Lock()
	sb.dropCreate = true
	sb.mu.Unlock()
	_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
	require.Error(t, err)
	require.Equal(t, 1, sb.execs("deploy.sh"))
	var lost string
	inst.mu.RLock()
	for _, in := range inst.tabCreateIntents {
		lost = in.id
	}
	inst.mu.RUnlock()
	require.NotEmpty(t, lost)

	hold := make(chan struct{})
	sb.mu.Lock()
	sb.dropCreate = false
	sb.hold = hold
	sb.mu.Unlock()

	var wg sync.WaitGroup
	got := make([]*Tab, 2)
	run := func(k int) {
		defer wg.Done()
		tab, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		assert.NoError(t, err)
		got[k] = tab
	}
	wg.Add(1)
	go run(0)
	require.Eventually(t, func() bool {
		inst.mu.RLock()
		defer inst.mu.RUnlock()
		_, inFlight := inst.tabCreateInFlight[lost]
		return inFlight
	}, 5*time.Second, 5*time.Millisecond, "the first retry must be in flight")
	wg.Add(1)
	go run(1)
	time.Sleep(150 * time.Millisecond) // let the second retry reach its join
	close(hold)
	wg.Wait()

	require.NotNil(t, got[0])
	require.NotNil(t, got[1])
	assert.Equal(t, lost, got[0].ID)
	assert.Equal(t, lost, got[1].ID, "the second retry joins the in-flight attempt")
	assert.Equal(t, 1, sb.execs("deploy.sh"), "the command ran once")
	assert.Equal(t, sb.ids(), rosterIDs(inst))
}

// Teardown attempts for one tab are serialized: a conditional close retrying a
// teardown waits for the attempt already running, and the reserved name is
// released only once an attempt confirms (#6030, property 4).
func TestTeardownAttemptsForOneTabAreSerialized(t *testing.T) {
	const agentName = "af_serialize_agent"
	reapable := agentName + "__t1"
	mockExec, aliveFn, _ := cleanupExec(map[string]bool{agentName: true, reapable: true}, nil)
	pty := persistPtyFactory{t: t, cmdExec: mockExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, mockExec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	data := cleanupInstanceData(t, agentName, agentName+"__shell")
	data.PendingTabCleanup = []TabCleanupData{{TabID: "t1", TmuxName: reapable}}
	inst, err := FromInstanceData(data)
	require.NoError(t, err)

	release := inst.lockTeardown("t1") // an attempt is mid-teardown
	done := make(chan error, 1)
	go func() { done <- inst.CloseTabByIDIfGen("t1", "g") }()

	select {
	case err := <-done:
		t.Fatalf("a second attempt ran while the first was tearing down: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	assert.True(t, aliveFn(reapable), "the session must not be touched by the waiting attempt")
	require.Len(t, inst.PendingTabCleanup(), 1, "the reservation holds while an attempt runs")

	release()
	require.ErrorIs(t, <-done, ErrTabGone)
	assert.False(t, aliveFn(reapable))
	assert.Empty(t, inst.PendingTabCleanup(), "the confirming attempt releases the name")

	// Unrelated tabs do not queue behind it.
	other := inst.lockTeardown("t1")
	otherDone := make(chan struct{})
	go func() { inst.lockTeardown("t2")(); close(otherDone) }()
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown of a different tab blocked")
	}
	other()
}

// A definitive answer consumes only the intent of the request it answers: a
// different-shape create that reuses the id and is refused leaves the original
// attempt's intent, so the sweep can still adopt its running execution and its
// id-less retry still joins it (#6030, property 1).
func TestRefusedConflictingCreateKeepsTheOriginalIntent(t *testing.T) {
	sb, inst := newLifecycleSandbox(t)
	sb.mu.Lock()
	sb.dropCreate = true
	sb.mu.Unlock()
	_, err := inst.AddProcessTabWithID("shared-id", "A.sh", "")
	require.Error(t, err)
	sb.mu.Lock()
	sb.dropCreate = false
	sb.mu.Unlock()

	// Same id, different command: the sandbox's id idempotency answers with A's
	// tab, which the daemon refuses as a conflict for B.
	_, err = inst.AddProcessTabWithID("shared-id", "B.sh", "")
	require.Error(t, err)
	assert.Equal(t, 0, sb.execs("B.sh"), "B must not run")
	inst.mu.RLock()
	_, kept := inst.tabCreateIntents[intentKey("shared-id", TabKindProcess, "A.sh", "")]
	inst.mu.RUnlock()
	require.True(t, kept, "B's refusal consumed A's intent")

	t.Run("the sweep still adopts A's execution", func(t *testing.T) {
		assert.True(t, inst.ReconcileOffBoxTabs())
		assert.Equal(t, sb.ids(), rosterIDs(inst))
		assert.Equal(t, 1, sb.execs("A.sh"))
	})
	t.Run("an id-less retry of A joins it, never a fresh run", func(t *testing.T) {
		tab, err := inst.AddProcessTabWithID("", "A.sh", "")
		require.NoError(t, err)
		assert.Equal(t, "shared-id", tab.ID)
		assert.Equal(t, 1, sb.execs("A.sh"))
	})
}

// Adopting a sandbox tab keeps the name reserved for the create, exactly as a
// fresh create does: the sandbox names a shell against its own roster, which
// does not hold the daemon's web tabs (#5962).
func TestAdoptHonoursTheReservedName(t *testing.T) {
	addWeb := func(inst *Instance) {
		inst.mu.Lock()
		inst.Tabs = append(inst.Tabs, &Tab{ID: "web-1", Name: "shell", Kind: TabKindWeb, URL: "http://example.invalid"})
		inst.mu.Unlock()
	}
	names := func(inst *Instance) map[string]int {
		inst.mu.RLock()
		defer inst.mu.RUnlock()
		out := map[string]int{}
		for _, tab := range inst.Tabs {
			out[tab.Name]++
		}
		return out
	}

	t.Run("a fresh create", func(t *testing.T) {
		_, inst := newLifecycleSandbox(t)
		addWeb(inst)
		tab, err := inst.AddShellTabWithID("")
		require.NoError(t, err)
		assert.Equal(t, "shell-2", tab.Name)
		assert.Equal(t, 1, names(inst)["shell"])
	})

	t.Run("a sweep adoption", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		addWeb(inst)
		_, err := inst.AddShellTabWithID("")
		require.Error(t, err)
		require.True(t, inst.ReconcileOffBoxTabs())
		assert.Equal(t, 1, names(inst)["shell"], "the adopted shell must not duplicate the web tab's name")
		assert.Equal(t, 1, names(inst)["shell-2"])
	})
}

// An intent whose replay window ran out is not discarded while its execution may
// exist: the sweep adopts it, or discards it only on a roster read that lacks
// the id (#6023).
func TestExpiredIntentIsAdoptedNotDiscarded(t *testing.T) {
	expire := func(inst *Instance) {
		inst.mu.Lock()
		for _, in := range inst.tabCreateIntents {
			in.at = time.Now().Add(-tabIntentTTL - time.Second)
		}
		inst.mu.Unlock()
	}
	t.Run("an orphaned command rejoins the roster after reconnect", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.Error(t, err)
		expire(inst)

		// Another request's roster read must not eat the expired intent either.
		_, err = inst.AddProcessTabWithID("", "other.sh", "")
		require.Error(t, err)

		require.True(t, inst.ReconcileOffBoxTabs())
		assert.Equal(t, sb.ids(), rosterIDs(inst), "both executions are on the roster")
	})
	t.Run("a roster read that lacks the id proves it gone", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.refuseNew = false
		sb.mu.Unlock()
		inst.mu.Lock()
		inst.recordIntentLocked("never-ran", TabKindProcess, "ghost.sh", "", "ghost-sh")
		inst.mu.Unlock()
		inst.ReconcileOffBoxTabs()
		assert.NotEmpty(t, inst.tabCreateIntents, "an unexpired intent waits")
		expire(inst)
		inst.ReconcileOffBoxTabs()
		assert.Empty(t, inst.tabCreateIntents)
	})
}

// A restored sandbox is a fresh one that never heard of the session's shell and
// process tabs: their rows are retired instead of erroring ErrTabGone forever
// (#5954).
func TestReplacedSandboxRetiresRowsItDoesNotHold(t *testing.T) {
	setup := func(t *testing.T) (*lifecycleSandbox, *Instance, *Tab, *Tab) {
		sb, inst := newLifecycleSandbox(t)
		shell, err := inst.AddShellTabWithID("")
		require.NoError(t, err)
		proc, err := inst.AddProcessTabWithID("", "tail -f log", "")
		require.NoError(t, err)
		inst.mu.Lock()
		inst.Tabs = append(inst.Tabs, &Tab{ID: "web-1", Name: "docs", Kind: TabKindWeb, URL: "https://example.com"})
		inst.mu.Unlock()
		return sb, inst, shell, proc
	}

	t.Run("the replacement holds none of them", func(t *testing.T) {
		sb, inst, shell, proc := setup(t)
		sb.mu.Lock()
		sb.tabs = map[string]TabData{} // archive reaped the old sandbox; the new one is empty
		sb.mu.Unlock()

		require.True(t, inst.ReconcileOffBoxTabs())
		ids := rosterIDs(inst)
		assert.NotContains(t, ids, shell.ID)
		assert.NotContains(t, ids, proc.ID)
		assert.Contains(t, ids, "web-1", "a metadata-only tab has no sandbox runtime to lose")
		assert.Equal(t, agentTabName, inst.Tabs[0].Name)

		as := inst.AgentServer().(*remoteAgentServer)
		_, err := as.SubscribeTab(shell.ID, 0)
		assert.ErrorIs(t, err, ErrTabGone, "its stream ends rather than hanging on a dead id")
	})

	t.Run("a tab the sandbox still holds stays", func(t *testing.T) {
		_, inst, shell, proc := setup(t)
		inst.ReconcileOffBoxTabs()
		assert.Contains(t, rosterIDs(inst), shell.ID)
		assert.Contains(t, rosterIDs(inst), proc.ID)
	})

	t.Run("a different incarnation under the same id is not the row's runtime", func(t *testing.T) {
		sb, inst, shell, _ := setup(t)
		sb.mu.Lock()
		td := sb.tabs[shell.ID]
		td.Gen = "another-incarnation"
		sb.tabs[shell.ID] = td
		sb.mu.Unlock()
		inst.ReconcileOffBoxTabs()
		assert.NotContains(t, rosterIDs(inst), shell.ID)
	})

	t.Run("a row created after the roster was read is not retired", func(t *testing.T) {
		_, inst, shell, _ := setup(t)
		// The snapshot predates the row, as it does for a create that finishes
		// while the sweep's read is in flight.
		retired := inst.retireLostOffBoxTabs(map[string]string{}, map[string]TabData{})
		assert.Empty(t, retired)
		assert.Contains(t, rosterIDs(inst), shell.ID)
	})
}
