package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// fakeStagedChecker is the runtime half of the staged-draft recheck (#7545):
// it answers scripted verdicts and counts its captures, so the watch's
// bounded-cost contract is itself under test — a resolved send, or a latched
// flag, must never be captured for again.
type fakeStagedChecker struct {
	session.AgentServer // embedded for the interface; only the checker method is exercised
	mu                  sync.Mutex
	reads               []session.StagedDraftRead
	calls               int
}

func (f *fakeStagedChecker) BoundDraftStillStaged() (session.StagedDraftRead, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.reads) == 0 {
		return session.StagedDraftNoEvidence, "no scripted verdict"
	}
	r := f.reads[0]
	if len(f.reads) > 1 {
		f.reads = f.reads[1:]
	}
	return r, "scripted verdict"
}

func (f *fakeStagedChecker) captureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// stagedDraftFixture registers a Ready session whose last send was recorded
// sent-unverified at attemptAt — the #7545 arming condition.
func stagedDraftFixture(t *testing.T, attemptAt time.Time) (*Manager, string, string, *session.Instance) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "worker",
		readyFakeBackend{FakeBackend: session.NewFakeBackend()}, true, session.Ready)
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, attemptAt))
	return manager, repoID, repoPath, inst
}

func stagedDraftKey(repoID string, inst *session.Instance) string {
	return stableSessionKey(repoID, inst)
}

// feedStagedDraftTick mirrors the poll's ordering (#7545): the feed runs under
// the instance's observation-settlement fence — where it may only RECORD a due
// flag, never take the session op lock — and raiseDueStagedDraftFlag lands the
// flag after the fence releases.
func feedStagedDraftTick(manager *Manager, repoID, key string, inst *session.Instance, checker session.AgentServer) {
	release := inst.HoldAgentObservationSettlement()
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	release()
	manager.raiseDueStagedDraftFlag(repoID, key, inst)
}

// stagedDraftWatchEntry is the test's read of one watch row.
func stagedDraftWatchEntry(manager *Manager, key string) *stagedDraftWatch {
	manager.pausedMu.Lock()
	defer manager.pausedMu.Unlock()
	return manager.stagedDraftWatches[key]
}

// Staged + Ready sustained across the settle window raises the session's
// attention flag; once latched, the watch spends no more captures on that
// send.
func TestStagedDraftWatchFlagsSustainedStagedIdle(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	now := base
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, base.Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)

	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "one staged tick must not flag before the settle window")
	require.Equal(t, 1, checker.captureCount())

	now = base.Add(stagedDraftSettleWindow - time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "the sustain must be unbroken across the whole window")

	now = base.Add(stagedDraftSettleWindow + time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, inst.HasAttention(), "sustained Ready+staged past the window flags attention")
	reason := inst.Attention().Reason
	require.Contains(t, reason, "still unsubmitted in the composer")
	require.NotContains(t, reason, stagedTailFixtureText, "no prompt text may leave the pane")
	require.True(t, stagedDraftWatchEntry(manager, key).closed, "a landed flag closes the send's watch")

	calls := checker.captureCount()
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.Equal(t, calls, checker.captureCount(), "no captures after the flag latches")
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.Equal(t, calls, checker.captureCount(), "a latched send is never rechecked")
}

const stagedTailFixtureText = "the end of the staged prompt"

// The Captain's named case: a delivered send's echo above the composer ends
// the watch — the common sent-unverified resolution costs a bounded number
// of captures and then none. The closed entry REMEMBERS that resolution: a
// Ready session's durable verdict stays sent-unverified, so without it every
// idle poll would re-capture forever (#7545 review).
func TestStagedDraftWatchDeliveredEchoEndsTheWatch(t *testing.T) {
	now := time.Now()
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, now.Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{
		session.StagedDraftStaged, session.StagedDraftSubmitted}}
	key := stagedDraftKey(repoID, inst)

	feedStagedDraftTick(manager, repoID, key, inst, checker)
	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry, "a staged read arms the watch")
	require.False(t, entry.closed)

	feedStagedDraftTick(manager, repoID, key, inst, checker)
	entry = stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry, "a resolved attempt is remembered, not forgotten")
	require.True(t, entry.closed, "a submitted echo closes the watch")
	require.False(t, inst.HasAttention(), "a resolved send is never flagged")

	calls := checker.captureCount()
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.Equal(t, calls, checker.captureCount(), "the same attempt is never captured for again")
}

// Mid-turn queued text is the #4622 exclusion: a Running session's watch is
// dropped and the checker is never asked.
func TestStagedDraftWatchNeverArmsWhileRunning(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
	feedStagedDraftTick(manager, repoID, stagedDraftKey(repoID, inst), inst, checker)
	require.Equal(t, 0, checker.captureCount(), "a mid-turn session is never captured")
	require.Nil(t, stagedDraftWatchEntry(manager, stagedDraftKey(repoID, inst)))
}

// A delivered send's watch cannot fire: the gate reads the recorded verdict,
// not just the pane.
func TestStagedDraftWatchNeverArmsForADeliveredSend(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, time.Now()))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	feedStagedDraftTick(manager, repoID, stagedDraftKey(repoID, inst), inst, checker)
	require.Equal(t, 0, checker.captureCount())
}

// A send that bound no evidence — an empty prompt, a boundary that could not
// attribute — can never be flagged. Its one verdict is remembered as closed,
// so the same attempt is never asked about again.
func TestStagedDraftWatchNoEvidenceCostsNothing(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	checker := &fakeStagedChecker{} // unscripted: reports NoEvidence
	key := stagedDraftKey(repoID, inst)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry, "a send that bound nothing is asked once and remembered")
	require.True(t, entry.closed)
	require.False(t, inst.HasAttention())
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.Equal(t, 1, checker.captureCount(), "the no-evidence verdict costs exactly one read")
}

// A runtime without the capability — the off-box backends of v1 — drops the
// watch rather than guessing, but keeps the closed memory of an attempt it
// already resolved.
func TestStagedDraftWatchDropsForUncheckableRuntimes(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	key := stagedDraftKey(repoID, inst)
	feedStagedDraftTick(manager, repoID, key, inst, nil)
	require.Nil(t, stagedDraftWatchEntry(manager, key))

	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftSubmitted}}
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, stagedDraftWatchEntry(manager, key).closed, "setup: a resolved attempt")
	feedStagedDraftTick(manager, repoID, key, inst, nil)
	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry, "a runtime flap must not re-pay a resolved attempt")
	require.True(t, entry.closed)
	require.Equal(t, 1, checker.captureCount(), "the resolved attempt was never re-read")
}

// A newer send supersedes the watch: the sustain restarts against the new
// attempt, never against a verdict an older send earned.
func TestStagedDraftWatchRearmsForANewerSend(t *testing.T) {
	base := time.Now()
	now := base
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, base.Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)
	feedStagedDraftTick(manager, repoID, key, inst, checker)

	second := base.Add(30 * time.Second)
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, second))
	now = base.Add(stagedDraftSettleWindow + time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "the newer send's sustain starts over — an old window must not carry")

	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry)
	require.True(t, entry.attemptAt.Equal(second), "the watch belongs to the newer send")
}

// The sustain is unbroken Ready: a Running tick drops the watch, and a later
// Ready re-arms from zero.
func TestStagedDraftWatchSustainResetsAcrossRunning(t *testing.T) {
	base := time.Now()
	now := base
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, base.Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)
	feedStagedDraftTick(manager, repoID, key, inst, checker)

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))

	now = base.Add(stagedDraftSettleWindow + time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "the Running tick reset the sustain — the window restarts")
}

// An agent's own attention request holds the row: the flag refuses to
// clobber it and stays unclosed so a later tick retries — and does, the
// moment the row frees.
func TestStagedDraftWatchNeverClobbersAnAttentionRequest(t *testing.T) {
	base := time.Now()
	now := base
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, base.Add(-time.Minute))
	_, err := inst.SetAttention("the agent's own question", now)
	require.NoError(t, err)
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)

	feedStagedDraftTick(manager, repoID, key, inst, checker)
	now = base.Add(stagedDraftSettleWindow + time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)

	reason := inst.Attention().Reason
	require.Equal(t, "the agent's own question", reason, "the flag must not overwrite what the agent asked")
	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry)
	require.False(t, entry.closed, "a refused flag stays open so the row frees it can retry")
	require.False(t, entry.due, "the refused due mark is consumed — the next tick re-marks it")

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, inst.HasAttention(), "the flag lands on the tick after the row frees")
	require.Contains(t, inst.Attention().Reason, "still unsubmitted in the composer")
}

// A dismissed flag is honored: clearing the attention after it latched does
// not re-raise it for the same send.
func TestStagedDraftWatchHonorsDismissal(t *testing.T) {
	base := time.Now()
	now := base
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, base.Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	now = base.Add(stagedDraftSettleWindow + time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, inst.HasAttention())

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	calls := checker.captureCount()
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "a user's dismissal is never re-raised for the same send")
	require.Equal(t, calls, checker.captureCount(), "and it costs no further captures")
}

// The sweep reclaims watches for sessions the daemon no longer tracks.
func TestStagedDraftWatchSweepDropsDeadSessions(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)
	feedStagedDraftTick(manager, repoID, key, inst, checker)

	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, inst.Title))
	manager.mu.Unlock()
	manager.sweepStagedDraftWatches()
	require.Nil(t, stagedDraftWatchEntry(manager, key), "a watch whose session is gone is swept")
}

// The flag the poll owes lands only AFTER the observation fence releases:
// under it, taking the session op lock deadlocks against an automated send
// holding that lock while it waits on the fence (Codex #7547). This test
// parks an automated send inside its op lock, before the fence, and proves
// the under-fence feed completes anyway — a hang here was the bug, so every
// phase is timeout-fenced rather than awaited.
func TestStagedDraftFlagNeverBlocksUnderTheObservationFence(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	manager, repoID, _, inst := stagedDraftFixture(t, attemptAt)
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)
	manager.pausedMu.Lock()
	manager.stagedDraftWatches[key] = &stagedDraftWatch{
		attemptAt: attemptAt, since: time.Now().Add(-2 * stagedDraftSettleWindow)}
	manager.pausedMu.Unlock()

	// The automated send takes the session op lock, then parks at the hook
	// before it can take the observation fence — the deadlock's own order.
	reachedOpLock := make(chan struct{})
	releaseSend := make(chan struct{})
	originalHook := testHookTaskPromptBeforeObservationFence
	var once sync.Once
	testHookTaskPromptBeforeObservationFence = func() {
		once.Do(func() { close(reachedOpLock); <-releaseSend })
	}
	t.Cleanup(func() { testHookTaskPromptBeforeObservationFence = originalHook })
	sendDone := make(chan error, 1)
	go func() {
		_, err := manager.SendPromptWithStatus(SendPromptRequest{
			Title: inst.Title, RepoID: repoID, Prompt: "a task send", TaskOrigin: true})
		sendDone <- err
	}()
	select {
	case <-reachedOpLock:
	case <-time.After(5 * time.Second):
		t.Fatal("the automated send never reached its observation fence")
	}

	// The poll-side feed — the code that USED to take the op lock under the
	// fence — must complete while the send still holds it.
	feedDone := make(chan struct{})
	go func() {
		release := inst.HoldAgentObservationSettlement()
		manager.noteStagedDraftEvidence(repoID, key, inst, checker)
		release()
		close(feedDone)
	}()
	select {
	case <-feedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the staged-draft feed blocked on the send's op lock under the observation fence")
	}
	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry)
	require.True(t, entry.due, "the under-fence feed only records that the flag is due")
	require.False(t, inst.HasAttention(), "nothing may be raised under the fence")

	// The deferred raise queues behind the send's whole guarded section —
	// flagging a condition the send is about to change would be a lie — so it
	// only completes after the send lets the op lock go.
	flushDone := make(chan struct{})
	go func() {
		manager.raiseDueStagedDraftFlag(repoID, key, inst)
		close(flushDone)
	}()
	close(releaseSend)
	select {
	case <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the automated send stayed blocked after the fence was free")
	}
	select {
	case <-flushDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the deferred flag raise stayed blocked after the op lock was free")
	}
	require.False(t, inst.HasAttention(),
		"the send superseded the due mark — re-validation must refuse to flag it")
	require.False(t, entry.due, "a refused due mark is consumed")

	// And the deferred raise is still the working path: re-qualify and the
	// flag lands on the next tick.
	second := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, second))
	manager.pausedMu.Lock()
	manager.stagedDraftWatches[key] = &stagedDraftWatch{
		attemptAt: second, since: time.Now().Add(-2 * stagedDraftSettleWindow)}
	manager.pausedMu.Unlock()
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, inst.HasAttention(), "the deferred raise lands the flag after the race")
	require.Contains(t, inst.Attention().Reason, "still unsubmitted in the composer")
}

// The reason string follows the repo's copy rules and carries the send's
// time, never the prompt.
func TestStagedDraftReasonCopy(t *testing.T) {
	attemptAt := time.Date(2026, 11, 18, 3, 51, 0, 0, time.UTC)
	reason := stagedDraftAttentionReason(attemptAt)
	if strings.Contains(reason, stagedTailFixtureText) {
		t.Fatalf("the reason must never carry prompt text: %q", reason)
	}
	if !strings.Contains(reason, "still unsubmitted in the composer") ||
		!strings.Contains(reason, "—") || strings.Contains(reason, "...") {
		t.Fatalf("the reason must be short, sentence-case, and use the clause dash: %q", reason)
	}
}
