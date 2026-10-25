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

// The streak's expected cadence must follow the serial sweep, not just the
// configured tick: a fleet pass longer than its tick still captures every
// session on schedule, and only a gap beyond that honest spacing may rebase
// the observation streak (#5221 review). The floor is CAPPED, though: a pass
// slower than the cap is a stall that left panes genuinely unwatched, not a
// schedule to normalize — blindness that long must rebase the streak.
func TestStatusSweepExpectedIntervalFloorsAtSweepDuration(t *testing.T) {
	old := lastStatusSweepNs.Load()
	t.Cleanup(func() { lastStatusSweepNs.Store(old) })

	lastStatusSweepNs.Store(0)
	require.Equal(t, time.Second, statusSweepExpectedInterval(time.Second),
		"no sweep history: the scheduled interval alone applies")

	lastStatusSweepNs.Store(int64(500 * time.Millisecond))
	require.Equal(t, time.Second, statusSweepExpectedInterval(time.Second),
		"a sweep faster than the tick does not shorten the expected cadence")

	lastStatusSweepNs.Store(int64(20 * time.Second))
	require.Equal(t, 20*time.Second, statusSweepExpectedInterval(time.Second),
		"an honest overrun within the cap becomes the expected cadence")

	lastStatusSweepNs.Store(int64(10 * time.Minute))
	require.Equal(t, statusSweepFloorCap, statusSweepExpectedInterval(time.Second),
		"a stalled pass is not normalized as cadence: its blindness must rebase streaks")
	require.Equal(t, taskRunPollBackstop, statusSweepExpectedInterval(taskRunPollBackstop),
		"the paused backstop cadence is unaffected by a stalled pass")

	// An operator-set interval slower than the cap is itself the declared
	// cadence: a sweep overrunning it stays within the operator's own bound,
	// so the expected interval must not be compressed to the cap.
	lastStatusSweepNs.Store(int64(3 * time.Minute))
	require.Equal(t, 2*time.Minute, statusSweepExpectedInterval(2*time.Minute),
		"a configured cadence above the cap is preserved, not compressed")
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

// The paused backstop probe must drop an unanswered capture whole: settling
// the all-false frame would read the pane idle on no evidence, letting the
// last confirmed capture's still-live streak release the quiet arm mid-outage
// (#5221 review).
func TestObserveTaskRunWhilePausedDropsFailedCapture(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &flakyCaptureBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "paused-drop", backend, true, session.Running)
	key := daemonInstanceKey(repoID, inst.Title)

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
