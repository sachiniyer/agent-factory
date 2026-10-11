package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
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
// the observation streak (#5221 review).
func TestStatusSweepExpectedIntervalFloorsAtSweepDuration(t *testing.T) {
	old := lastStatusSweepNs.Load()
	t.Cleanup(func() { lastStatusSweepNs.Store(old) })

	lastStatusSweepNs.Store(0)
	require.Equal(t, time.Second, statusSweepExpectedInterval(time.Second),
		"no sweep history: the scheduled interval alone applies")

	lastStatusSweepNs.Store(int64(500 * time.Millisecond))
	require.Equal(t, time.Second, statusSweepExpectedInterval(time.Second),
		"a sweep faster than the tick does not shorten the expected cadence")

	lastStatusSweepNs.Store(int64(90 * time.Second))
	require.Equal(t, 90*time.Second, statusSweepExpectedInterval(time.Second),
		"a sweep slower than the tick becomes the expected cadence")
	require.Equal(t, 90*time.Second, statusSweepExpectedInterval(taskRunPollBackstop),
		"the paused backstop cadence floors at the sweep too")
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
