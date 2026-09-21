package daemon

import (
	"encoding/json"
	"errors"
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

// The post-marker adoption race (#4162's missing discharge half).
//
// The pre-marker race (TestTaskSessionLifecycle_PreMarkerDeliveryRaceSurvivesAbortOnRestart)
// is the symmetric failure the codebase already closes: a delivery lands in the
// window before the durable marker is filed, and FileOwedOnCompleteIfNotDischarged
// refuses to file under i.mu so a post-keystroke marker cannot erase the in-memory
// adoption signal.
//
// The post-marker race is the asymmetric one this file pins. A run finishes, the
// durable marker is filed, the daemon parks on the hook wait, and the user then
// adopts the session by prompting it: NoteAdoptionDelivery clears owedOnComplete in
// memory and fires the notify to persist the discharge. Before this fix the notify
// was a void, best-effort settlement write: its persist error was swallowed,
// NoteAdoptionDelivery returned nil, the caller proceeded to the PTY write, and if
// the daemon died before the next poll's FlushOwedSettlements the in-memory retry
// (settleOwed) was lost while the durable marker survived — re-arming a drain that
// reaped a session whose PTY write had landed. The agent-server path has no other
// durable adoption signal (the pane-churn watermark is disclaimed for input that
// reaches an agent-server entry point), so the lost discharge IS the lost veto.
//
// The fix makes the discharge durable a prerequisite for returning success: the
// notify returns any persist error and NoteAdoptionDelivery propagates it, refusing
// the PTY write; on failure the in-memory marker is restored so a re-attempted
// delivery re-runs the durable clear instead of proceeding on top of a marker the
// failed write left set. The discharge path records no settleOwed retry — that
// in-memory-only retry drained by the next poll is exactly the loss vector this
// closes, and it would race the marker restore.

// TestTaskSessionLifecycle_PostMarkerDischargePersistFailureRefusesDeliveryAndRestoresMarker
// is the focused, no-restart test of the fix's contract: a failed discharge persist
// must (1) refuse the delivery with an error, (2) restore the in-memory marker so a
// retry re-runs the durable clear, (3) leave the durable marker set, (4) record no
// in-memory settleOwed retry, and (5) succeed on the caller's re-attempt once the
// write can land, clearing the durable marker.
func TestTaskSessionLifecycle_PostMarkerDischargePersistFailureRefusesDeliveryAndRestoresMarker(t *testing.T) {
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
	endRunOnIdleEdge(t, inst)

	// File the durable marker (and install the discharge notify) exactly as the
	// completion edge does, without launching the lifecycle worker — the worker
	// is irrelevant to the delivery's discharge contract under test.
	manager.fileOwedTaskLifecycle(repo.ID, inst)
	require.NotNil(t, inst.OwedOnComplete(), "precondition: the durable marker is filed")
	require.Equal(t, uint64(0), inst.AdoptionDeliveries(),
		"precondition: nothing has been delivered yet")

	// Inject a persist failure targeted at the discharge write ONLY. The filing
	// write carries PendingOnComplete != nil and passes through; the seed write
	// carries no TaskID and passes through. The discharge write carries
	// PendingOnComplete == nil with a TaskID and fails.
	prev := testHookPersistInstanceData
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "nightly" && data.TaskID != "" && data.PendingOnComplete == nil {
			return errors.New("injected discharge persist failure (disk error)")
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = prev })

	// THE FIX: the delivery must propagate the discharge persist error and refuse
	// the PTY write rather than proceed on top of a durable marker the failed
	// write left set.
	err = inst.NoteAdoptionDelivery()
	require.Error(t, err, "the durable discharge did not land — NoteAdoptionDelivery must refuse the PTY write")
	assert.Contains(t, err.Error(), "discharge for session", "the error names the discharge that did not persist")

	// The in-memory marker is restored so a re-attempted delivery re-runs the
	// durable clear instead of seeing owedOnComplete == nil and proceeding.
	require.NotNil(t, inst.OwedOnComplete(),
		"the in-memory marker is restored so a retry re-runs the durable clear")

	// The user's intent to adopt is recorded even on the refused delivery: the
	// count is the in-memory stand-down signal a concurrent teardown would read
	// through the fence.
	require.Equal(t, uint64(1), inst.AdoptionDeliveries(),
		"the delivery count advances on the intent to deliver, even when the durable discharge fails")

	// The durable marker survives the failed discharge.
	raw, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0], "pending_on_complete",
		"the discharge did not persist — the durable marker survives")

	// No settleOwed retry was recorded: the discharge path must not lean on an
	// in-memory-only retry a unclean exit is about to lose (and that would race
	// the marker restore above, writing the restored marker back to disk).
	manager.mu.Lock()
	assert.Empty(t, manager.settleOwed, "the discharge path records no in-memory retry — its only retry is the caller's")
	manager.mu.Unlock()

	// The user re-attempts the delivery once the transient error clears: the
	// retry must succeed and durably clear the marker.
	testHookPersistInstanceData = func(string, session.InstanceData) error { return nil }
	require.NoError(t, inst.NoteAdoptionDelivery(),
		"the re-attempted delivery must succeed once the persist can land")
	require.Nil(t, inst.OwedOnComplete(),
		"the in-memory marker is cleared after the retry discharged it")
	require.Equal(t, uint64(2), inst.AdoptionDeliveries(),
		"the retry advances the count a second time")

	// The retry's discharge is durable.
	raw, err = config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	rows = nil
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0], "pending_on_complete",
		"the retry's discharge is durable — no marker survives")
}

// TestTaskSessionLifecycle_PostMarkerDeliveryRaceHonestReapWhenDischargeFailsBeforeUncleanExit
// is the restart half of the post-marker race after the fix: a discharge persist
// failure followed by an unclean exit before the user can re-attempt leaves the
// durable marker set, and the restarted drain reaps the session — but HONESTLY,
// because NoteAdoptionDelivery returned an error and the PTY write never landed.
//
// Before this fix the same sequence reaped a session whose PTY write HAD landed —
// the work-losing window the bug is about. The distinguishing observable is the
// NoteAdoptionDelivery return value: nil (bug, PTY write proceeds) vs. error
// (fix, PTY write refused). The durable-marker-survives-and-reaps outcome is
// common to both; the honesty is that no PTY write landed on top of it.
func TestTaskSessionLifecycle_PostMarkerDeliveryRaceHonestReapWhenDischargeFailsBeforeUncleanExit(t *testing.T) {
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
	const tmuxName = "af_4162_post_marker_race_reap"
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedName(tmuxName, "claude"))

	// Hooks in flight: the lifecycle worker parks on them, so the marker stays
	// filed and the worker stays parked through the (simulated) unclean exit.
	hooksInFlight := make(chan struct{})
	inst.GitWorktreeForTest().SetHooksDoneForTest(hooksInFlight)

	// File the durable marker and park the worker, exactly as the completion
	// edge does in production.
	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repo.ID, inst, was)
	require.NotNil(t, inst.OwedOnComplete(), "precondition: the durable marker is filed")

	// Inject a persist failure targeted at the discharge write ONLY.
	prev := testHookPersistInstanceData
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "nightly" && data.TaskID != "" && data.PendingOnComplete == nil {
			return errors.New("injected discharge persist failure (disk error)")
		}
		return nil
	}

	// THE FIX (distinguishing assertion): the delivery must propagate the
	// discharge persist error and refuse the PTY write. Before the fix this
	// returned nil, the caller wrote to the PTY, and the unclean exit below
	// lost the in-memory retry while leaving the durable marker set — reaping
	// a session whose PTY write had landed.
	require.Error(t, inst.NoteAdoptionDelivery(),
		"the durable discharge did not land — NoteAdoptionDelivery must refuse the PTY write")
	require.NotNil(t, inst.OwedOnComplete(),
		"the in-memory marker is restored so the retry the unclean exit is about to lose is the caller's, not an in-memory one")

	// The durable marker survives the failed discharge.
	raw, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0], "pending_on_complete",
		"the discharge did not persist — the durable marker survives the failed discharge")

	// Restore the hook so the restart's reap path is unaffected by the injected
	// failure: the bug is about the DELIVERY's discharge, not the reap's writes.
	testHookPersistInstanceData = prev

	// The daemon goes down before the user can re-attempt (the in-memory marker
	// restore and the in-memory delivery count are lost with the process).
	manager.stopAndWaitBackgroundMutationsForShutdown()

	// Stage the surviving tmux session so the restart reattaches rather than
	// respawning an agent the runner lacks.
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", tmuxName).Run(),
		"staging the surviving tmux session")

	restarted, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.RestoreInstances())

	key := daemonInstanceKey(repo.ID, "nightly")
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
		t.Fatalf("the marked session must survive the restart (materialization error: %v)", merr)
	}

	// The durable marker was re-armed on restore and the intent re-parked.
	require.NotNil(t, restored.OwedOnComplete(),
		"the durable marker survived the failed discharge and was re-armed on restart")
	require.True(t, parked, "the intent was re-parked by armOwedTaskLifecyclesLocked (parkedID=%q)", parkedID)

	// The hooks the restarted worker adopts are done: the reap can proceed.
	adoptedHooks := make(chan struct{})
	close(adoptedHooks)
	if gw := restored.GitWorktreeForTest(); gw != nil {
		gw.SetHooksDoneForTest(adoptedHooks)
	}

	// The first unpaused drain re-arms the worker, which re-validates and reaps.
	// This reap is HONEST: the delivery above was refused, so no PTY write landed
	// and the durable state ("marker set, no churn") truthfully reports no
	// adoption. The bug closed by this fix is the work-losing window where the
	// PTY write landed anyway.
	restarted.applyDeferredTaskSessionLifecycle(repo.ID, restored)

	require.Eventually(t, func() bool {
		restarted.mu.Lock()
		defer restarted.mu.Unlock()
		_, present := restarted.instances[key]
		return !present
	}, 20*time.Second, 25*time.Millisecond,
		"the declared on_complete=kill reaps the session — honestly, because the refused delivery left no PTY write behind")

	// The kill's delete is the marker's discharge.
	raw, err = config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	rows = nil
	require.NoError(t, json.Unmarshal(raw, &rows))
	assert.Empty(t, rows, "a reaped session must not leave its durable record behind")
}

// TestTaskSessionLifecycle_PostMarkerDeliveryRaceSurvivesRestartWhenDischargePersists is the
// control: when the discharge persist lands, NoteAdoptionDelivery returns nil, the
// durable marker is cleared, and a restart finds nothing to re-arm — the session the
// user adopted survives. This is the happy path the bug used to share with the
// work-losing window (both returned nil); the fix makes the nil return conditional on
// the durable clear actually landing.
func TestTaskSessionLifecycle_PostMarkerDeliveryRaceSurvivesRestartWhenDischargePersists(t *testing.T) {
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
	const tmuxName = "af_4162_post_marker_race_adopt"
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedName(tmuxName, "claude"))
	inst.GitWorktreeForTest().SetHooksDoneForTest(make(chan struct{}))

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repo.ID, inst, was)
	require.NotNil(t, inst.OwedOnComplete(), "precondition: the durable marker is filed")

	// No injected failure: the discharge persist lands, as it does in the
	// overwhelming common case.
	require.NoError(t, inst.NoteAdoptionDelivery(),
		"the durable discharge landed — NoteAdoptionDelivery returns nil and the PTY write may proceed")
	require.Nil(t, inst.OwedOnComplete(),
		"the in-memory marker is cleared after the durable discharge")

	// The durable marker is cleared by the discharge.
	raw, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0], "pending_on_complete",
		"the discharge is durable — no marker survives")

	// The daemon goes down; the in-memory delivery count is lost but the
	// durable marker is gone too, so there is nothing to re-arm.
	manager.stopAndWaitBackgroundMutationsForShutdown()

	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", tmuxName).Run(),
		"staging the surviving tmux session")

	restarted, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.RestoreInstances())

	key := daemonInstanceKey(repo.ID, "nightly")
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

	// THE CONTROL: with the durable marker cleared by the discharge, restore
	// re-arms nothing and parks nothing. The drain finds no obligation and the
	// session the user adopted survives.
	require.Nil(t, restored.OwedOnComplete(),
		"the discharge persisted — restore must not re-arm a marker that was durably cleared")
	assert.False(t, parked, "no durable marker survived, so the restart must not re-park the deferred intent (parkedID=%q)", parkedID)

	restarted.applyDeferredTaskSessionLifecycle(repo.ID, restored)
	time.Sleep(300 * time.Millisecond)

	restarted.mu.Lock()
	_, present := restarted.instances[key]
	restarted.mu.Unlock()
	assert.True(t, present, "a session the user adopted (and whose discharge persisted) must survive the restart")
}

// TestTaskSessionLifecycle_PostMarkerDischargePersistFailureRefusesBrowserTerminalInput
// is the end-to-end test through the browser-PTY path the bug is specifically
// about: InputTab takes no manager lock, so the adoption fence and the discharge
// notify are the whole of its serialization. A failed discharge persist must refuse
// the keystroke with an error before any byte reaches the pane — the work-losing
// half of the bug.
func TestTaskSessionLifecycle_PostMarkerDischargePersistFailureRefusesBrowserTerminalInput(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	endRunOnIdleEdge(t, inst)

	// File the durable marker and install the discharge notify. Use the
	// production filing entry point so the notify is the real discharge path.
	manager.fileOwedTaskLifecycle(repoID, inst)
	require.NotNil(t, inst.OwedOnComplete(), "precondition: the durable marker is filed")

	// Inject a persist failure targeted at the discharge write ONLY.
	prev := testHookPersistInstanceData
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "nightly" && data.TaskID != "" && data.PendingOnComplete == nil {
			return errors.New("injected discharge persist failure (disk error)")
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = prev })

	// The browser terminal's keystroke reaches InputTab with no manager lock —
	// the bug's primary path. The discharge persist fails, so the keystroke must
	// be refused with the discharge error before any byte reaches the pane.
	ptyErr := browserTerminalInput(t, inst, "one more thing\r")
	require.Error(t, ptyErr, "a browser PTY keystroke whose discharge persist failed must be refused")
	assert.NotErrorIs(t, ptyErr, session.ErrAdoptionFenced,
		"the refusal is the discharge error, not the adoption fence — the fence is open, the durable clear is what failed")
	require.NotNil(t, inst.OwedOnComplete(),
		"the in-memory marker is restored so the user's re-attempted keystroke re-runs the durable clear")

	// The durable marker survives the failed discharge — the durable veto the
	// user's keystroke was supposed to clear is still set.
	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0], "pending_on_complete",
		"the discharge did not persist — the durable marker survives the refused keystroke")

	// Restore the hook and re-attempt the delivery: once the persist can land,
	// the discharge succeeds and the durable marker is cleared. The retry goes
	// through NoteAdoptionDelivery directly — the broker step that follows it in
	// InputTab needs a live tmux tab the fixture does not stage, and it is
	// unrelated to the discharge contract under test (the first keystroke above
	// already exercised the full InputTab → NoteAdoptionDelivery path).
	testHookPersistInstanceData = prev
	require.NoError(t, inst.NoteAdoptionDelivery(),
		"the re-attempted delivery must succeed once the discharge persist can land")
	require.Nil(t, inst.OwedOnComplete(),
		"the in-memory marker is cleared after the durable discharge")
	raw, err = config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	rows = nil
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0], "pending_on_complete",
		"the re-attempted discharge is durable — no marker survives")
}

// TestTaskSessionLifecycle_PostMarkerConcurrentDeliverySharesDischargeResult
// is the concurrent half of the post-marker race: the durable-clear PR closes
// the single-caller window (a delivery that returned nil before the discharge
// persist had landed) but leaves a second one when TWO callers hit a marked
// session at once. The first caller to take i.mu clears owedOnComplete in
// memory, installs the discharge future, releases i.mu, and starts the notify;
// a second caller arriving while that notify is still in flight finds no
// in-memory marker to take (the first already cleared it). Without a shared
// discharge it would proceed past nil owedOnComplete to its own PTY write, and
// the durable marker the first caller's persist just failed to clear would
// survive an unclean exit and re-arm a teardown that destroys the second
// caller's work — the same work-losing half the single-caller fix closes, on
// the second caller instead.
//
// The discharge-in-flight state on i.discharge lets the second caller share
// the first's durable verdict: it captures the future under i.mu, releases
// the lock, and parks on the future's done channel. On success both may
// write (the marker is durably gone, once, by the first); on failure both
// are refused so no PTY write lands on top of the surviving durable marker.
// This test gates the first caller's persist so the second caller enters
// while it is still in flight, then releases the gate with a failure and
// asserts both calls exit with the shared discharge error, no PTY write
// landed, and the durable marker survives — and that a re-attempt once the
// persist can land clears it durable-ly.
func TestTaskSessionLifecycle_PostMarkerConcurrentDeliverySharesDischargeResult(t *testing.T) {
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
	endRunOnIdleEdge(t, inst)

	// File the durable marker and install the discharge notify exactly as the
	// completion edge does, without launching the lifecycle worker.
	manager.fileOwedTaskLifecycle(repo.ID, inst)
	require.NotNil(t, inst.OwedOnComplete(), "precondition: the durable marker is filed")
	require.Equal(t, uint64(0), inst.AdoptionDeliveries(),
		"precondition: nothing has been delivered yet")

	// Gate the discharge persist so the first caller is parked inside the
	// notify while a second concurrent caller takes i.mu. By the time the
	// gate fires the discharger has already released i.mu and cleared
	// owedOnComplete in memory, so the second caller finds no marker to
	// clear — and, with the fix, parks on the in-flight discharge future rather
	// than proceeding to its own PTY write on top of the durable marker the
	// first is about to fail to clear.
	persistStarted := make(chan struct{})
	persistBlocked := make(chan error, 1)
	prev := testHookPersistInstanceData
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "nightly" && data.TaskID != "" && data.PendingOnComplete == nil {
			select {
			case <-persistStarted:
				return errors.New("injected discharge persist failure (disk error)")
			default:
				close(persistStarted)
				return <-persistBlocked
			}
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = prev })

	// First caller: takes i.mu, sees the marker, clears it in memory, installs
	// the discharge future, releases i.mu, and blocks inside the persist hook
	// on the gate above. Its notify is the only path through the hook on this
	// run — the second caller shares the future and never calls notify itself.
	firstErr := make(chan error, 1)
	go func() { firstErr <- inst.NoteAdoptionDelivery() }()
	<-persistStarted

	// Second caller: arrives while the first is still parked in the persist
	// hook. It takes i.mu, bumps the delivery count, finds nil owedOnComplete,
	// captures the in-flight discharge future, releases i.mu, and parks on
	// the future's done channel — instead of returning nil and writing its
	// bytes to the PTY on top of the durable marker the first is failing to
	// discharge.
	secondErr := make(chan error, 1)
	go func() { secondErr <- inst.NoteAdoptionDelivery() }()

	// Both callers must have bumped the count before either returns: the
	// first inside its i.mu section, the second inside its own — even though
	// the second parks on the future, the count bump happens under i.mu
	// before it parks. require.Eventually waits for the second to have made
	// it through i.mu and parked on the future; without the shared-future
	// fix the second caller would have returned nil and the durable marker
	// would have survived its PTY write.
	require.Eventually(t, func() bool {
		return inst.AdoptionDeliveries() == 2
	}, 5*time.Second, 25*time.Millisecond,
		"both concurrent deliveries must bump the count before either returns; the second parks on the in-flight discharge future rather than bypassing it")

	// Release the discharge gate with a persist failure. The first caller's
	// notify returns the error; the first caller restores the in-memory
	// marker, closes the discharge future with the same error, and returns
	// it. The second caller's <-wait.done unblocks; it sees the shared error
	// and returns it — its PTY write never happens.
	persistBlocked <- errors.New("injected discharge persist failure (disk error)")
	first := <-firstErr
	second := <-secondErr

	require.Error(t, first, "the discharger's failed persist refuses its delivery")
	require.Error(t, second,
		"a concurrent delivery must share the in-flight discharge error rather than bypass it to its own PTY write on top of the surviving durable marker")
	assert.Contains(t, second.Error(), "discharge for session",
		"the second caller's refusal is the shared discharge failure, not the adoption fence")

	// Both callers' delivery counts stay bumped even though neither PTY write
	// landed: the in-memory count is the stand-down signal a concurrent
	// teardown would read through the fence even before either re-attempts.
	require.Equal(t, uint64(2), inst.AdoptionDeliveries(),
		"both concurrent deliveries bumped the count even though neither PTY write landed")

	// The in-memory marker is restored by the discharging caller so a
	// re-attempt of either re-runs the durable clear instead of proceeding.
	require.NotNil(t, inst.OwedOnComplete(),
		"the in-memory marker is restored so a re-attempt re-runs the durable clear")

	// The durable marker survives the failed discharge.
	raw, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0], "pending_on_complete",
		"the discharge did not persist — the durable marker survives the failed concurrent discharge")

	// No settleOwed retry was recorded: the discharge path's only retry is the
	// caller's, shared here just as it is in the single-caller case.
	manager.mu.Lock()
	assert.Empty(t, manager.settleOwed, "the discharge path records no in-memory retry even when its result is shared")
	manager.mu.Unlock()

	// Once the persist can land, a re-attempt succeeds and clears the durable
	// marker.
	testHookPersistInstanceData = func(string, session.InstanceData) error { return nil }
	require.NoError(t, inst.NoteAdoptionDelivery(),
		"a re-attempted delivery must succeed once the discharge persist can land")
	require.Nil(t, inst.OwedOnComplete(),
		"the in-memory marker is cleared after the retry's durable discharge")
	require.Equal(t, uint64(3), inst.AdoptionDeliveries(),
		"the retry advances the count a third time")

	raw, err = config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	rows = nil
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0], "pending_on_complete",
		"the re-attempted discharge is durable — no marker survives")
}
