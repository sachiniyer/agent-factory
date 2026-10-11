package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// A frame captured BEFORE a redelivery's boundary must not reach the new
// window's chrome watcher (#5221 review): the helper reads the boundary at
// feed time, so a send landing between SnapshotAgent and the feed would
// otherwise arm a fresh watcher off a pane that never saw the new prompt —
// for scoped agents an indicator-absent capture primes the absent→present
// contract, after which unrelated pre-boundary chrome can pass as the
// redelivery's turn and release the held idle edge early.
func TestNoteTaskRunTurnEvidenceDropsPreBoundaryCapture(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-fence", Path: t.TempDir(), Program: "codex", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")

	boundary := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	_, epoch := inst.InFlightOpAndEpoch()

	m := &Manager{taskRunTurnWatches: map[string]*taskRunTurnWatchEntry{}}
	const key = "turn-fence"

	// The capture started before the attempt: whatever its content, it is not
	// this window's evidence and must not create or mutate a watcher.
	quiet := session.Observation{Content: "\x1b[2J\x1b[H› \r\n\r\n", Captured: true}
	require.False(t, m.noteTaskRunTurnEvidence(key, inst, quiet, boundary.Add(-time.Millisecond), epoch))
	_, watched := m.taskRunTurnWatches[key]
	require.False(t, watched, "a pre-boundary capture must not arm the new window's watcher")

	// A post-boundary capture feeds normally — the quiet frame arms the
	// scoped agent's absent→present contract.
	require.False(t, m.noteTaskRunTurnEvidence(key, inst, quiet, boundary.Add(time.Millisecond), epoch))
	_, watched = m.taskRunTurnWatches[key]
	require.True(t, watched, "a post-boundary capture feeds the watcher")
}

// An unanswered capture is no pane evidence at all — but a CONFIRMED empty
// frame is: the scoped agents arm on indicator-absent, so a clean capture of a
// blank pane primes the contract a later chrome frame can satisfy (#5221
// review).
func TestNoteTaskRunTurnEvidenceDistinguishesConfirmedEmpty(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-empty", Path: t.TempDir(), Program: "codex", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")

	boundary := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	_, epoch := inst.InFlightOpAndEpoch()

	m := &Manager{taskRunTurnWatches: map[string]*taskRunTurnWatchEntry{}}
	const key = "turn-empty"
	at := boundary.Add(time.Millisecond)

	// A failed capture — Captured false — feeds nothing.
	require.False(t, m.noteTaskRunTurnEvidence(key, inst, session.Observation{Captured: false}, at, epoch))
	_, watched := m.taskRunTurnWatches[key]
	require.False(t, watched, "an unanswered capture must not arm the watcher")

	// A confirmed capture of a genuinely empty pane arms the scoped watcher's
	// absent→present contract — the absent half of the turn proof.
	require.False(t, m.noteTaskRunTurnEvidence(key, inst, session.Observation{Captured: true}, at, epoch))
	_, watched = m.taskRunTurnWatches[key]
	require.True(t, watched, "a confirmed empty capture arms the scoped watcher")
}

// The streak's expiry reserve must follow the serial sweep, not just the
// configured tick: a fleet pass longer than its tick still captures every
// session on schedule, and only a gap beyond that honest spacing may rebase
// the observation streak (#5221 review). The reserve also carries one
// capped sweep outright — position drift can move a session from the start
// of one pass to the end of the next, so the declared expiry anticipates it
// at write time rather than re-judging at read time. Sweep terms stay
// CAPPED: a pass slower than the cap is a stall that left panes genuinely
// unwatched, not a schedule to normalize — blindness that long must rebase
// the streak.
func TestStatusSweepExpectedIntervalFloorsAtSweepDuration(t *testing.T) {
	old := lastStatusSweepNs.Load()
	oldStart := statusSweepStartNs.Load()
	t.Cleanup(func() { lastStatusSweepNs.Store(old); statusSweepStartNs.Store(oldStart) })
	statusSweepStartNs.Store(0)

	lastStatusSweepNs.Store(0)
	require.Equal(t, time.Second, statusSweepExpectedInterval(time.Second),
		"no sweep history: the scheduled interval alone applies")

	lastStatusSweepNs.Store(int64(500 * time.Millisecond))
	require.Equal(t, time.Second+500*time.Millisecond, statusSweepExpectedInterval(time.Second),
		"a fast sweep still reserves its own duration for next-pass drift")

	lastStatusSweepNs.Store(int64(20 * time.Second))
	require.Equal(t, 40*time.Second, statusSweepExpectedInterval(time.Second),
		"an honest overrun reserves lastSweep for pacing plus lastSweep for drift")

	lastStatusSweepNs.Store(int64(10 * time.Minute))
	require.Equal(t, 2*statusSweepFloorCap, statusSweepExpectedInterval(time.Second),
		"a stalled pass is not normalized as cadence: only its capped terms count")
	require.Equal(t, taskRunPollBackstop+statusSweepFloorCap, statusSweepExpectedInterval(taskRunPollBackstop),
		"the backstop cadence paces the reserve; the stall adds only the drift term")

	// An operator-set interval slower than the cap is the pacing term, so
	// position drift ADDS to it rather than hiding inside it (#5221 review).
	lastStatusSweepNs.Store(int64(3 * time.Minute))
	require.Equal(t, 2*time.Minute+statusSweepFloorCap, statusSweepExpectedInterval(2*time.Minute),
		"a configured cadence above the cap is preserved, not compressed")
}

// One pass's duration plus one position's worth of drift is the honest
// spacing until the next capture of the same session: map iteration can
// move a session from the start of one sweep to the end of the next
// (#5221 review).
func TestStatusSweepExpectedIntervalCoversPositionDrift(t *testing.T) {
	old := lastStatusSweepNs.Load()
	oldStart := statusSweepStartNs.Load()
	oldNow := nowFunc
	t.Cleanup(func() {
		lastStatusSweepNs.Store(old)
		statusSweepStartNs.Store(oldStart)
		nowFunc = oldNow
	})

	now := time.Now()
	nowFunc = func() time.Time { return now }

	// A 20s completed sweep, this capture landing 15s into the next pass:
	// pacing is 20s, position 15s, next-pass drift reserved 20s — 55s.
	lastStatusSweepNs.Store(int64(20 * time.Second))
	statusSweepStartNs.Store(now.Add(-15 * time.Second).UnixNano())
	require.Equal(t, 55*time.Second, statusSweepExpectedInterval(time.Second),
		"the reserve covers this pass's pacing, position, and next-pass drift")

	// A session near the pass's end gets nearly two capped sweeps of reserve,
	// bounded by scheduled + two caps.
	now = now.Add(19 * time.Second)
	require.Equal(t, time.Second+2*statusSweepFloorCap, statusSweepExpectedInterval(time.Second),
		"elapsed and drift terms each stay bounded, and the sum is bounded too")

	// Each term stays capped: two stalled passes contribute at most the cap
	// apiece, never normalizing their own blindness.
	lastStatusSweepNs.Store(int64(10 * time.Minute))
	now = now.Add(10 * time.Minute)
	require.Equal(t, time.Second+2*statusSweepFloorCap, statusSweepExpectedInterval(time.Second),
		"stalled passes cannot stretch the reserve past scheduled + two capped terms")
}

// flakyCaptureBackend answers HasUpdatedObserved with a FAILED capture —
// the (false,false,"",false) shape a local capture-pane error produces inside
// SnapshotAgent — until flipped to confirmed (#5221 review).
type flakyCaptureBackend struct {
	*session.FakeBackend
	captured bool
}

func (b *flakyCaptureBackend) HasUpdatedObserved(*session.Instance) (bool, bool, string, bool, bool) {
	return false, false, "", false, b.captured
}

// promptWaitBackend answers HasUpdatedObserved with a confirmed-unchanged
// frame — optionally carrying a confirmation-dialog prompt flag, the shape a
// turn parked at a y/n dialog produces (#5221 review).
type promptWaitBackend struct {
	*session.FakeBackend
	captured bool
	prompt   bool
}

func (b *promptWaitBackend) HasUpdatedObserved(*session.Instance) (bool, bool, string, bool, bool) {
	return false, b.prompt, "confirm? [y/n]", false, b.captured
}

// A confirmation dialog is an active waiting state, not pane stillness
// (#5221 review): an unchanged frame carrying HasPrompt must withhold the
// still stamp AND retire an open candidate, or a turn parked at the dialog
// latches the edge and its resumed output passes as the new prompt's churn.
func TestPromptWaitRetiresTaskRunStillCandidate(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &promptWaitBackend{FakeBackend: session.NewFakeBackend(), captured: true}
	inst := registerTaskRun(t, manager, repoID, repoPath, "wait-still")
	inst.SetBackend(backend)
	key := stableSessionKey(repoID, inst)

	now := time.Now()
	oldNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = oldNow })

	// A contended boundary owes the still base rather than seeding it.
	boundary := now.Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(now.Add(-30*time.Second), epoch))
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, now.Add(-15*time.Second)))
	require.True(t, inst.TaskRunStillBaseAt().IsZero(), "precondition: a contended bind owes the stillness edge")

	manager.observeTaskRunWhilePaused(repoID, key, inst)
	now = now.Add(2 * time.Second)
	manager.observeTaskRunWhilePaused(repoID, key, inst)

	// The dialog appears — the waiting frame must retire the candidate.
	backend.prompt = true
	now = now.Add(2 * time.Second)
	manager.observeTaskRunWhilePaused(repoID, key, inst)

	// Stillness resumes past where the interrupted run would have latched:
	// the 5s grace re-earns from the first prompt-free frame, so the base
	// must still be owed one full uninterrupted run.
	backend.prompt = false
	now = now.Add(6 * time.Second)
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	require.True(t, inst.TaskRunStillBaseAt().IsZero(),
		"a dialog-interrupted still run reopened rather than latching")
	now = now.Add(6 * time.Second)
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	require.False(t, inst.TaskRunStillBaseAt().IsZero(),
		"an uninterrupted run past the grace latches the edge")
}

// workingWaitBackend answers HasUpdatedObserved with a confirmed-unchanged
// frame showing amp's static mid-turn indicator — the shape a turn parked on
// an unbounded tool call produces (#5221 review).
type workingWaitBackend struct {
	*session.FakeBackend
	working bool
}

func (b *workingWaitBackend) HasUpdatedObserved(*session.Instance) (bool, bool, string, bool, bool) {
	content := ampPaneIdle
	if b.working {
		content = ampPaneThinking
	}
	return false, false, content, false, true
}

// A visible working indicator is affirmative mid-turn evidence, not
// stillness (#5221 review): an unchanged capture carrying one must withhold
// the still stamp AND retire an open candidate, or a boundary bound over an
// in-flight amp turn latches the still base ON that turn's own frame — and
// noteTaskRunTurnEvidence then primes the redelivery's watcher with proof
// the older prompt already produced.
func TestWorkingFrameRetiresTaskRunStillCandidate(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &workingWaitBackend{FakeBackend: session.NewFakeBackend(), working: true}
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "work-still", Path: repoPath, Program: "amp", TaskID: "task-work-still",
	})
	require.NoError(t, err)
	inst.SetBackend(backend)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repoID, "work-still", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "work-still")] = inst
	manager.mu.Unlock()
	key := stableSessionKey(repoID, inst)

	now := time.Now()
	oldNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = oldNow })

	// A redelivery binds a contended boundary that owes the still edge.
	boundary := now.Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(now.Add(-30*time.Second), epoch))
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, now.Add(-15*time.Second)))
	require.True(t, inst.TaskRunStillBaseAt().IsZero(), "precondition: a rebind owes the stillness edge")

	// The old turn's static working frame holds unchanged for the whole
	// stillness grace and more — it must never open the candidate.
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	now = now.Add(6 * time.Second)
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	require.True(t, inst.TaskRunStillBaseAt().IsZero(),
		"a working frame held past the grace must not latch the still edge")

	// The turn ends: the bare frame stamps stillness, but the edge re-earns
	// its own grace — it cannot borrow the working frames' elapsed time.
	backend.working = false
	now = now.Add(6 * time.Second)
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	require.True(t, inst.TaskRunStillBaseAt().IsZero(),
		"the first idle frame after working ones re-opens, not latches")
	now = now.Add(6 * time.Second)
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	require.False(t, inst.TaskRunStillBaseAt().IsZero(),
		"an uninterrupted prompt-free working-free run latches the edge")
}

// The paused backstop probe must drop an unanswered capture whole: settling
// the all-false frame would read the pane idle on no evidence, letting the
// last confirmed capture's still-live streak release the quiet arm mid-outage
// (#5221 review).
func TestObserveTaskRunWhilePausedDropsFailedCapture(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &flakyCaptureBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "paused-drop", backend, true, session.Running)
	key := stableSessionKey(repoID, inst)

	manager.observeTaskRunWhilePaused(repoID, key, inst)
	require.Equal(t, session.LiveRunning, inst.GetLiveness(),
		"a failed capture is no observation: nothing may settle off it")

	backend.captured = true
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	require.Equal(t, session.LiveReady, inst.GetLiveness(),
		"a confirmed unchanged frame settles Ready — the failed one was the gate")
}

// The normal poll keeps the same rule: Alive() may prove the session lives,
// but a failed pane capture saw nothing of the pane, so the idle branch must
// not settle Ready off it (#5221 review). Lost detection is untouched — the
// probe still runs, it just cannot complete the idle path.
func TestRefreshInstanceStatusKeepsLivenessOnFailedCapture(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &flakyCaptureBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "poll-drop", backend, true, session.Running)

	manager.refreshInstanceStatus(repoID, inst)
	require.Equal(t, session.LiveRunning, inst.GetLiveness(),
		"an unanswered capture must not settle the idle branch")

	backend.captured = true
	manager.refreshInstanceStatus(repoID, inst)
	require.Equal(t, session.LiveReady, inst.GetLiveness(),
		"a confirmed unchanged frame settles Ready")
}

// A blind window must invalidate the run's chrome watcher (#5221 review):
// the scoped agents' absence grace and the timed agents' prev-row pairing
// both assume CONTINUOUS sight, so a capture failure that leaves them
// intact lets the outage stand in for evidence — a repaint gap bracketing
// the hole would arm the watch on frames it never actually saw.
func TestFailedCaptureDropsTaskRunTurnWatch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &flakyCaptureBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "watch-drop", backend, true, session.Running)
	key := stableSessionKey(repoID, inst)

	boundary := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	seed := func() {
		manager.taskRunTurnWatches[key] = &taskRunTurnWatchEntry{
			agent: inst.ResolvedAgent(), boundary: boundary,
			watch: task.NewTurnWatch(inst.ResolvedAgent(), boundary),
		}
	}

	// The paused backstop's unanswered-capture path.
	seed()
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	_, ok := manager.taskRunTurnWatches[key]
	require.False(t, ok, "a failed capture must invalidate the paused path's watcher")

	// The normal poll's unanswered-capture path.
	seed()
	manager.refreshInstanceStatus(repoID, inst)
	_, ok = manager.taskRunTurnWatches[key]
	require.False(t, ok, "a failed capture must invalidate the normal path's watcher")
}

// A successful capture that re-anchors the observation streak invalidates
// the watcher the same way a failed capture does (#5221 review): the frames
// on either side of the hole never saw each other, so absence time accrued
// across it — or a prev-row pairing straddling it — is not evidence.
func TestStreakReanchorDropsTaskRunTurnWatch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &flakyCaptureBackend{FakeBackend: session.NewFakeBackend(), captured: true}
	inst := registerStarted(t, manager, repoID, repoPath, "watch-gap", backend, true, session.Running)
	key := stableSessionKey(repoID, inst)

	now := time.Now()
	oldNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = oldNow })

	// The first probe anchors the streak; an on-schedule capture keeps any
	// watcher seeded under it.
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	boundary := now
	manager.taskRunTurnWatches[key] = &taskRunTurnWatchEntry{
		agent: inst.ResolvedAgent(), boundary: boundary,
		watch: task.NewTurnWatch(inst.ResolvedAgent(), boundary),
	}
	now = now.Add(taskRunPollBackstop)
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	_, ok := manager.taskRunTurnWatches[key]
	require.True(t, ok, "an on-schedule capture keeps the watcher")

	// A stall past the gap horizon rebases the streak on the next success —
	// the watcher's paired state dies with the streak it straddled.
	now = now.Add(4 * taskRunPollBackstop)
	manager.observeTaskRunWhilePaused(repoID, key, inst)
	_, ok = manager.taskRunTurnWatches[key]
	require.False(t, ok, "a re-anchored streak must invalidate the watcher")
}

// A boundary that bound over proven stillness — a fresh task's first bind
// seeds the still base at the send behind WaitForReady — already observed
// the absent half of the scoped contract: the watcher the feed builds for
// it is primed, so a turn that began before the first daemon capture still
// counts on its first chrome frame rather than being read as indicator
// noise the absent→present contract never armed (#5221 review).
func TestNoteTaskRunTurnEvidencePrimesFromSeededStillBase(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-primed", Path: t.TempDir(), Program: "codex", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")

	boundary := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	require.False(t, inst.TaskRunStillBaseAt().IsZero(),
		"a first bind seeds the still base — the send proved the pane still")
	_, epoch := inst.InFlightOpAndEpoch()

	m := &Manager{taskRunTurnWatches: map[string]*taskRunTurnWatchEntry{}}
	const key = "turn-primed"

	// The very first daemon frame already shows the indicator — the turn
	// began in the blind window before the first capture. Primed by the
	// seeded base, the watch counts it; unprimed it would discard the chrome.
	require.True(t, m.noteTaskRunTurnEvidence(key, inst,
		session.Observation{
			Content:  "\x1b[2J\x1b[H› \r\n\r\n  esc to interrupt\r\n",
			Captured: true,
		}, boundary.Add(time.Millisecond), epoch),
		"proven-absence boundary: the first present frame is the turn")
}

// A run that ends on a Ready→Ready observation changes no liveness, and
// on_complete=keep writes nothing either — yet the marker flip is durable
// state a restart reads. Without a forced checkpoint the on-disk row keeps
// TaskRunActive=true and a daemon restart resurrects the finished run into
// its held concurrency slot until the fallback matures again (#5221 review).
func TestRefreshInstanceStatusPersistsReadyToReadyRunEnd(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "held-run", "task-keep")
	stubTaskLifecycle(t, "task-keep", task.OnCompleteKeep)

	// The row already sits Ready before the polls — the hole is the unchanged
	// frame, so no liveness edge may carry the write.
	inst.SetStatusForTest(session.Ready)
	// Seed the durable precondition this bug strand leaves behind: the on-disk
	// row still carries the in-flight marker from the run's own writes.
	seeded, err := json.Marshal([]session.InstanceData{
		{Title: "held-run", Path: repoPath, Status: session.Ready, TaskRunActive: true},
	})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))

	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, time.Now()))
	manager.refreshInstanceStatus(repoID, inst)
	require.True(t, inst.TaskRunActive(),
		"the first idle edge inside the turn gate holds — the agent has not demonstrably turned")

	require.True(t, inst.RecordTaskRunTurn(time.Now()))
	manager.refreshInstanceStatus(repoID, inst)
	require.False(t, inst.TaskRunActive(), "post-turn Ready→Ready releases the held edge")

	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var rows []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	require.False(t, rows[0].TaskRunActive,
		"the marker flip must reach disk even though the poll moved no liveness")
}
