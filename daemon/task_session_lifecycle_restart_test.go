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

// TestTaskSessionLifecycle_PreMarkerDeliveryRaceSurvivesAbortOnRestart pins the
// in-window adoption the durable-marker filing used to lose (#4162's race).
//
// In the PAUSED poll path deferTaskSessionLifecycleWhilePaused parks the
// in-memory deferred intent under m.mu, releases m.mu, and only THEN calls
// fileOwedTaskLifecycle to file the durable marker under i.mu. A TUI/browser
// keystroke that lands in that window reaches NoteAdoptionDelivery — which takes
// i.mu ALONE, not m.mu — before owedOnComplete exists, so the delivery bumps the
// in-memory adoption count but discharges nothing durably: there is no marker to
// clear. The marker is then filed with a FiledAt POSTDATING the keystroke.
//
// A restart before the next backstop poll wipes the in-memory count — the only
// adoption evidence — while leaving the post-keystroke marker durable. The
// unpaused drain after restart reads deliveries == atRunEnd == 0 and finds
// lastPaneChurnAt predates marker.FiledAt, so both adoption guards pass, the
// teardown is authorized, and the session the user just typed into is reaped.
//
// The fix takes the filed-or-refuse decision under i.mu in a single critical
// section: a delivery that already advanced deliveries > atRunEnd leaves
// FileOwedOnCompleteIfNotDischarged refusing to file, so the marker is never set
// nor persisted and a restart finds nothing to re-arm.
//
// This test FAILS on the unfixed build: the marker is filed inside that window
// (FiledAt = T3, later than the keystroke), survives the restart, and the
// first unpaused poll reaps the session the user adopted. On the fixed build the
// helper refuses to file, no marker survives, and the drain stands down.
func TestTaskSessionLifecycle_PreMarkerDeliveryRaceSurvivesAbortOnRestart(t *testing.T) {
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
	// the staged session below lands under it. The tmux reattach during
	// RestoreInstances (LocalBackend.Start) restores started=true, which is
	// what routes armOwedTaskLifecyclesLocked through its default (re-park)
	// branch rather than its inert-discharge branch — i.e. the production
	// shape of a restart that DID re-arm an obligation.
	const tmuxName = "af_4162_pre_marker_race"
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedName(tmuxName, "claude"))

	// T0: the durable pre-keystroke pane-churn watermark. The completion tick
	// that ends the run observes idle output, so churn does NOT advance it
	// (RecordPaneChurnCheckpointAtEpoch is gated on obs.Updated); the first
	// post-restart reattach is a Baseline, which also does not advance it. So
	// a marker filed after T0 only fails the churn guard if churn advanced past
	// it — and nothing between the keystroke and the restart moves churn. The
	// marker stayed pre-keystroke on durable state for the bug's guard to pass.
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	inst.ReconcileIdleEvidence(time.Time{}, "", t0)

	// The run ends on the paused tick: atRunEnd is pinned to deliveries=0,
	// lastPaneChurnAt stays at T0, taskRunActive flips false.
	was := endRunOnIdleEdge(t, inst)
	require.True(t, was, "precondition: the run was active before this tick")
	require.False(t, inst.TaskRunActive(), "precondition: the run is over")
	require.Equal(t, t0, inst.LastPaneChurnAt(),
		"precondition: the idle edge does not advance the pane-churn watermark")
	require.Zero(t, inst.AdoptionDeliveriesAtRunEnd(),
		"precondition: the run-end baseline is the no-deliveries state")
	require.Zero(t, inst.AdoptionDeliveries(),
		"precondition: nothing has been delivered yet")

	// Persist the pre-keystroke durable watermark BEFORE the race, so it
	// survives the restart (the drain's churn guard reads durable churn
	// against the durable marker.FiledAt, and the test must surface the bug on
	// both signals exactly as production does).
	_ = manager.persistOwedTaskLifecycle(repo.ID, inst)

	// Park the deferred intent exactly as deferTaskSessionLifecycleWhilePaused
	// does on the paused poll path, then release m.mu — the race window opens
	// here. The window is the gap between this unlock and fileOwedTaskLifecycle's
	// i.mu acquire below; the next call opens it on the production path, this
	// in-test park reproduces that shape so the file marker is filed AFTER the
	// in-window delivery lands (or, on the fixed build, refuses to be filed).
	key := daemonInstanceKey(repo.ID, inst.Title)
	manager.mu.Lock()
	if manager.deferredTaskLifecycle == nil {
		manager.deferredTaskLifecycle = make(map[string]string)
	}
	manager.deferredTaskLifecycle[key] = inst.ID
	manager.mu.Unlock()

	// The TUI/browser keystroke lands in the window: it takes only i.mu, so it
	// crosses the manager lock release (the browser-PTY path InputTab →
	// NoteAdoptionDelivery holds zero manager locks). The marker is not filed
	// yet, so the delivery bumps the in-memory count but discharges nothing
	// durably — there is nothing to clear.
	require.NoError(t, inst.NoteAdoptionDelivery(),
		"a browser PTY keystroke is admitted by the adoption fence — it takes no manager lock")
	require.Equal(t, uint64(1), inst.AdoptionDeliveries(),
		"the in-memory delivery landed before the marker was filed")
	require.Nil(t, inst.OwedOnComplete(),
		"precondition: no durable marker exists yet, so the in-window delivery had nothing durably to clear")

	// The marker is filed AFTER the keystroke — the very race. On the unfixed
	// build this is SetOwedOnComplete + persist with FiledAt postdating the
	// keystroke (T3 > T0). On the fixed build FileOwedOnCompleteIfNotDischarged
	// sees deliveries > atRunEnd under i.mu and refuses to file.
	t3 := t0.Add(time.Minute)
	prev := nowFunc
	nowFunc = func() time.Time { return t3 }
	t.Cleanup(func() { nowFunc = prev })

	manager.fileOwedTaskLifecycle(repo.ID, inst)

	// THE FIX: a pre-filing delivery discharges the obligation — the helper
	// must refuse to file a post-keystroke marker. FAILS on the unfixed build
	// (the marker is set in memory with FiledAt = T3, and on this build that
	// marker has also been persisted to disk).
	require.Nil(t, inst.OwedOnComplete(),
		"a pre-filing delivery discharges the obligation — the helper must refuse to file a post-keystroke marker; "+
			"on the unfixed build this is the post-keystroke durable marker that erase the in-memory adoption signal and re-arm the teardown after a restart")

	// The daemon goes down before the next backstop poll could observe the
	// keystroke's pane output (which would otherwise advance lastPaneChurnAt
	// past T3 and stand the teardown down on its own, masking the bug). On the
	// unfixed build this is what wedges the in-window delivery in memory and
	// leaves only the buggy post-keystroke marker durable.
	manager.stopAndWaitBackgroundMutationsForShutdown()

	// Stage the surviving tmux session under the row's persisted name so the
	// restart load takes tmux's plain reattach branch (LocalBackend.Start
	// restores started=true) rather than respawning an agent the runner lacks.
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", tmuxName).Run(),
		"staging the surviving tmux session")

	restarted, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.RestoreInstances())

	restarted.mu.Lock()
	restored := restarted.instances[key]
	parkedID, parked := restarted.deferredTaskLifecycle[key]
	restarted.mu.Unlock()
	require.NotNil(t, restored, "the session must survive the restart")
	if restored == nil {
		rawRows, rerr := config.LoadRepoInstances(repo.ID)
		require.NoError(t, rerr)
		var items []session.InstanceData
		require.NoError(t, json.Unmarshal(rawRows, &items))
		require.Len(t, items, 1)
		_, merr := fromInstanceDataForRefresh(items[0])
		t.Fatalf("the session must survive the restart (materialization error: %v)", merr)
	}

	// The durable, restart-resilient signals after restore. The in-memory
	// adoption counters are reset by the restart (they are not serialized),
	// and the durable churn watermark survives at T0 — those are the bug's
	// preconditions on durable state, which survive the restart regardless of
	// the fix.
	require.True(t, restored.Started(),
		"precondition: the tmux reattach restored started=true, so armOwedTaskLifecyclesLocked takes its re-park branch (not its inert-discharge branch) — that is what makes the bug reproducible, and on the fixed build simply means there is nothing to re-arm")
	require.Equal(t, t0, restored.LastPaneChurnAt(),
		"the durable pre-keystroke pane-churn watermark survives the restart without advancing")
	require.Zero(t, restored.AdoptionDeliveries(),
		"the in-memory delivery count is reset by the restart — the very evidence lost to the bug")
	require.Zero(t, restored.AdoptionDeliveriesAtRunEnd(),
		"the run-end baseline is likewise reset by the restart")

	// THE FIX (durable half): with no durable marker filed, armOwedTaskLifecyclesLocked
	// finds nothing to re-arm. The intent is NOT re-parked, the durable row
	// carries no marker, and the first unpaused poll's drain finds no parked
	// intent and returns. FAILS on the unfixed build: the marker survives at
	// FiledAt = T3 and armOwedTaskLifecyclesLocked re-parks the intent.
	require.Nil(t, restored.OwedOnComplete(),
		"BUG: a pre-filing delivery discharges the obligation — no durable marker was filed, so restore must not re-arm one")
	require.False(t, parked, "no durable marker was filed, so the restart must not re-park the deferred intent (parkedID=%q)", parkedID)

	// The deferred drain, exactly as the first unpaused poll runs it. On the
	// unfixed build this fires against the re-parked intent and reaches the
	// teardown; on the fixed build there is nothing parked and it returns
	// immediately.
	restarted.applyDeferredTaskSessionLifecycle(repo.ID, restored)

	// Brief settle window so an erroneously-rearmed worker (only the buggy
	// build can launch one) would have its reap observed. On the fixed build
	// no worker exists; this is a no-op wait.

	require.Eventually(t, func() bool {
		restarted.mu.Lock()
		defer restarted.mu.Unlock()
		_, present := restarted.instances[key]
		return present
	}, 2*time.Second, 25*time.Millisecond,
		"BUG: the teardown was authorized on a session the user adopted — a delivery landed before the marker was filed, "+
			"and the restart erased the in-memory evidence while the durable churn watermark predates the post-keystroke marker")

	restarted.mu.Lock()
	restored = restarted.instances[key]
	restarted.mu.Unlock()
	require.NotNil(t, restored, "a session the user adopted across the restart must be left in place")
	require.NotEqual(t, session.LiveArchived, restored.GetLiveness(),
		"nor archived out from under the user — the in-window adoption stands")
	require.Nil(t, restored.OwedOnComplete(),
		"no durable obligation survives — the marker was never filed, and the user's adoption shows in the drain's stand-down")
}
