package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestPersistPollChange_SettlementCheckpointCededToInFlightOpIsRetried asserts
// that when a poll consumes the one-shot settlementCheckpoint (via
// ClearLostRestoreFailureAtObservation) but then cedes to an in-flight op in
// persistPollChangeWithIdleEvidence, the obligation is preserved in settleOwed
// and subsequently flushed to disk by FlushOwedSettlements once the op clears.
//
// Without the fix (the "if settlementCheckpoint { recordSettlementWrite ... }"
// blocks in limit.go), the one-shot is spent in memory, every later poll
// computes ClearLostRestoreFailureAtObservation == false, and the cleared
// terminal failure never reaches disk — a restart reloads the old outage.
//
// Deterministic via testHookPollBeforePersistLock: the fence is raised by a
// direct BeginHandoff transition inside the hook, after the lock-free gate
// passes but before repoStartLock is taken. No goroutines or sleeps are needed
// because the hook fires synchronously in the poll goroutine.
func TestPersistPollChange_SettlementCheckpointCededToInFlightOpIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// FakeBackend: HasUpdated=(false,false,""), IsAlive=true — the idle probe
	// path that calls resolveIdleLiveness and eventually sets observedAlive.
	inst := registerStarted(t, manager, repoID, repoPath, "ceded-checkpoint", session.NewFakeBackend(), true, session.Running)

	// Arm the one-shot: record a terminal restore failure so
	// ClearLostRestoreFailureAtObservation can consume it.
	if !inst.SetLostRestoreFailure(3, errors.New("agent exited at startup")) {
		t.Fatal("SetLostRestoreFailure rejected: precondition failed")
	}
	// Persist the row WITH the LostRestoreFailure so disk and memory start
	// identical. After the test, disk must show it cleared.
	manager.persistInstance(repoID, inst)
	if rec := recordFor(t, repoID, "ceded-checkpoint"); rec == nil || rec.LostRestoreFailure == nil {
		t.Fatalf("seed record = %+v, want LostRestoreFailure set", rec)
	}

	// In the poll's pre-lock window, raise OpReplacing via BeginHandoff so the
	// under-lock re-check in persistPollChangeWithIdleEvidence cedes to the op.
	// This is the interleaving the fix covers: the one-shot is consumed (in
	// memory) and the write is deferred to settleOwed for retry.
	prev := testHookPollBeforePersistLock
	t.Cleanup(func() { testHookPollBeforePersistLock = prev })
	once := false
	testHookPollBeforePersistLock = func() {
		if once {
			return
		}
		once = true
		if err := inst.Transition(session.BeginHandoff()); err != nil {
			t.Errorf("BeginHandoff: %v", err)
		}
	}

	// Drive the full poll, which calls refreshInstanceStatus → SnapshotAgent →
	// (idle, probeAlive) → resolveIdleLiveness → noteAliveObservationAtGeneration
	// → ClearLostRestoreFailureAtObservation → settlementCheckpoint=true →
	// persistPollChangeWithIdleEvidence → hook raises OpReplacing → under-lock
	// cede → recordSettlementWrite(non-nil error) → settleOwed entry created.
	manager.refreshInstanceStatus(repoID, inst)

	// Verify the one-shot was consumed: lostRestoreFailure is clear in memory.
	if inst.LostRestoreFailureSnapshot() != nil {
		t.Error("in-memory LostRestoreFailure must be cleared by the poll's ClearLostRestoreFailureAtObservation call")
	}

	// Verify the obligation is owed: the write did not reach disk.
	if mid := recordFor(t, repoID, "ceded-checkpoint"); mid == nil || mid.LostRestoreFailure == nil {
		t.Fatalf("record after the ceded poll = %+v, want LostRestoreFailure still on disk (the write was ceded, not made)", mid)
	}

	// Verify the obligation is tracked in settleOwed.
	manager.mu.Lock()
	_, owed := manager.settleOwed[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	if !owed {
		t.Error("settleOwed must contain the session after a ceded settlementCheckpoint write")
	}

	// The op finishes (here: aborted). The fence is now down.
	if err := inst.Transition(session.AbortHandoff()); err != nil {
		t.Fatalf("AbortHandoff: %v", err)
	}
	if got := inst.GetInFlightOp(); got != session.OpNone {
		t.Fatalf("in-memory InFlightOp after AbortHandoff = %v, want OpNone", got)
	}

	// The retry must now make the cleared LostRestoreFailure durable.
	manager.FlushOwedSettlements()

	final := recordFor(t, repoID, "ceded-checkpoint")
	if final == nil {
		t.Fatal("no record on disk after FlushOwedSettlements")
	}
	if final.LostRestoreFailure != nil {
		t.Errorf("on-disk LostRestoreFailure = %+v after FlushOwedSettlements, want nil: "+
			"the ceded write obligation must have been retried and made durable", final.LostRestoreFailure)
	}
}

// TestFlushOwedSettlements_HoldsOffWhileOpStillInFlight asserts that a
// settlement retry attempted while the session is still inside an operation
// leaves the obligation owed rather than discharging it prematurely.
// flushOneOwedSettlement returns nil (not an error) in this situation, and the
// loop's delete is guarded by !registered, so the obligation stays live for the
// next tick.
func TestFlushOwedSettlements_HoldsOffWhileOpStillInFlight(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "owed-in-flight", session.NewFakeBackend(), true, session.Running)

	if !inst.SetLostRestoreFailure(1, errors.New("startup failed")) {
		t.Fatal("SetLostRestoreFailure rejected")
	}
	manager.persistInstance(repoID, inst)

	// Raise the fence and manually enroll a settlement obligation, mirroring
	// what persistPollChangeWithIdleEvidence does on the cede path.
	if err := inst.Transition(session.BeginHandoff()); err != nil {
		t.Fatalf("BeginHandoff: %v", err)
	}
	// Clear in memory (as the poll would), then enroll the retry.
	inst.ClearLostRestoreFailure()
	key := daemonInstanceKey(repoID, inst.Title)
	manager.recordSettlementWrite(repoID, key, inst, errors.New("ceded"))

	// Disk still shows the old row; the op is in flight.
	manager.FlushOwedSettlements()
	if mid := recordFor(t, repoID, "owed-in-flight"); mid == nil || mid.LostRestoreFailure == nil {
		t.Fatalf("record after flush-while-busy = %+v, want LostRestoreFailure still on disk (the retry must not write while the op is in flight)", mid)
	}

	// Verify the obligation is still owed (the flush did not discharge it).
	manager.mu.Lock()
	_, stillOwed := manager.settleOwed[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	if !stillOwed {
		t.Error("settleOwed must still contain the session after a retry that could not run")
	}

	// Now the op clears; the retry succeeds.
	if err := inst.Transition(session.AbortHandoff()); err != nil {
		t.Fatalf("AbortHandoff: %v", err)
	}
	manager.FlushOwedSettlements()

	final := recordFor(t, repoID, "owed-in-flight")
	if final == nil || final.LostRestoreFailure != nil {
		t.Errorf("on-disk LostRestoreFailure = %+v after second flush, want nil", final.LostRestoreFailure)
	}
}

// limitRacePollBackend is a FakeBackend that reproduces the #2135 interleaving
// exactly: its pane capture returns a fixed usage-limit banner every idle tick,
// and its following liveness probe runs a one-shot hook AFTER the content has
// been captured but BEFORE the poll acts on it. The hook is where the racing
// resume lands, so the poll's decision is provably made from PRE-resume content.
//
// Driving the race from the following probe (rather than from a second goroutine)
// makes it deterministic without re-entering prompt delivery while Snapshot holds
// its same-runtime observation lock: the poll goroutine runs the resume after
// Snapshot returns, so there is no sleep, barrier, or deadlock.
type limitRacePollBackend struct {
	*session.FakeBackend
	mu           sync.Mutex
	content      string
	afterCapture func()
	sentPrompts  []string
}

func (b *limitRacePollBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	b.mu.Lock()
	content := b.content
	b.mu.Unlock()
	return false, false, content
}

func (b *limitRacePollBackend) IsAlive(*session.Instance) (bool, error) {
	b.mu.Lock()
	hook := b.afterCapture
	b.afterCapture = nil // one-shot: later ticks are ordinary polls
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return true, nil
}

func (b *limitRacePollBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sentPrompts = append(b.sentPrompts, prompt)
	return nil
}

func (b *limitRacePollBackend) prompts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sentPrompts...)
}

func (b *limitRacePollBackend) setHook(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.afterCapture = fn
}

// TestResumeFromLimit_ConcurrentPollCannotRevertResume is the #2135 regression: a
// poll tick that captured its pane content BEFORE a resume must not re-park the
// session at the usage-limit wall using that stale content.
//
// The interleaving: the poll snapshots a pane still showing the limit banner, the
// resume then clears the limit, re-delivers the prompt and persists LiveRunning,
// and only then does the poll run the detector over its stale capture. Before the
// fix that detector hit and called SetLimitReached, reverting the session to
// LiveLimitReached in memory; persistPollChange then flushed it to DISK too,
// because the reset time had changed even though the liveness compare read
// unchanged (LimitReached → LimitReached). The user was shown a limit-blocked
// session that was in fact working, and a daemon restart reloaded the wrong state.
func TestResumeFromLimit_ConcurrentPollCannotRevertResume(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "finish the migration"

	// Park it at the wall with a reset time that differs from the one the banner
	// parses to, so the poll's re-detection changes the reset time — the exact
	// shape that made persistPollChange flush the reverted state to disk.
	inst.SetLimitReached(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	manager.persistInstance(repoID, inst)

	// The resume lands between the poll's capture and the poll's decision.
	backend.setHook(func() {
		if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "limited", RepoID: repoID}); err != nil {
			t.Errorf("resumeFromLimit: %v", err)
		}
	})

	manager.RefreshStatuses()

	if got := backend.prompts(); len(got) != 1 || got[0] != "finish the migration" {
		t.Fatalf("delivered prompts = %v, want exactly the stored prompt (the resume must have landed)", got)
	}
	if inst.LimitReached() {
		t.Errorf("in-memory liveness = %v, want LiveRunning: a poll deciding from PRE-resume content must not re-park the session (#2135)", inst.GetLiveness())
	}
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Errorf("in-memory liveness = %v, want LiveRunning (#2135)", got)
	}
	if got, ok := inst.LimitResetAt(); ok || !got.IsZero() {
		t.Errorf("in-memory reset time = (%v, %v), want (zero, false) after a successful resume (#2135)", got, ok)
	}
	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveRunning {
		t.Errorf("persisted liveness = %v, want LiveRunning: the stale poll must not reach disk (#2135)", got)
	}
	if got := persistedLimitReset(t, repoID, "limited"); !got.IsZero() {
		t.Errorf("persisted reset time = %v, want zero (#2135)", got)
	}
}

// TestRefreshStatuses_GenuineLimitHitAfterResumeStillParks is the
// over-suppression control for #2135: the fix drops only a decision made from
// content captured before a transition, never limit detection in general. A LATER
// tick — fresh capture, no racing resume — that still sees the banner must park
// the session again, in memory and on disk.
//
// This is what rules out a "suppress limit detection for N seconds after a
// resume" fix: an agent that immediately walks back into the wall would go
// undetected for the whole window.
func TestRefreshStatuses_GenuineLimitHitAfterResumeStillParks(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "finish the migration"
	inst.SetLimitReached(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	manager.persistInstance(repoID, inst)

	backend.setHook(func() {
		if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "limited", RepoID: repoID}); err != nil {
			t.Errorf("resumeFromLimit: %v", err)
		}
	})
	manager.RefreshStatuses()
	if inst.LimitReached() {
		t.Fatalf("precondition: the resumed session must not be limit-blocked, got %v", inst.GetLiveness())
	}

	// Next tick: no resume in flight, the pane still shows the banner. This is a
	// genuine observation and must park the session.
	manager.RefreshStatuses()

	if !inst.LimitReached() {
		t.Fatalf("in-memory liveness = %v, want LiveLimitReached: a fresh limit observation must still park the session", inst.GetLiveness())
	}
	if _, ok := inst.LimitResetAt(); !ok {
		t.Error("a parseable reset time must be stored for the badge")
	}
	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveLimitReached {
		t.Errorf("persisted liveness = %v, want LiveLimitReached", got)
	}
}

// TestPersistPollChange_ResumeDuringWriteWindowIsNotOverwritten is the second
// half of #2135, and it survives the first: even when the poll's limit detection
// is GENUINE — fresh content, nothing stale about it — the write itself happens
// later, after a repo start lock a session create can hold for seconds. A resume
// landing in that window cleared the limit and persisted LiveRunning; the poll
// then flushed the payload it had decided from and put the limit-blocked row back
// on disk, where a daemon restart would reload it.
//
// The poll must persist what is TRUE at write time, not the intermediate it
// decided from.
func TestPersistPollChange_ResumeDuringWriteWindowIsNotOverwritten(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "finish the migration"

	// The poll's own decision, made from content that is genuinely current: park
	// the session at the wall. This is what it is about to write.
	before := inst.GetLiveness()
	beforeReset, _ := inst.LimitResetAt()
	inst.SetLimitReached(time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC))

	// The resume lands in the write window: after the poll read its payload, before
	// it takes the repo start lock.
	prev := testHookPollBeforePersistLock
	t.Cleanup(func() { testHookPollBeforePersistLock = prev })
	once := false
	testHookPollBeforePersistLock = func() {
		if once {
			return
		}
		once = true
		if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "limited", RepoID: repoID}); err != nil {
			t.Errorf("resumeFromLimit: %v", err)
		}
	}

	manager.persistPollChange(repoID, inst, before, beforeReset, false)

	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveRunning {
		t.Errorf("persisted liveness = %v, want LiveRunning: the poll must not overwrite a resume that landed while it waited for the write lock (#2135)", got)
	}
	if got := persistedLimitReset(t, repoID, "limited"); !got.IsZero() {
		t.Errorf("persisted reset time = %v, want zero (#2135)", got)
	}
}

// TestRefreshStatuses_OrdinaryPollUnaffectedByEpochGuard is the plain-path
// control: an idle session with no limit banner and nothing racing it still
// settles Ready and still persists that transition. The #2135 guard must be
// invisible to the ordinary poll.
func TestRefreshStatuses_OrdinaryPollUnaffectedByEpochGuard(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: "$ "}
	inst := registerStarted(t, manager, repoID, repoPath, "idle", backend, true, session.Running)

	manager.RefreshStatuses()

	if got := inst.GetLiveness(); got != session.LiveReady {
		t.Fatalf("in-memory liveness = %v, want LiveReady", got)
	}
	if got := persistedLiveness(t, repoID, "idle"); got != session.LiveReady {
		t.Fatalf("persisted liveness = %v, want LiveReady", got)
	}
}

// TestPersistPollChange_HandoffSwapFailureDuringWriteWindowIsNotPersistedAsSettled
// is the regression for the poll/handoff persist race. persistPollChangeWithIdleEvidence
// checked InFlightOp only BEFORE taking repoStartLock and never re-checked after the
// under-lock re-read, so a handoff that started between that lock-free gate and the
// re-read could have its mid-OpReplacing snapshot persisted: RecordHandoffSwap had
// already rewritten Program to the incoming agent, the mission marker was not set
// yet, and ForStorage strips the transient op axis — storing the incoming agent as
// SETTLED with no delivery obligation. HandoffSession holds the per-(repo,title) and
// op locks (neither is repoStartLock), so the poll could re-read that snapshot under
// repoStartLock while the handoff was mid-transaction. The swapErr rollback reverts
// memory only and persists nothing, so an unclean exit before the next durable write
// would reload the incoming agent with no mission. The fix re-checks InFlightOp
// under the lock and cedes to the op's executor.
//
// Deterministic via the testHookPollBeforePersistLock seam: the handoff is started
// in a goroutine that parks inside SwapAgent (blockingNthSwapBackend) AFTER
// RecordHandoffSwap rewrote Program, and the poll re-reads while it is parked. No
// sleeps: channels order the poll goroutine against the handoff goroutine, and the
// poll holds repoStartLock while the handoff holds lockTarget + opLockFor, so the
// re-read provably lands inside the mid-OpReplacing window.
func TestPersistPollChange_HandoffSwapFailureDuringWriteWindowIsNotPersistedAsSettled(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend(), swapErr: errors.New("account-scoped session refuses handoff")}
	backend := &blockingNthSwapBackend{
		handoffBackend: base,
		blockAt:        1,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "handoff-poll-race", backend)

	// The poll's own decision, made from current state: a liveness transition so
	// durableChanged is true and the lock-free gate lets persistPollChange through
	// to the hook. Project the outgoing (claude) row onto disk first so the
	// assertion can tell "the poll wrote the incoming agent" apart from "nothing
	// was seeded".
	before := inst.GetLiveness()
	beforeReset, _ := inst.LimitResetAt()
	inst.SetLimitReached(time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC))
	manager.persistInstance(repoID, inst)
	if seed := recordFor(t, repoID, "handoff-poll-race"); seed == nil || seed.Program != tmux.ProgramClaude {
		t.Fatalf("seed record = %+v, want the outgoing claude row the poll would overwrite", seed)
	}

	// The handoff lands in the poll's pre-lock window. It runs in a goroutine and
	// parks inside SwapAgent AFTER RecordHandoffSwap rewrote Program to gemini, so
	// the poll's under-lock re-read observes a mid-OpReplacing snapshot. The poll
	// takes repoStartLock; the handoff holds lockTarget + opLockFor — different
	// locks — so the poll can re-read while the handoff is parked.
	prev := testHookPollBeforePersistLock
	t.Cleanup(func() { testHookPollBeforePersistLock = prev })
	handoffDone := make(chan error, 1)
	once := false
	testHookPollBeforePersistLock = func() {
		if once {
			return
		}
		once = true
		go func() {
			_, err := manager.HandoffSession(HandoffSessionRequest{
				Title: "handoff-poll-race", RepoID: repoID, To: tmux.ProgramGemini,
			})
			handoffDone <- err
		}()
		<-backend.entered
	}

	manager.persistPollChange(repoID, inst, before, beforeReset, false)

	// The handoff is still parked inside SwapAgent: the poll has just re-read the
	// mid-OpReplacing snapshot. Prove the race window was actually reached, so the
	// assertion below is not exercising a no-op.
	if got := inst.AgentProgram(); got != tmux.ProgramGemini {
		t.Fatalf("in-memory Program during the write window = %q, want %q: the handoff must have rewritten Program before the poll re-read (otherwise this test exercises nothing)", got, tmux.ProgramGemini)
	}
	if got := inst.GetInFlightOp(); got != session.OpReplacing {
		t.Fatalf("in-memory InFlightOp during the write window = %v, want OpReplacing: the handoff's fence must be up when the poll re-reads", got)
	}
	// The fix: the poll must not persist a mid-transaction row. The on-disk record
	// stays the outgoing claude row, NOT the incoming gemini snapshot ForStorage
	// would have flattened to a settled swap with no mission.
	mid := recordFor(t, repoID, "handoff-poll-race")
	if mid == nil {
		t.Fatal("no record on disk: the seed row vanished")
	}
	if mid.Program != tmux.ProgramClaude {
		t.Fatalf("the poll persisted a mid-OpReplacing snapshot as settled: on-disk Program = %q, want %q. "+
			"ForStorage strips InFlightOp to OpNone, so this row names the incoming agent with no delivery "+
			"obligation — a state the session never legitimately reached, and the swapErr rollback persists "+
			"nothing to repair it",
			mid.Program, tmux.ProgramClaude)
	}
	if mid.PendingHandoffMission != "" {
		t.Fatalf("on-disk PendingHandoffMission = %q, want empty: a mid-OpReplacing snapshot carries no mission yet", mid.PendingHandoffMission)
	}

	// Release the handoff; its SwapAgent returns the injected swapErr, so the
	// rollback reverts Program in memory and lowers the fence WITHOUT persisting.
	close(backend.release)
	if err := <-handoffDone; err == nil {
		t.Fatal("HandoffSession succeeded; want the injected swapErr so the rollback path is exercised")
	}
	if got := inst.AgentProgram(); got != tmux.ProgramClaude {
		t.Fatalf("in-memory Program after rollback = %q, want %q: RevertHandoff must restore the outgoing agent", got, tmux.ProgramClaude)
	}
	if got := inst.GetInFlightOp(); got != session.OpNone {
		t.Fatalf("in-memory InFlightOp after rollback = %v, want OpNone: AbortHandoff must lower the fence", got)
	}
	// Disk and memory agree on the outgoing agent: the corruption window is closed.
	if final := recordFor(t, repoID, "handoff-poll-race"); final == nil || final.Program != tmux.ProgramClaude {
		t.Fatalf("on-disk Program after rollback = %+v, want %q: the disk must not diverge from the reverted in-memory agent", final, tmux.ProgramClaude)
	}
}
