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

	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "one staged tick must not flag before the settle window")
	require.Equal(t, 1, checker.captureCount())

	now = base.Add(stagedDraftSettleWindow - time.Second)
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "the sustain must be unbroken across the whole window")

	now = base.Add(stagedDraftSettleWindow + time.Second)
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.True(t, inst.HasAttention(), "sustained Ready+staged past the window flags attention")
	reason := inst.Attention().Reason
	require.Contains(t, reason, "still unsubmitted in the composer")
	require.NotContains(t, reason, stagedTailFixtureText, "no prompt text may leave the pane")

	calls := checker.captureCount()
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.Equal(t, calls, checker.captureCount(), "no captures after the flag latches")
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.Equal(t, calls, checker.captureCount(), "a latched send is never rechecked")
}

const stagedTailFixtureText = "the end of the staged prompt"

// The Captain's named case: a delivered send's echo above the composer ends
// the watch — the common sent-unverified resolution costs a bounded number
// of captures and then none.
func TestStagedDraftWatchDeliveredEchoEndsTheWatch(t *testing.T) {
	now := time.Now()
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, now.Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{
		session.StagedDraftStaged, session.StagedDraftSubmitted}}
	key := stagedDraftKey(repoID, inst)

	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	manager.pausedMu.Lock()
	_, armed := manager.stagedDraftWatches[key]
	manager.pausedMu.Unlock()
	require.True(t, armed, "a staged read arms the watch")

	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	manager.pausedMu.Lock()
	_, armed = manager.stagedDraftWatches[key]
	manager.pausedMu.Unlock()
	require.False(t, armed, "a submitted echo ends the watch")
	require.False(t, inst.HasAttention(), "a resolved send is never flagged")
}

// Mid-turn queued text is the #4622 exclusion: a Running session's watch is
// dropped and the checker is never asked.
func TestStagedDraftWatchNeverArmsWhileRunning(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
	manager.noteStagedDraftEvidence(repoID, stagedDraftKey(repoID, inst), inst, checker)
	require.Equal(t, 0, checker.captureCount(), "a mid-turn session is never captured")
	manager.pausedMu.Lock()
	_, armed := manager.stagedDraftWatches[stagedDraftKey(repoID, inst)]
	manager.pausedMu.Unlock()
	require.False(t, armed)
}

// A delivered send's watch cannot fire: the gate reads the recorded verdict,
// not just the pane.
func TestStagedDraftWatchNeverArmsForADeliveredSend(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, time.Now()))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	manager.noteStagedDraftEvidence(repoID, stagedDraftKey(repoID, inst), inst, checker)
	require.Equal(t, 0, checker.captureCount())
}

// A send that bound no evidence — an empty prompt, a boundary that could not
// attribute — can never be flagged, and its verdict ends the watch without a
// second capture.
func TestStagedDraftWatchNoEvidenceCostsNothing(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	checker := &fakeStagedChecker{} // unscripted: reports NoEvidence
	manager.noteStagedDraftEvidence(repoID, stagedDraftKey(repoID, inst), inst, checker)
	manager.pausedMu.Lock()
	_, armed := manager.stagedDraftWatches[stagedDraftKey(repoID, inst)]
	manager.pausedMu.Unlock()
	require.False(t, armed, "a send that bound nothing arms nothing")
	require.False(t, inst.HasAttention())
}

// A runtime without the capability — the off-box backends of v1 — drops the
// watch rather than guessing.
func TestStagedDraftWatchDropsForUncheckableRuntimes(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	manager.noteStagedDraftEvidence(repoID, stagedDraftKey(repoID, inst), inst, nil)
	manager.pausedMu.Lock()
	_, armed := manager.stagedDraftWatches[stagedDraftKey(repoID, inst)]
	manager.pausedMu.Unlock()
	require.False(t, armed)
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
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)

	second := base.Add(30 * time.Second)
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, second))
	now = base.Add(stagedDraftSettleWindow + time.Second)
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "the newer send's sustain starts over — an old window must not carry")

	manager.pausedMu.Lock()
	entry := manager.stagedDraftWatches[key]
	manager.pausedMu.Unlock()
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
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))

	now = base.Add(stagedDraftSettleWindow + time.Second)
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "the Running tick reset the sustain — the window restarts")
}

// An agent's own attention request holds the row: the flag refuses to
// clobber it and stays unlatched so a later tick retries.
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

	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	now = base.Add(stagedDraftSettleWindow + time.Second)
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)

	reason := inst.Attention().Reason
	require.Equal(t, "the agent's own question", reason, "the flag must not overwrite what the agent asked")
	manager.pausedMu.Lock()
	entry := manager.stagedDraftWatches[key]
	manager.pausedMu.Unlock()
	require.NotNil(t, entry)
	require.False(t, entry.flagged, "a refused raise stays unlatched so the row frees it can retry")
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
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	now = base.Add(stagedDraftSettleWindow + time.Second)
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.True(t, inst.HasAttention())

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	calls := checker.captureCount()
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "a user's dismissal is never re-raised for the same send")
	require.Equal(t, calls, checker.captureCount(), "and it costs no further captures")
}

// The sweep reclaims watches for sessions the daemon no longer tracks.
func TestStagedDraftWatchSweepDropsDeadSessions(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)
	manager.noteStagedDraftEvidence(repoID, key, inst, checker)

	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, inst.Title))
	manager.mu.Unlock()
	manager.sweepStagedDraftWatches()
	manager.pausedMu.Lock()
	_, armed := manager.stagedDraftWatches[key]
	manager.pausedMu.Unlock()
	require.False(t, armed, "a watch whose session is gone is swept")
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
