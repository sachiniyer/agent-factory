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

// stagedReadingBackend scripts the staged-draft verdict through the real
// forwarding path — localAgentServer asserts stagedDraftBackend on the
// current backend and calls it — so a refreshInstanceStatus-level test
// drives the same `as` the production poll reads, not a stand-in handed in
// from outside.
type stagedReadingBackend struct {
	readyFakeBackend
	checker *fakeStagedChecker
}

func (b stagedReadingBackend) BoundDraftStillStaged(*session.Instance) (session.StagedDraftRead, string) {
	return b.checker.BoundDraftStillStaged()
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
// clobber it — and while the row is owned the feed spends neither a capture
// nor a due mark on it (#7547 independent review). Once the row frees the
// watch arms normally and the flag lands after the sustain.
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
	require.Equal(t, 0, checker.captureCount(),
		"no capture is spent while another attention request owns the row")
	require.Nil(t, stagedDraftWatchEntry(manager, key),
		"and nothing is marked due while the row is owned — the tick after it frees re-arms")

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(), "the sustain only accrues on ticks that actually read the pane")
	now = base.Add(2*stagedDraftSettleWindow + time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, inst.HasAttention(), "the flag lands once the row is free and the sustain holds")
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

	// The deferred raise no longer queues behind the send at all: the op lock
	// is TryLock'd, so the contended flush RETURNS at once — the due mark
	// stays, owed to a later tick — instead of parking the poll behind the
	// send's whole guarded section.
	flushDone := make(chan struct{})
	go func() {
		manager.raiseDueStagedDraftFlag(repoID, key, inst)
		close(flushDone)
	}()
	select {
	case <-flushDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the deferred raise must return on contention, not block behind the send")
	}
	require.True(t, entry.due, "a contended flush keeps the due mark for a later tick")
	require.False(t, inst.HasAttention())

	close(releaseSend)
	select {
	case <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the automated send stayed blocked after the fence was free")
	}

	// The send superseded the due mark: its own attempt record moved the
	// verdict, so the next tick drops the open watch rather than flagging a
	// send that is no longer sent-unverified.
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(),
		"the send superseded the due mark — a moved verdict must never flag")
	require.Nil(t, stagedDraftWatchEntry(manager, key),
		"a send whose verdict moved off sent-unverified retires its open watch")

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

// The two refusal classes, pinned separately: contention (the op lock is
// busy) KEEPS the due mark — the flag is still owed, so a later tick lands
// it — while a raise refused because the condition it re-validates moved
// CONSUMES the mark: a newer attempt, a delivered verdict, a Running turn,
// or another attention request owning the row each mean this due mark is
// stale, and the next qualifying tick re-marks from the state that is true
// then (#7547).
func TestStagedDraftFlushConsumesTheDueMarkWhenTheConditionMoved(t *testing.T) {
	for name, move := range map[string]func(t *testing.T, inst *session.Instance, attemptAt time.Time){
		"a newer attempt supersedes the send": func(t *testing.T, inst *session.Instance, _ time.Time) {
			require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, time.Now()))
		},
		"the verdict moved to delivered": func(t *testing.T, inst *session.Instance, _ time.Time) {
			require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, time.Now()))
		},
		"the session went back to Running": func(t *testing.T, inst *session.Instance, _ time.Time) {
			require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
		},
		"another attention request owns the row": func(t *testing.T, inst *session.Instance, attemptAt time.Time) {
			_, err := inst.SetAttention("the agent's own question", attemptAt)
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			attemptAt := time.Now().Add(-time.Minute)
			manager, repoID, _, inst := stagedDraftFixture(t, attemptAt)
			key := stagedDraftKey(repoID, inst)
			manager.pausedMu.Lock()
			manager.stagedDraftWatches[key] = &stagedDraftWatch{
				attemptAt: attemptAt, since: attemptAt, due: true, dueWhy: "test"}
			manager.pausedMu.Unlock()

			move(t, inst, attemptAt)
			manager.raiseDueStagedDraftFlag(repoID, key, inst)

			entry := stagedDraftWatchEntry(manager, key)
			require.NotNil(t, entry, "the watch itself stays — only the stale due mark is consumed")
			require.False(t, entry.due, "a state-change refusal consumes the due mark")
			if name == "another attention request owns the row" {
				require.Equal(t, "the agent's own question", inst.Attention().Reason,
					"the flag must never overwrite what the agent asked")
			} else {
				require.False(t, inst.HasAttention(), "a moved condition must not flag")
			}
		})
	}
}

// The deferred flush must never block the poll on the op lock: on contention
// it keeps the due mark for a later tick — the same TryLock rule
// settlement.go's poll-side write follows (#7547 independent review).
func TestStagedDraftFlushKeepsTheDueMarkUnderOpLockContention(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	manager, repoID, _, inst := stagedDraftFixture(t, attemptAt)
	key := stagedDraftKey(repoID, inst)
	manager.pausedMu.Lock()
	manager.stagedDraftWatches[key] = &stagedDraftWatch{
		attemptAt: attemptAt, since: time.Now().Add(-2 * stagedDraftSettleWindow), due: true, dueWhy: "test"}
	manager.pausedMu.Unlock()

	opLock := manager.opLockFor(daemonInstanceKey(repoID, inst.Title))
	opLock.Lock()
	flushDone := make(chan struct{})
	go func() {
		manager.raiseDueStagedDraftFlag(repoID, key, inst)
		close(flushDone)
	}()
	select {
	case <-flushDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the deferred flush blocked on the contended op lock")
	}
	opLock.Unlock()
	entry := stagedDraftWatchEntry(manager, key)
	require.True(t, entry.due, "a contended flush keeps the due mark for a later tick")
	require.False(t, inst.HasAttention(), "nothing may be raised while the op lock is contended")

	// The next flush — the lock now free — lands the retained due mark.
	manager.raiseDueStagedDraftFlag(repoID, key, inst)
	require.True(t, inst.HasAttention(), "the kept due mark lands on the next uncontended flush")
}

// A blind read that precedes the first staged verdict must not count toward
// the sustain: the window opens on the first PROVABLE staged read, so a send
// rechecked once during a redraw and once staged still waits the full window
// (Codex #7547, third round — a blind-owned entry carried a zero since and
// flagged a fresh send in seconds).
func TestStagedDraftWatchOpensTheWindowOnTheFirstStagedRead(t *testing.T) {
	base := time.Now()
	now := base
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })

	manager, repoID, _, inst := stagedDraftFixture(t, base.Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{
		session.StagedDraftUnreadable, session.StagedDraftStaged, session.StagedDraftStaged}}
	key := stagedDraftKey(repoID, inst)

	feedStagedDraftTick(manager, repoID, key, inst, checker)
	now = base.Add(time.Hour) // far past any window — only the blind read ran so far
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.False(t, inst.HasAttention(),
		"the window opens on the first staged verdict, not the blind reads before it")

	now = now.Add(stagedDraftSettleWindow + time.Second)
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, inst.HasAttention(), "a full sustained window after the first staged read flags")
}

// Blind reads are never a negative — but they are not free either: a pane
// unjudgeable for stagedDraftMaxBlindReads consecutive ticks retires the
// attempt's watch rather than capturing per poll forever (#7547 independent
// review).
func TestStagedDraftWatchRetiresAfterBoundBlindReads(t *testing.T) {
	manager, repoID, _, inst := stagedDraftFixture(t, time.Now().Add(-time.Minute))
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftUnreadable}}
	key := stagedDraftKey(repoID, inst)

	for i := 0; i < stagedDraftMaxBlindReads; i++ {
		feedStagedDraftTick(manager, repoID, key, inst, checker)
	}
	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry)
	require.False(t, entry.closed, "the budget is not yet exhausted")

	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.True(t, stagedDraftWatchEntry(manager, key).closed, "the blind budget retires the attempt")

	calls := checker.captureCount()
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	require.Equal(t, calls, checker.captureCount(), "a retired attempt is never captured for again")
	require.False(t, inst.HasAttention())

	// A newer send re-arms a fresh watch — the retired attempt's budget does
	// not follow it.
	second := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, second))
	checker.reads = []session.StagedDraftRead{session.StagedDraftStaged}
	feedStagedDraftTick(manager, repoID, key, inst, checker)
	entry = stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry)
	require.True(t, entry.attemptAt.Equal(second), "the newer send owns the watch")
	require.Zero(t, entry.blind, "a provable read leaves no blind streak")
	require.False(t, entry.closed)
}

// The same deadlock, proven through the real poll tick rather than the
// feed's entry points (the test above exercises those directly): with an
// automated send parked between the session op lock and the observation
// fence, refreshInstanceStatus must still complete — the under-fence feed
// only records the flag, and the deferred flush TryLocks rather than
// parking the sweep behind the send (#7547 independent review). Moving the
// deferred-raise defer below the fence's release — the regression this
// guards — makes the poll tick deadlock again and this test goes red.
func TestStagedDraftPollTickNeverBlocksBehindAnOpLockSend(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	checker := &fakeStagedChecker{reads: []session.StagedDraftRead{session.StagedDraftStaged}}
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "worker",
		stagedReadingBackend{readyFakeBackend{session.NewFakeBackend()}, checker}, true, session.Ready)
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, attemptAt))
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

	// The real poll tick must complete while the send holds the op lock: the
	// feed records the due flag under the fence, and the deferred flush
	// TryLocks rather than blocking the sweep.
	pollDone := make(chan struct{})
	go func() {
		manager.refreshInstanceStatus(repoID, inst)
		close(pollDone)
	}()
	select {
	case <-pollDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the poll tick blocked on the send's op lock — the deferred flag must not block")
	}
	entry := stagedDraftWatchEntry(manager, key)
	require.NotNil(t, entry)
	require.True(t, entry.due, "the contended flush keeps the due mark for a later tick")
	require.False(t, inst.HasAttention(), "nothing may be raised while the op lock is contended")

	close(releaseSend)
	select {
	case <-sendDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the automated send stayed blocked after the fence was free")
	}

	// The send superseded the due mark's attempt: the next poll tick drops
	// the open watch rather than flagging a stale send.
	pollDone = make(chan struct{})
	go func() {
		manager.refreshInstanceStatus(repoID, inst)
		close(pollDone)
	}()
	select {
	case <-pollDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the post-send poll tick blocked")
	}
	require.False(t, inst.HasAttention(), "the send superseded the due mark — no flag for a stale send")
	require.Nil(t, stagedDraftWatchEntry(manager, key), "a delivered send's open watch is dropped")

	// And the deferred raise is still the working path through the real tick:
	// re-qualify and the flag lands once the op lock is free.
	second := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, second))
	manager.pausedMu.Lock()
	manager.stagedDraftWatches[key] = &stagedDraftWatch{
		attemptAt: second, since: time.Now().Add(-2 * stagedDraftSettleWindow)}
	manager.pausedMu.Unlock()
	manager.refreshInstanceStatus(repoID, inst)
	require.True(t, inst.HasAttention(), "the deferred raise lands the flag through the real poll tick")
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
