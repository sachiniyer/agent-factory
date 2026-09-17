package daemon

import (
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// The restart half of task-session teardown (#4162).
//
// A task with on_complete=archive|kill whose post-worktree hooks are still
// running when the daemon shuts down used to lose its declared teardown
// PERMANENTLY: the lifecycle worker carrying it lived only in memory, and the
// completion edge it answered had already been spent (task_run_active is
// persisted false and flips exactly once), so nothing after the restart could
// re-derive that a teardown was owed. The session and its worktree stayed
// forever — one silent leak per interrupted run.
//
// The fix files the obligation on the session's own row BEFORE the hook wait
// begins, so a restart re-arms the intent and re-waits on whatever hook run it
// adopts. These tests reproduce the real shape — hooks in flight, shutdown,
// restart, teardown — without spawning a daemon.

// TestTaskSessionLifecycle_TeardownSurvivesRestartWithHooksInFlight is the
// issue's report end to end: the run finishes while a post-worktree hook is
// still in flight, the daemon goes down mid-wait, and the next generation must
// still deliver the declared kill once the (adopted) hook run completes.
func TestTaskSessionLifecycle_TeardownSurvivesRestartWithHooksInFlight(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	testguard.IsolateTmux(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	inst := registerTaskSpawnedSession(t, manager, repo.ID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	// A persisted agent-tab name: the row carries it across the restart, and
	// the staged session below lands under it. The contract under test is the
	// durable marker, not the tmux respawn — which on a runner without the
	// agent binary spawns a pane that dies under the probe.
	const tmuxName = "af_4162_restart_teardown"
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedName(tmuxName, "claude"))

	// A post-worktree hook still running when the run finishes. The channel
	// stands in for a live scope the way the lifecycle worker sees it — the
	// test never closes it, so the first-generation worker stays parked exactly
	// as it would be when a shutdown interrupts the wait.
	hooksInFlight := make(chan struct{})
	inst.GitWorktreeForTest().SetHooksDoneForTest(hooksInFlight)

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repo.ID, inst, was)

	// The obligation must already be durable — before the wait, before any
	// shutdown could drop the worker carrying it. Read the raw row rather than
	// InstanceData so the check still sees the field's absence, which IS the
	// bug on an unpatched tree.
	raw, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0], "pending_on_complete",
		"the teardown obligation must be filed durably before the hook wait begins — "+
			"a shutdown after this point must not be able to lose it")

	// The daemon shuts down mid-wait. On the fixed build the worker is an
	// admitted background mutation and stands down leaving the marker intact;
	// either way, the in-memory waiter is gone after this.
	manager.stopAndWaitBackgroundMutationsForShutdown()

	// The session's tmux outliving the daemon is the ordinary shape of a
	// restart (a bounce, not a machine outage): stage it on the isolated
	// server under the row's persisted name so the load takes tmux's plain
	// reattach branch rather than respawning an agent the runner lacks.
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", tmuxName).Run(),
		"staging the surviving tmux session")

	// Restart: a fresh manager over the same AF_HOME restores the row. The
	// restored worktree gets a still-running adopted hook — the surrogate for
	// the scope AdoptRunningHookRuns would rebuild from a surviving systemd
	// scope in production.
	restarted, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.RestoreInstances())

	key := daemonInstanceKey(repo.ID, "nightly")
	restarted.mu.Lock()
	restored := restarted.instances[key]
	restarted.mu.Unlock()
	if restored == nil {
		// The row is still on disk; re-materialize it directly so the failure
		// names the actual load error instead of a bare nil.
		rawRows, rerr := config.LoadRepoInstances(repo.ID)
		require.NoError(t, rerr)
		var items []session.InstanceData
		require.NoError(t, json.Unmarshal(rawRows, &items))
		require.Len(t, items, 1)
		_, merr := fromInstanceDataForRefresh(items[0])
		t.Fatalf("the marked session must survive the restart (materialization error: %v)", merr)
	}

	adoptedHooks := make(chan struct{})
	if gw := restored.GitWorktreeForTest(); gw != nil {
		gw.SetHooksDoneForTest(adoptedHooks)
	}

	// The first ordinary drain of the armed obligation. It re-resolves the
	// task's verb and re-waits on the adopted hook run — not reaping out from
	// under it.
	restarted.applyDeferredTaskSessionLifecycle(repo.ID, restored)

	restarted.mu.Lock()
	_, present := restarted.instances[key]
	restarted.mu.Unlock()
	assert.True(t, present,
		"hooks still in flight after the restart: the teardown must re-wait, not reap a worktree a hook may still be writing to")

	close(adoptedHooks)
	require.Eventually(t, func() bool {
		restarted.mu.Lock()
		defer restarted.mu.Unlock()
		_, present := restarted.instances[key]
		return !present
	}, 20*time.Second, 25*time.Millisecond,
		"the declared on_complete=kill must still land after the restart — the lost teardown is the leak #4162 reports")

	// And the row is gone with it: the kill's delete is the marker's discharge.
	raw, err = config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	rows = nil
	require.NoError(t, json.Unmarshal(raw, &rows))
	assert.Empty(t, rows, "a reaped session must not leave its durable record behind")
}

// TestTaskSessionLifecycle_RestartAdoptionStillStandsDown covers the other half
// of the contract: durable adoption evidence. A prompt delivered between the
// restart and the drain discharges the obligation — the fence sees the delivery
// count move — and an attached-pane type after the filing is caught by the
// durable churn watermark. The teardown must not land on the user's session.
func TestTaskSessionLifecycle_RestartAdoptionStillStandsDown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	testguard.IsolateTmux(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	inst := registerTaskSpawnedSession(t, manager, repo.ID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	// Same staging as the hooks-in-flight test: a persisted agent-tab name the
	// restarted load can reattach to, so restore never respawns the agent.
	const tmuxName = "af_4162_restart_adopt"
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedName(tmuxName, "claude"))
	inst.GitWorktreeForTest().SetHooksDoneForTest(make(chan struct{}))

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repo.ID, inst, was)
	manager.stopAndWaitBackgroundMutationsForShutdown()

	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", tmuxName).Run(),
		"staging the surviving tmux session")

	restarted, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.RestoreInstances())

	key := daemonInstanceKey(repo.ID, "nightly")
	restarted.mu.Lock()
	restored := restarted.instances[key]
	restarted.mu.Unlock()
	if restored == nil {
		rawRows, rerr := config.LoadRepoInstances(repo.ID)
		require.NoError(t, rerr)
		var items []session.InstanceData
		require.NoError(t, json.Unmarshal(rawRows, &items))
		require.Len(t, items, 1)
		_, merr := fromInstanceDataForRefresh(items[0])
		t.Fatalf("the marked session must survive the restart (materialization error: %v)", merr)
	}

	// The user adopts the restored session before the drain runs: an
	// agent-server delivery is the adoption the fence already knows how to see.
	require.NoError(t, restored.NoteAdoptionDelivery())

	restarted.applyDeferredTaskSessionLifecycle(repo.ID, restored)
	time.Sleep(300 * time.Millisecond)

	restarted.mu.Lock()
	_, present := restarted.instances[key]
	restarted.mu.Unlock()
	assert.True(t, present, "a session the user adopted across the restart must be left in place")
}
