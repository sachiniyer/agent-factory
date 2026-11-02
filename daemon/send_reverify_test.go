package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// #6590: a watch delivery recorded sent-unverified because the remedy Enter was
// withheld is re-verified by the daemon's pane feed — the same #6474 turn
// evidence, kept bound to the send's attempt instant past the bounded Confirm
// window. A turn the agent demonstrably began after the send promotes the
// recorded verdict to sent; anything else leaves it unverified.

// reverifyTurnRow is claude's in-turn status row: interrupt hint and elapsed
// timer on one row (task/submitted_turn.go's timed vocabulary).
func reverifyTurnRow(elapsed string) string {
	return "✻ Working… (" + elapsed + " · esc to interrupt)\n❯ \n"
}

// unverifiedSendBackend stages the #6590 steady state: every preview shows an
// already-running turn (so the bounded Confirm is ineligible and settles
// without polling) and the send itself reports the withheld remedy.
type unverifiedSendBackend struct {
	*session.FakeBackend
}

func (b *unverifiedSendBackend) Preview(*session.Instance) (string, error) {
	return reverifyTurnRow("9m 0s"), nil
}

func (b *unverifiedSendBackend) SendPromptCommandWithStatus(*session.Instance, string) (session.PromptDeliveryStatus, error) {
	return session.PromptSentUnverified, nil
}

// The end-to-end property: a task delivery the pane could not prove at the
// Enter records sent-unverified, and the daemon's poll feed — not any retry —
// is what promotes it once the agent demonstrably begins the send's turn.
func TestTaskSendArmsReverifyOnUnverified(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root",
		&unverifiedSendBackend{FakeBackend: session.NewFakeBackend()}, true, session.Running)
	seedUnverifiedTask(t, "feed6595")

	status, err := m.sendPromptGuarded(SendPromptRequest{
		Title: "root", RepoID: repoID, Prompt: "queued event", TaskOrigin: true, TaskID: "feed6595",
	}, nil)
	require.NoError(t, err)
	require.Equal(t, session.PromptSentUnverified, status)

	// The watcher records the verdict the way taskrun maps it.
	now := time.Now()
	_, uerr := task.UpdateTaskStatus("feed6595", &now, TaskStatusSentUnverified)
	require.NoError(t, uerr)

	key := stableSessionKey(repoID, inst)
	m.pausedMu.Lock()
	armed := m.unverifiedSendWatches[key]["feed6595"]
	m.pausedMu.Unlock()
	require.NotNil(t, armed, "an unverified task send must arm its re-verification watch")
	require.True(t, m.unverifiedSendWatchPending(key),
		"a pending watch must make the paused backstop observe the attached pane")
	attemptedAt := armed.attemptedAt

	// The send's own turn keeps ticking — never evidence — then ends, and the
	// queued draft's turn begins and ticks: the task's verdict promotes.
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyTurnRow("9m 1s"), Captured: true}, attemptedAt.Add(time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: "❯ \n", Captured: true}, attemptedAt.Add(2*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyTurnRow("1s"), Captured: true}, attemptedAt.Add(3*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyTurnRow("2s"), Captured: true}, attemptedAt.Add(4*time.Second))
	stored, err := task.GetTask("feed6595")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"a watch delivery the agent demonstrably took must end recorded delivered")
}

// A manual send-prompt to the same pane arms nothing — it owns no task row
// whose verdict could be promoted.
func TestManualSendArmsNoReverify(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, m, repoID, repoPath, "root",
		&unverifiedSendBackend{FakeBackend: session.NewFakeBackend()}, true, session.Running)

	status, err := m.sendPromptGuarded(SendPromptRequest{
		Title: "root", RepoID: repoID, Prompt: "hello",
	}, nil)
	require.NoError(t, err)
	require.Equal(t, session.PromptSentUnverified, status)

	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	require.Empty(t, m.unverifiedSendWatches, "a manual send-prompt must not arm task re-verification")
}

func seedUnverifiedTask(t *testing.T, taskID string) {
	t.Helper()
	require.NoError(t, task.AddTask(task.Task{
		ID: taskID, Name: "captain-events", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: t.TempDir(), Enabled: true,
	}))
	now := time.Now()
	_, err := task.UpdateTaskStatus(taskID, &now, TaskStatusSentUnverified)
	require.NoError(t, err)
}

// The queued-draft case: the agent was mid-turn when the send landed, its own
// clock keeps the running turn from ever counting, and the NEW turn beginning
// after it — the prompt's queued draft finally taken — fires the promotion.
func TestUnverifiedSendPromotesOnPostSendTurn(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnverifiedTask(t, "feed6590")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6590", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	// The turn running at send time keeps ticking; its clock outdates the send.
	captured(reverifyTurnRow("9m 1s"), attemptedAt.Add(time.Second))
	captured(reverifyTurnRow("9m 2s"), attemptedAt.Add(2*time.Second))
	stored, err := task.GetTask("feed6590")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnverified, stored.LastRunStatus,
		"a turn older than the send must not promote its verdict")

	// That turn ends and a new one begins — the queued prompt's work.
	captured("❯ \n", attemptedAt.Add(3*time.Second))
	captured(reverifyTurnRow("1s"), attemptedAt.Add(4*time.Second))
	captured(reverifyTurnRow("2s"), attemptedAt.Add(5*time.Second))
	stored, err = task.GetTask("feed6590")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"a turn beginning after the send must promote sent-unverified to sent")
}

// The never-taken case: no post-send turn evidence arrives, so the recorded
// verdict is never promoted — and the pending watch expires on its window
// rather than promoting on stale, weakly-correlated chrome.
func TestUnverifiedSendNeverTakenNeverPromotes(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnverifiedTask(t, "feed6591")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6591", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	// Idle composer, then a draft that stayed staged: no turn ever begins.
	for i := 0; i < 5; i++ {
		m.noteUnverifiedSendEvidence(key, inst,
			session.Observation{Content: "❯ staged draft text\n", Captured: true},
			attemptedAt.Add(time.Duration(i+1)*time.Second))
	}
	stored, err := task.GetTask("feed6591")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnverified, stored.LastRunStatus)

	// Past the re-verification window the watch retires: a turn beginning this
	// late can no longer be attributed to the send. Arm at an instant already
	// outside the window rather than racing the daemon clock.
	expiredAttempt := attemptedAt.Add(-2 * sendReverifyWindow)
	m.armUnverifiedSendWatch(key, inst, "feed6591", expiredAttempt, task.NewTurnWatch("claude", expiredAttempt))
	m.noteUnverifiedSendEvidence(key, inst,
		session.Observation{Content: "❯ \n", Captured: true}, time.Now())
	m.pausedMu.Lock()
	_, still := m.unverifiedSendWatches[key]
	m.pausedMu.Unlock()
	require.False(t, still, "an expired re-verification watch must be retired, not fed")
}

// A newer outcome already on the task record is never clobbered by a stale
// promotion: the compare-and-set is what keeps a late evidence fire from
// rewriting a verdict it does not own.
func TestUnverifiedSendPromotionNeverClobbersNewerOutcome(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnverifiedTask(t, "feed6592")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6592", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	// A newer delivery already rewrote the verdict before the fire lands.
	now := time.Now()
	_, err := task.UpdateTaskStatus("feed6592", &now, "errored: target gone")
	require.NoError(t, err)

	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: "❯ \n", Captured: true}, attemptedAt.Add(time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyTurnRow("1s"), Captured: true}, attemptedAt.Add(2*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyTurnRow("2s"), Captured: true}, attemptedAt.Add(3*time.Second))
	stored, err := task.GetTask("feed6592")
	require.NoError(t, err)
	require.Equal(t, "errored: target gone", stored.LastRunStatus,
		"a stale promotion must never overwrite a newer recorded outcome")
}

// The task was retargeted: this session's watch fires, but the verdict on the
// row was written by a DIFFERENT session's later delivery — outside the
// verdict-write slack. It is still unverified, and the stale fire must refuse
// it rather than claim a delivery it cannot see.
func TestUnverifiedSendNeverPromotesAnotherDelivery(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnverifiedTask(t, "feed6596")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6596", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	// A different session's delivery rewrote the verdict well after this
	// send's verdict-write slack.
	newer := attemptedAt.Add(2 * unverifiedVerdictWriteSlack)
	_, err := task.UpdateTaskStatus("feed6596", &newer, TaskStatusSentUnverified)
	require.NoError(t, err)

	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: "❯ \n", Captured: true}, attemptedAt.Add(time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyTurnRow("1s"), Captured: true}, attemptedAt.Add(2*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyTurnRow("2s"), Captured: true}, attemptedAt.Add(3*time.Second))
	stored, err := task.GetTask("feed6596")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnverified, stored.LastRunStatus,
		"a fire must never promote a verdict stamped outside its send's write window")
}

// The arm lands before the caller's verdict write: evidence that fires in
// that gap banks, not dies — the promotion retries once the send's own verdict
// is on the record (#6592 review).
func TestUnverifiedSendFiredEvidenceSurvivesLateVerdictWrite(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6597", Name: "captain-events", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: t.TempDir(), Enabled: true,
	}))

	attemptedAt := time.Now()
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6597", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	// The send's turn fires BEFORE the verdict is recorded — the CAS must
	// refuse, and the evidence must bank for retry rather than be lost.
	captured("❯ \n", attemptedAt.Add(time.Second))
	captured(reverifyTurnRow("1s"), attemptedAt.Add(2*time.Second))
	captured(reverifyTurnRow("2s"), attemptedAt.Add(3*time.Second))
	stored, err := task.GetTask("feed6597")
	require.NoError(t, err)
	require.Empty(t, stored.LastRunStatus)

	// The delivery's own verdict write lands late — the banked evidence
	// promotes it on the next capture even though no new turn is visible.
	now := time.Now()
	_, err = task.UpdateTaskStatus("feed6597", &now, TaskStatusSentUnverified)
	require.NoError(t, err)
	captured("❯ \n", attemptedAt.Add(4*time.Second))
	stored, err = task.GetTask("feed6597")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"evidence banked before the verdict write must promote once it lands")
}

// A re-send re-bounds the watch: only the newest unverified send's evidence may
// promote, since the run status carries the latest send's verdict. The older
// watch is replaced outright — a turn it might have caught belongs to a
// verdict already off the record.
func TestUnverifiedSendReArmKeepsLatestBoundary(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	seedUnverifiedTask(t, "feed6593")

	first := time.Now()
	second := first.Add(20 * time.Second)
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6593", first, task.NewTurnWatch("claude", first))
	m.armUnverifiedSendWatch(key, inst, "feed6593", second, task.NewTurnWatch("claude", second))

	m.pausedMu.Lock()
	entry := m.unverifiedSendWatches[key]["feed6593"]
	m.pausedMu.Unlock()
	require.NotNil(t, entry)
	require.True(t, entry.attemptedAt.Equal(second),
		"a later unverified send must re-bound the pending re-verification watch")
}

// Capture evidence older than the send — an in-flight frame that predates the
// attempt — must not feed its watch, and a capture the backend could not take
// feeds nothing.
func TestUnverifiedSendEvidenceFencesPreSendAndUncaptured(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnverifiedTask(t, "feed6594")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6594", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: "❯ \n", Captured: true},
		attemptedAt.Add(-time.Millisecond))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Captured: false}, attemptedAt.Add(time.Second))
	m.pausedMu.Lock()
	entry := m.unverifiedSendWatches[key]["feed6594"]
	m.pausedMu.Unlock()
	require.NotNil(t, entry, "unusable frames must not retire or mutate the pending watch")

	stored, err := task.GetTask("feed6594")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnverified, stored.LastRunStatus)
}
