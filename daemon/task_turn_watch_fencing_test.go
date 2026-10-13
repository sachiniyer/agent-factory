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

// A boundary set behind WaitForReady carries proven-idle proof, so the daemon
// primes the watcher (PrimeProvenIdle) and a turn starting sub-second after the
// boundary is recognized on its first ticking capture — instead of being
// permanently rejected by the elapsed fit, whose whole-second display is
// forever one second short of the boundary's age in the affected capture phase
// (#5221 review). This is the daemon-side wiring of the fix: the readiness
// path marks the boundary proven-idle, and noteTaskRunTurnEvidence primes from
// it lazily each poll.
func TestNoteTaskRunTurnEvidencePrimesProvenIdleBoundary(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-prime", Path: t.TempDir(), Program: "claude", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")

	boundary := time.Now()
	// RecordPromptAttempt establishes the boundary and CLEARS the proven-idle
	// flag (a new boundary is unproven until the readiness path re-marks it).
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, boundary))
	require.False(t, inst.TaskRunPromptProvenIdle(),
		"a fresh prompt attempt is unproven until the readiness path marks it")
	// The readiness-proven send path re-marks the boundary proven-idle.
	inst.MarkTaskRunPromptProvenIdle()
	require.True(t, inst.TaskRunPromptProvenIdle(), "the readiness path marks the boundary proven-idle")

	_, epoch := inst.InFlightOpAndEpoch()
	m := &Manager{taskRunTurnWatches: map[string]*taskRunTurnWatchEntry{}}
	const key = "turn-prime"
	// A turn starting ~200ms after the boundary: at boundary+1.2s the row
	// shows 1s, at boundary+2.2s it shows 2s (ticking). The fit 2+1=3 <= 2.2
	// fails, so without priming this would never fire.
	row := func(elapsed string) string {
		return "transcript\n✻ Whirring… (" + elapsed + " · esc to interrupt)\n❯ "
	}
	// First capture establishes the row; it cannot already be a tick.
	require.False(t, m.noteTaskRunTurnEvidence(key, inst,
		session.Observation{Content: row("1s"), Captured: true}, boundary.Add(1200*time.Millisecond), epoch),
		"the first capture establishes the row; it cannot already be a tick")
	_, watched := m.taskRunTurnWatches[key]
	require.True(t, watched, "a post-boundary capture arms the watcher")
	// Second capture: the row ticked. Primed, it fires despite the fit
	// being unsatisfiable for this sub-second turn start.
	require.True(t, m.noteTaskRunTurnEvidence(key, inst,
		session.Observation{Content: row("2s"), Captured: true}, boundary.Add(2200*time.Millisecond), epoch),
		"a ticking row after a proven-idle boundary fires — the fit stands down")
}

// A boundary NOT proven idle (a direct send that bypassed WaitForReady) keeps
// the elapsed fit, so a sub-second-post-boundary turn still cannot fire — the
// protective fit that blocks pre-boundary turns stays in place for sends whose
// pane may have been mid-turn. This is the conservative path the direct
// daemon delivery paths (manual RPC, limit resume without readiness) take.
func TestNoteTaskRunTurnEvidenceUnprovenBoundaryKeepsElapsedFit(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-unproven", Path: t.TempDir(), Program: "claude", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")

	boundary := time.Now()
	// A direct send (no readiness): RecordPromptAttempt alone, no
	// MarkTaskRunPromptProvenIdle. The boundary is unproven.
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, boundary))
	require.False(t, inst.TaskRunPromptProvenIdle(), "a direct send leaves the boundary unproven")

	_, epoch := inst.InFlightOpAndEpoch()
	m := &Manager{taskRunTurnWatches: map[string]*taskRunTurnWatchEntry{}}
	const key = "turn-unproven"
	row := func(elapsed string) string {
		return "transcript\n✻ Whirring… (" + elapsed + " · esc to interrupt)\n❯ "
	}
	// Establish the row, then tick it on the next capture at a sub-second
	// post-boundary offset the fit cannot satisfy.
	m.noteTaskRunTurnEvidence(key, inst,
		session.Observation{Content: row("1s"), Captured: true}, boundary.Add(1200*time.Millisecond), epoch)
	require.False(t, m.noteTaskRunTurnEvidence(key, inst,
		session.Observation{Content: row("2s"), Captured: true}, boundary.Add(2200*time.Millisecond), epoch),
		"an unproven boundary keeps the fit: a sub-second-post-boundary turn cannot fire without proof")
	// Once the turn is old enough that the fit IS satisfiable (elapsed+1 <=
	// age), the unproven watcher still fires — the fit blocks only the
	// sub-second-start regime, not turns ≥ 1s old at capture.
	m.noteTaskRunTurnEvidence(key, inst,
		session.Observation{Content: row("2s"), Captured: true}, boundary.Add(3500*time.Millisecond), epoch)
	require.True(t, m.noteTaskRunTurnEvidence(key, inst,
		session.Observation{Content: row("3s"), Captured: true}, boundary.Add(4500*time.Millisecond), epoch),
		"an unproven boundary still fires once the fit is satisfiable — the fit is the proof for unproven boundaries")
}

// A redelivery clears the proven-idle proof: RecordPromptAttempt wipes the
// flag for every new boundary, so a watcher built for the redelivery does not
// inherit the prior boundary's readiness proof — the new boundary must earn
// its own priming (or keep the fit). This keeps a direct redelivery from
// laundering an in-flight turn through a stale readiness proof.
func TestNoteTaskRunTurnEvidenceRedeliveryClearsProvenIdle(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-redeliver", Path: t.TempDir(), Program: "claude", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.TaskRunActive())

	first := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, first))
	inst.MarkTaskRunPromptProvenIdle()
	require.True(t, inst.TaskRunPromptProvenIdle(), "the first boundary is proven-idle")

	// A redelivery records a fresh boundary and clears the proof.
	second := first.Add(time.Second)
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, second))
	require.False(t, inst.TaskRunPromptProvenIdle(),
		"a redelivery clears the proven-idle proof; the new boundary is unproven")
}
