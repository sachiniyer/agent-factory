package daemon

import (
	"fmt"
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

// reverifyBusyRow is claude's real mid-turn pane (task/testdata's working
// fixtures): the elapsed timer rides on the status row above the composer
// rules and "esc to interrupt" lives on a separate footer — never one row
// holding both fragments.
func reverifyBusyRow(elapsed string) string {
	return "❯ transcript\n" +
		"* Bloviating… (" + elapsed + " · ↓ 108 tokens)\n" +
		"────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────\n" +
		"  ⏸ manual mode on · esc to interrupt · ← 2 agents\n"
}

// reverifyIdleRow is the settled pane: the finished turn's done summary in the
// transcript, an idle composer, no hint anywhere.
func reverifyIdleRow() string {
	return "❯ transcript\n" +
		"✻ Cogitated for 26s · done 11:09 PM\n" +
		"────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────\n"
}

// reverifyDraftRow stages text inside the live composer — the withheld-Enter
// draft a busy target keeps staged.
func reverifyDraftRow(draft string) string {
	return "❯ transcript\n" +
		"✻ Cogitated for 26s · done 11:09 PM\n" +
		"────────────────────────────────\n" +
		"❯ " + draft + "\n" +
		"────────────────────────────────\n"
}

// unverifiedSendBackend stages the #6590 steady state: every preview shows an
// already-running turn (so the bounded Confirm is ineligible and settles
// without polling) and the send itself reports the withheld remedy.
type unverifiedSendBackend struct {
	*session.FakeBackend
}

func (b *unverifiedSendBackend) Preview(*session.Instance) (string, error) {
	return reverifyBusyRow("9m 0s"), nil
}

func (b *unverifiedSendBackend) SendPromptCommandWithStatus(*session.Instance, string) (session.PromptDeliveryStatus, error) {
	return session.PromptSentUnverified, nil
}

// unconfirmedSendBackend stages the #6819 send: the pane could not be observed
// at all — the pre-send capture errors the way an unanswerable pane does — and
// the send itself reports could-not-confirm.
type unconfirmedSendBackend struct {
	*session.FakeBackend
}

func (b *unconfirmedSendBackend) Preview(*session.Instance) (string, error) {
	return "", fmt.Errorf("pane capture unavailable")
}

func (b *unconfirmedSendBackend) SendPromptCommandWithStatus(*session.Instance, string) (session.PromptDeliveryStatus, error) {
	return session.PromptCouldNotConfirm, nil
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
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyBusyRow("9m 1s"), Captured: true}, attemptedAt.Add(time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyIdleRow(), Captured: true}, attemptedAt.Add(2*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyBusyRow("1s"), Captured: true}, attemptedAt.Add(3*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyBusyRow("2s"), Captured: true}, attemptedAt.Add(4*time.Second))
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

// seedUnconfirmedTask is seedUnverifiedTask for a send whose pane could not be
// observed at all: the run record carries the honest "no verdict either way".
func seedUnconfirmedTask(t *testing.T, taskID string) {
	t.Helper()
	require.NoError(t, task.AddTask(task.Task{
		ID: taskID, Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: t.TempDir(), Enabled: true,
	}))
	now := time.Now()
	_, err := task.UpdateTaskStatus(taskID, &now, TaskStatusSentUnconfirmed)
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
	captured(reverifyBusyRow("9m 1s"), attemptedAt.Add(time.Second))
	captured(reverifyBusyRow("9m 2s"), attemptedAt.Add(2*time.Second))
	stored, err := task.GetTask("feed6590")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnverified, stored.LastRunStatus,
		"a turn older than the send must not promote its verdict")

	// That turn ends and a new one begins — the queued prompt's work.
	captured(reverifyIdleRow(), attemptedAt.Add(3*time.Second))
	captured(reverifyBusyRow("1s"), attemptedAt.Add(4*time.Second))
	captured(reverifyBusyRow("2s"), attemptedAt.Add(5*time.Second))
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
			session.Observation{Content: reverifyDraftRow("staged draft text"), Captured: true},
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
		session.Observation{Content: reverifyIdleRow(), Captured: true}, time.Now())
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

	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyIdleRow(), Captured: true}, attemptedAt.Add(time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyBusyRow("1s"), Captured: true}, attemptedAt.Add(2*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyBusyRow("2s"), Captured: true}, attemptedAt.Add(3*time.Second))
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

	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyIdleRow(), Captured: true}, attemptedAt.Add(time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyBusyRow("1s"), Captured: true}, attemptedAt.Add(2*time.Second))
	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyBusyRow("2s"), Captured: true}, attemptedAt.Add(3*time.Second))
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
	captured(reverifyIdleRow(), attemptedAt.Add(time.Second))
	captured(reverifyBusyRow("1s"), attemptedAt.Add(2*time.Second))
	captured(reverifyBusyRow("2s"), attemptedAt.Add(3*time.Second))
	stored, err := task.GetTask("feed6597")
	require.NoError(t, err)
	require.Empty(t, stored.LastRunStatus)

	// The delivery's own verdict write lands late — the banked evidence
	// promotes it on the next capture even though no new turn is visible.
	now := time.Now()
	_, err = task.UpdateTaskStatus("feed6597", &now, TaskStatusSentUnverified)
	require.NoError(t, err)
	captured(reverifyIdleRow(), attemptedAt.Add(4*time.Second))
	stored, err = task.GetTask("feed6597")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"evidence banked before the verdict write must promote once it lands")
}

// A retargeted re-send supersedes the earlier send's pending watch on the OLD
// session too: the verdict on the row now belongs to the newer delivery, and
// the stale watch's banked evidence — timestamped inside the first send's
// slack — must never reach it (#6592 review).
func TestUnverifiedSendRetargetSupersedesEarlierWatch(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	instA := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	instB := registerStarted(t, m, repoID, repoPath, "work", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6598", Name: "captain-events", WatchCmd: "emit.sh",
		TargetSession: "work", ProjectPath: t.TempDir(), Enabled: true,
	}))

	keyA := stableSessionKey(repoID, instA)
	keyB := stableSessionKey(repoID, instB)
	attemptedAt := time.Now()

	// The first send's watch fires before its verdict is written — the
	// refused CAS banks the evidence for retry.
	m.armUnverifiedSendWatch(keyA, instA, "feed6598", attemptedAt, task.NewTurnWatch("claude", attemptedAt))
	capturedA := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(keyA, instA, session.Observation{Content: content, Captured: true}, at)
	}
	capturedA(reverifyIdleRow(), attemptedAt.Add(time.Second))
	capturedA(reverifyBusyRow("1s"), attemptedAt.Add(2*time.Second))
	capturedA(reverifyBusyRow("2s"), attemptedAt.Add(3*time.Second))

	// The task is retargeted and re-sends unverified to the other session:
	// its watch is the only one that may ever promote again.
	later := attemptedAt.Add(4 * time.Second)
	m.armUnverifiedSendWatch(keyB, instB, "feed6598", later, task.NewTurnWatch("claude", later))
	m.pausedMu.Lock()
	stale := m.unverifiedSendWatches[keyA]["feed6598"]
	m.pausedMu.Unlock()
	require.Nil(t, stale, "a retargeted re-send must supersede the earlier send's watch on the old session")

	// The newer delivery's verdict lands inside the FIRST send's write
	// slack — exactly where the stale banked evidence would fit — and a
	// stale capture on the old session still must not promote it.
	now := later.Add(time.Second)
	_, err := task.UpdateTaskStatus("feed6598", &now, TaskStatusSentUnverified)
	require.NoError(t, err)
	capturedA(reverifyIdleRow(), attemptedAt.Add(5*time.Second))
	stored, err := task.GetTask("feed6598")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnverified, stored.LastRunStatus,
		"banked evidence from a superseded delivery must never promote a newer verdict")

	// The newer send's own evidence still promotes its own verdict.
	capturedB := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(keyB, instB, session.Observation{Content: content, Captured: true}, at)
	}
	capturedB(reverifyIdleRow(), later.Add(time.Second))
	capturedB(reverifyBusyRow("1s"), later.Add(2*time.Second))
	capturedB(reverifyBusyRow("2s"), later.Add(3*time.Second))
	stored, err = task.GetTask("feed6598")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"the latest send's own evidence must still promote its verdict")
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

	m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: reverifyIdleRow(), Captured: true},
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

// A restart drops the in-memory watch, but the verdict row is durable: a
// sent-unverified row still inside its window re-arms on the first sweep. The
// rebuilt boundary is the verdict's own write stamp — strictly later than the
// lost attempt instant — so the contract can only miss, never weaken (#6590).
func TestUnverifiedSendReArmsFromVerdictRowOnBoot(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6599", Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: repoPath, Enabled: true,
	}))
	lastRun := time.Now().Add(-time.Minute)
	_, err := task.UpdateTaskStatusForTarget("feed6599", &lastRun, TaskStatusSentUnverified, "root")
	require.NoError(t, err)

	m.sweepUnverifiedSendWatches() // the boot sweep re-arms in-window rows

	key := stableSessionKey(repoID, inst)
	m.pausedMu.Lock()
	entry := m.unverifiedSendWatches[key]["feed6599"]
	m.pausedMu.Unlock()
	require.NotNil(t, entry, "a sent-unverified row inside its window must re-arm after restart")
	require.True(t, entry.attemptedAt.Equal(lastRun),
		"the verdict's own write stamp is the rebuilt evidence boundary")

	// The rebuilt watch promotes on the same contract: a turn demonstrably
	// beginning after the stamp.
	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	captured(reverifyIdleRow(), lastRun.Add(2*time.Second))
	captured(reverifyBusyRow("1s"), lastRun.Add(3*time.Second))
	captured(reverifyBusyRow("2s"), lastRun.Add(4*time.Second))
	stored, err := task.GetTask("feed6599")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"a re-armed watch must still promote on post-send turn evidence")
}

// A row already past its window stays frozen: a turn that far out was never
// attributable to the send, so the sweep leaves the verdict unverified rather
// than arming a watch it could not honestly feed.
func TestUnverifiedSendReArmSkipsExpiredRow(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6600", Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: repoPath, Enabled: true,
	}))
	lastRun := time.Now().Add(-2 * sendReverifyWindow)
	_, err := task.UpdateTaskStatusForTarget("feed6600", &lastRun, TaskStatusSentUnverified, "root")
	require.NoError(t, err)

	m.sweepUnverifiedSendWatches()

	key := stableSessionKey(repoID, inst)
	m.pausedMu.Lock()
	armed := m.unverifiedSendWatches[key]["feed6600"]
	m.pausedMu.Unlock()
	require.Nil(t, armed, "a verdict already past its window must stay frozen")
}

// A row whose status already moved on — delivered, errored, retried — is not
// re-armed: only a row still reading sent-unverified carries an open verdict.
func TestUnverifiedSendReArmSkipsSettledRows(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6601", Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: repoPath, Enabled: true,
	}))
	lastRun := time.Now().Add(-time.Minute)
	_, err := task.UpdateTaskStatus("feed6601", &lastRun, "sent")
	require.NoError(t, err)

	m.sweepUnverifiedSendWatches()

	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	require.Empty(t, m.unverifiedSendWatches, "a settled verdict must not arm a re-verification watch")
}

// A task retargeted after the send keeps its verdict bound to the session the
// delivery actually went to: the re-arm reads the recorded LastRunTarget, not
// the task's current config, so an unrelated turn on the NEW target can never
// promote the old send (#6590 review).
func TestUnverifiedSendReArmBindsRecordedTarget(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	instA := registerStarted(t, m, repoID, repoPath, "a", session.NewFakeBackend(), true, session.Running)
	instB := registerStarted(t, m, repoID, repoPath, "b", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6602", Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "b", ProjectPath: repoPath, Enabled: true, // retargeted after the send
	}))
	lastRun := time.Now().Add(-time.Minute)
	_, err := task.UpdateTaskStatusForTarget("feed6602", &lastRun, TaskStatusSentUnverified, "a")
	require.NoError(t, err)

	m.sweepUnverifiedSendWatches()

	m.pausedMu.Lock()
	onA := m.unverifiedSendWatches[stableSessionKey(repoID, instA)]["feed6602"]
	onB := m.unverifiedSendWatches[stableSessionKey(repoID, instB)]["feed6602"]
	m.pausedMu.Unlock()
	require.NotNil(t, onA, "the send went to a: its watch re-arms on a")
	require.Nil(t, onB, "the retargeted config must not arm the new target")
}

// A row recorded before the delivery target existed carries no identity to
// bind: a verdict that cannot name its session stays frozen rather than arm
// the task's current — possibly retargeted — config (#6590 review).
func TestUnverifiedSendReArmSkipsUntargetedRow(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6603", Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: repoPath, Enabled: true,
	}))
	lastRun := time.Now().Add(-time.Minute)
	_, err := task.UpdateTaskStatus("feed6603", &lastRun, TaskStatusSentUnverified)
	require.NoError(t, err)

	m.sweepUnverifiedSendWatches()

	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	require.Empty(t, m.unverifiedSendWatches, "a row with no recorded delivery target must not re-arm")
}

// #6819: a task delivery the pane could not confirm at all records
// sent-unconfirmed and arms the SAME re-verification watch an unverified send
// does — the ReverifyWatch the confirmation builds for an unobservable pane
// carries the identical evidence contract, bound to the send's attempt
// instant. A turn the agent demonstrably began after the send promotes the
// recorded verdict to sent.
func TestUnconfirmedSendArmsReverifyAndPromotes(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root",
		&unconfirmedSendBackend{FakeBackend: session.NewFakeBackend()}, true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6819", Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: t.TempDir(), Enabled: true,
	}))

	status, err := m.sendPromptGuarded(SendPromptRequest{
		Title: "root", RepoID: repoID, Prompt: "nightly check", TaskOrigin: true, TaskID: "feed6819",
	}, nil)
	require.NoError(t, err)
	require.Equal(t, session.PromptCouldNotConfirm, status)

	// The watcher records the verdict the way taskrun maps it.
	now := time.Now()
	_, uerr := task.UpdateTaskStatus("feed6819", &now, TaskStatusSentUnconfirmed)
	require.NoError(t, uerr)

	key := stableSessionKey(repoID, inst)
	m.pausedMu.Lock()
	armed := m.unverifiedSendWatches[key]["feed6819"]
	m.pausedMu.Unlock()
	require.NotNil(t, armed, "a could-not-confirm task send must arm the same re-verification watch")
	require.True(t, m.unverifiedSendWatchPending(key),
		"a pending watch must make the paused backstop observe the attached pane")
	attemptedAt := armed.attemptedAt

	// The pane becomes observable again and the agent begins the send's turn:
	// the verdict promotes exactly the way a sent-unverified one does.
	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	captured(reverifyIdleRow(), attemptedAt.Add(time.Second))
	captured(reverifyBusyRow("1s"), attemptedAt.Add(2*time.Second))
	captured(reverifyBusyRow("2s"), attemptedAt.Add(3*time.Second))
	stored, err := task.GetTask("feed6819")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"a turn beginning after an unconfirmed send must promote its verdict to sent")
}

// The pane stays unobservable or no turn ever begins: nothing weaker than the
// send-bound turn evidence fires the watch, so the honest verdict stands and
// the watch retires on its window — for an unconfirmed send exactly as for an
// unverified one.
func TestUnconfirmedSendNoTurnStaysUnconfirmed(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnconfirmedTask(t, "feed6820")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6820", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	// Captures that fail — the pane still unobservable — feed nothing, and
	// readable frames with no new turn — an idle composer, a staged draft —
	// never fire the watch.
	for i := 0; i < 5; i++ {
		at := attemptedAt.Add(time.Duration(i+1) * time.Second)
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Captured: false}, at)
		m.noteUnverifiedSendEvidence(key, inst,
			session.Observation{Content: reverifyDraftRow("staged draft text"), Captured: true}, at)
	}
	stored, err := task.GetTask("feed6820")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnconfirmed, stored.LastRunStatus,
		"an unconfirmed send with no post-send turn must stay sent-unconfirmed")

	// Past the re-verification window the watch retires: a turn beginning this
	// late can no longer be attributed to the send. Arm at an instant already
	// outside the window rather than racing the daemon clock.
	expiredAttempt := attemptedAt.Add(-2 * sendReverifyWindow)
	m.armUnverifiedSendWatch(key, inst, "feed6820", expiredAttempt, task.NewTurnWatch("claude", expiredAttempt))
	m.noteUnverifiedSendEvidence(key, inst,
		session.Observation{Content: reverifyIdleRow(), Captured: true}, time.Now())
	m.pausedMu.Lock()
	_, still := m.unverifiedSendWatches[key]
	m.pausedMu.Unlock()
	require.False(t, still, "an expired re-verification watch must be retired, not fed")
}

// A turn already running when the send went out — its own elapsed clock
// outdates the send — is not this send's evidence, so it must not promote a
// sent-unconfirmed verdict either.
func TestUnconfirmedSendPreSendTurnDoesNotPromote(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnconfirmedTask(t, "feed6821")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed6821", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	// The turn running at send time keeps ticking; its clock outdates the send.
	captured(reverifyBusyRow("9m 1s"), attemptedAt.Add(time.Second))
	captured(reverifyBusyRow("9m 2s"), attemptedAt.Add(2*time.Second))
	stored, err := task.GetTask("feed6821")
	require.NoError(t, err)
	require.Equal(t, TaskStatusSentUnconfirmed, stored.LastRunStatus,
		"a turn older than the send must not promote a sent-unconfirmed verdict")
}

// A restart drops the in-memory watch, but the verdict row is durable: a
// sent-unconfirmed row still inside its window re-arms on the first sweep,
// same as a sent-unverified one (#6819).
func TestUnconfirmedSendReArmsFromVerdictRowOnBoot(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed6822", Name: "nightly", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: repoPath, Enabled: true,
	}))
	lastRun := time.Now().Add(-time.Minute)
	_, err := task.UpdateTaskStatusForTarget("feed6822", &lastRun, TaskStatusSentUnconfirmed, "root")
	require.NoError(t, err)

	m.sweepUnverifiedSendWatches() // the boot sweep re-arms in-window rows

	key := stableSessionKey(repoID, inst)
	m.pausedMu.Lock()
	entry := m.unverifiedSendWatches[key]["feed6822"]
	m.pausedMu.Unlock()
	require.NotNil(t, entry, "a sent-unconfirmed row inside its window must re-arm after restart")
	require.True(t, entry.attemptedAt.Equal(lastRun),
		"the verdict's own write stamp is the rebuilt evidence boundary")

	// The rebuilt watch promotes on the same contract: a turn demonstrably
	// beginning after the stamp.
	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	captured(reverifyIdleRow(), lastRun.Add(2*time.Second))
	captured(reverifyBusyRow("1s"), lastRun.Add(3*time.Second))
	captured(reverifyBusyRow("2s"), lastRun.Add(4*time.Second))
	stored, err := task.GetTask("feed6822")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"a re-armed unconfirmed verdict must still promote on post-send turn evidence")
}
