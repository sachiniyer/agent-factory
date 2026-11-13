package daemon

import (
	"fmt"
	stdlog "log"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7120 follow-persist suite. The follow commits its move to memory before
// it persists, and the warn-only best-effort pattern is only sound when some
// channel re-attempts the write — for this one none does (its own re-read
// judges Unchanged, and the poll's pending-write gate is never armed). These
// tests pin the rollback that restores self-healing: on a failed persist the
// in-memory record returns to what disk holds, so the next follow check
// re-detects the runtime/thread mismatch and retries.

// conversationBeingPersisted is the agent conversation a whole-row persist
// carries, read from the daemon side of the InstanceData projection the way
// noteConversationWritten does. Tests target one exact write with it.
func conversationBeingPersisted(data session.InstanceData) session.AgentConversationData {
	if len(data.Tabs) > 0 && data.Tabs[0].Conversation != nil {
		return *data.Tabs[0].Conversation
	}
	if data.AgentConversation != nil {
		return *data.AgentConversation
	}
	return session.AgentConversationData{}
}

// TestFollowPersistFailureRollsBackMemoryToDiskAndRetriesNextCheck is the
// headline: thread A is recorded and durable, the runtime moves to thread B,
// and the follow's persist of B fails. Memory must roll back to A (matching
// disk) so the next follow check re-detects the move and retries — the
// self-healing the archive.go best-effort contract presumes, which the
// warn-only handling broke. Three follow checks are driven: the first two
// fail the persist (and each must re-propose, which is impossible pre-fix where
// memory stayed at B and the loop judged Unchanged), the third succeeds once
// the transient failure clears.
func TestFollowPersistFailureRollsBackMemoryToDiskAndRetriesNextCheck(t *testing.T) {
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()

	// Thread A is recorded and durable; the runtime holds only A.
	first := writeHeldTestRollout(t, codexHome, heldMissionThreadID, dir)
	held := &heldOnly{}
	held.set(first)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	recorded := session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	}
	inst.SetAgentConversation(recorded)
	// A reaches disk before the hook is installed, so only the follow's write
	// of the moved-to thread is ever faulted.
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	require.Equal(t, heldMissionThreadID, persistedConversationID(t, repoID))

	// The pane starts a new thread; the runtime now holds only B.
	second := writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held.set(second)
	snap := inst.SessionCodexCaptureScope()
	token := inst.AgentRuntimeToken()

	var mu sync.Mutex
	persistAttempts := 0
	failMoveToB := true
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		conv := conversationBeingPersisted(data)
		mu.Lock()
		if conv.ID == heldUserThreadID {
			persistAttempts++
		}
		fail := failMoveToB && conv.ID == heldUserThreadID
		mu.Unlock()
		if fail {
			return fmt.Errorf("simulated transient fsync failure")
		}
		return nil
	}

	// First follow: commits B to memory, the persist fails, and the in-memory
	// record must roll back to A so memory and disk agree again.
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	require.Equal(t, heldMissionThreadID, inst.AgentConversation().ID,
		"on persist failure the in-memory record must roll back to what disk holds")
	require.Equal(t, heldMissionThreadID, persistedConversationID(t, repoID),
		"a failed write must leave the on-disk record untouched")
	require.False(t, inst.ConversationUnpersisted(),
		"the follow does not arm the poll's pending-write gate; the rollback is the retry channel")
	require.False(t, conversationNeedsWrite(inst),
		"the poll's whole-row retry gate stays closed for this write")
	mu.Lock()
	require.Equal(t, 1, persistAttempts, "the first follow proposed exactly one write")
	mu.Unlock()

	// Second follow, still failing: it must re-propose (the rollback made A
	// recorded while the runtime holds B), where pre-fix memory stayed at B and
	// the loop judged Unchanged — making zero attempts and diverging forever.
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	require.Equal(t, heldMissionThreadID, inst.AgentConversation().ID,
		"a repeated persist failure must roll the record back again")
	require.Equal(t, heldMissionThreadID, persistedConversationID(t, repoID))
	mu.Lock()
	require.Equal(t, 2, persistAttempts, "the follow re-proposes after a rolled-back failed persist")
	mu.Unlock()

	// The transient failure clears; the third follow re-detects the move and
	// the retry lands the moved thread on both memory and disk.
	mu.Lock()
	failMoveToB = false
	mu.Unlock()
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	require.Equal(t, heldUserThreadID, inst.AgentConversation().ID,
		"once the write succeeds the retry commits the move to memory")
	require.Equal(t, heldUserThreadID, persistedConversationID(t, repoID),
		"once the write succeeds the retry lands the moved thread on disk")
	mu.Lock()
	require.Equal(t, 3, persistAttempts, "the successful retry re-proposed exactly one write")
	mu.Unlock()
}

// TestFollowPersistFailureRollsBackAnUnstampedOperatorRepairBaseline covers the
// baselined outcome, whose record keeps the Operator's id but gains a baseline
// stamp of the runtime's current thread. A failed persist must roll the whole
// record — id AND stamp — back to the unstamped repair on disk, so the next
// follow re-baselines and retries. Pinning the stamp's rollback guards against
// a fix that only restores the id.
func TestFollowPersistFailureRollsBackAnUnstampedOperatorRepairBaseline(t *testing.T) {
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	// The runtime holds a fresh thread F.
	fresh := writeHeldTestRollout(t, codexHome, heldFreshThreadID, dir)
	held := &heldOnly{}
	held.set(fresh)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)

	// An operator repair for R that reached disk with no observation (the held
	// query could not answer, or the record predates the stamp).
	recorded := session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldUserThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureOperator,
	}
	inst.SetAgentConversation(recorded)
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	require.False(t, persistedAgentConversation(t, repoID).RepairStamped, "fixture: the on-disk repair carries no stamp")

	snap := inst.SessionCodexCaptureScope()
	token := inst.AgentRuntimeToken()

	var mu sync.Mutex
	persistAttempts := 0
	failBaseline := true
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		conv := conversationBeingPersisted(data)
		mu.Lock()
		if conv.RepairStamped && !conv.RepairHeldObserved {
			persistAttempts++
		}
		fail := failBaseline && conv.RepairStamped && !conv.RepairHeldObserved
		mu.Unlock()
		if fail {
			return fmt.Errorf("simulated transient fsync failure")
		}
		return nil
	}

	// First follow: baselines F onto the repair, the persist fails, and the
	// whole record (id and stamp) must roll back to disk's unstamped repair.
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	rolled := inst.AgentConversation()
	require.Equal(t, heldUserThreadID, rolled.ID, "the operator's recorded id stands")
	require.False(t, rolled.RepairStamped, "the baseline stamp must roll back with the id")
	require.Empty(t, rolled.RepairHeldID, "the baseline's held id must roll back too")
	require.False(t, inst.ConversationUnpersisted(), "the poll's pending-write gate stays closed")
	stored := persistedAgentConversation(t, repoID)
	require.False(t, stored.RepairStamped, "disk keeps the unstamped repair")
	require.Equal(t, heldUserThreadID, stored.ID)
	mu.Lock()
	require.Equal(t, 1, persistAttempts)
	mu.Unlock()

	// Second follow, still failing: re-baselines from the rolled-back unstamped
	// repair and retries (impossible pre-fix, where memory kept the stamp).
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	require.False(t, inst.AgentConversation().RepairStamped, "a repeated failure rolls the stamp back again")
	mu.Lock()
	require.Equal(t, 2, persistAttempts, "the follow re-baselines after a rolled-back failed persist")
	mu.Unlock()

	// The transient failure clears; the retry lands the baseline on disk.
	mu.Lock()
	failBaseline = false
	mu.Unlock()
	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	require.Equal(t, heldUserThreadID, inst.AgentConversation().ID, "the operator's id is unchanged by the baseline")
	require.True(t, inst.AgentConversation().RepairStamped, "the retry commits the baseline stamp to memory")
	durable := persistedAgentConversation(t, repoID)
	require.Equal(t, heldUserThreadID, durable.ID)
	require.True(t, durable.RepairStamped, "the retry lands the baseline stamp on disk")
	require.Equal(t, heldFreshThreadID, durable.RepairHeldID)
	require.False(t, durable.RepairHeldObserved, "a baseline is taken after the repair, not by it (#7129)")
	mu.Lock()
	require.Equal(t, 3, persistAttempts)
	mu.Unlock()
}

// TestFollowPersistsTheMovedThreadAndDoesNotRollBackWhenTheWriteSucceeds is the
// no-regression guard: the rollback is only for a failed persist. A successful
// follow write moves the record on both memory and disk, exactly as it did
// before the fix, and arms no pending-write flag.
func TestFollowPersistsTheMovedThreadAndDoesNotRollBackWhenTheWriteSucceeds(t *testing.T) {
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	first := writeHeldTestRollout(t, codexHome, heldMissionThreadID, dir)
	held := &heldOnly{}
	held.set(first)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	recorded := session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	}
	inst.SetAgentConversation(recorded)
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))

	second := writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held.set(second)
	snap := inst.SessionCodexCaptureScope()
	token := inst.AgentRuntimeToken()

	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	require.Equal(t, heldUserThreadID, inst.AgentConversation().ID,
		"a successful follow commits the move to memory")
	require.Equal(t, heldUserThreadID, persistedConversationID(t, repoID),
		"a successful follow lands the moved thread on disk")
	require.False(t, inst.ConversationUnpersisted(),
		"a successful follow still arms no pending-write flag — the poll never owned this write")
}

// parkedLogSink is a Manager log sink that, once armed closes, holds each
// write until released closes (bounded, so a broken test fails rather than
// hangs). The follow writes its log line only after it has released the start
// lock, so parking that line hands the lock to a writer queued behind it before
// the follow can do anything further: the interleaving the #7254 review named,
// made deterministic instead of left to the scheduler.
type parkedLogSink struct {
	logs     logCapture
	armed    <-chan struct{}
	released <-chan struct{}
}

func (s *parkedLogSink) Write(p []byte) (int, error) {
	select {
	case <-s.armed:
		select {
		case <-s.released:
		case <-time.After(10 * time.Second):
		}
	default:
	}
	return s.logs.Write(p)
}

// TestFollowPersistFailureRollsBackBeforeAQueuedWriterCanPersistTheMove is the
// #7254 review's interleaving. The follow persists under the repository start
// lock, and every whole-row writer (persistInstanceErr, the polls) snapshots
// the row under that same lock. A rollback made after the unlock leaves a
// window in which a writer queued on the lock snapshots the moved, unsaved id
// and persists it successfully; the rollback then puts memory back on A over a
// disk that says B — the divergence the rollback exists to prevent, in the
// other direction. Here a whole-row writer is queued on the start lock while
// the follow's persist fails, and it must snapshot what disk holds.
func TestFollowPersistFailureRollsBackBeforeAQueuedWriterCanPersistTheMove(t *testing.T) {
	armed := make(chan struct{})
	queuedSnapshotted := make(chan struct{})
	logger := stdlog.New(&parkedLogSink{armed: armed, released: queuedSnapshotted}, "", 0)
	manager, inst, repoID, key, codexHome := codexLoopTestInstanceWithOptions(t, managerOptions{warnLog: logger, infoLog: logger})
	dir := inst.GetWorktreePath()

	// Thread A is recorded and durable; the runtime then moves to B.
	first := writeHeldTestRollout(t, codexHome, heldMissionThreadID, dir)
	held := &heldOnly{}
	held.set(first)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	})
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	second := writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held.set(second)
	snap := inst.SessionCodexCaptureScope()
	token := inst.AgentRuntimeToken()

	var mu sync.Mutex
	calls := 0
	var followWrote, queuedWrote session.AgentConversationData
	queuedErr := make(chan error, 1)
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			// The follow's write, made while it holds the start lock: queue
			// a whole-row writer on that lock, then fail the write.
			mu.Lock()
			followWrote = conversationBeingPersisted(data)
			mu.Unlock()
			close(armed)
			go func() { queuedErr <- manager.persistInstanceErr(repoID, inst) }()
			return fmt.Errorf("simulated transient fsync failure")
		case 2:
			// The queued writer's snapshot, taken once it holds the lock.
			mu.Lock()
			queuedWrote = conversationBeingPersisted(data)
			mu.Unlock()
			close(queuedSnapshotted)
		}
		return nil
	}

	require.NoError(t, manager.followAgentConversation(repoID, key, inst, snap, token, inst.AgentConversation()))
	select {
	case err := <-queuedErr:
		require.NoError(t, err, "the queued whole-row writer's own write succeeds")
	case <-time.After(30 * time.Second):
		t.Fatal("the writer queued on the start lock never ran")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, heldUserThreadID, followWrote.ID, "fixture: the faulted write is the follow's move to B")
	require.Equal(t, heldMissionThreadID, queuedWrote.ID,
		"a writer queued on the start lock behind the follow's failed persist must not snapshot the moved, unsaved id")
	require.Equal(t, heldMissionThreadID, inst.AgentConversation().ID, "memory rolls back to A")
	require.Equal(t, inst.AgentConversation().ID, persistedConversationID(t, repoID),
		"memory and disk agree once both writers are done")
}

// TestFollowPersistFailureLogsTheMoveAsSeenNotRecorded pins what the daemon
// says when the follow's write fails: the move was seen, it was not saved, and
// the next follow check retries it. It must not claim, as the success line
// does, that the thread was recorded.
func TestFollowPersistFailureLogsTheMoveAsSeenNotRecorded(t *testing.T) {
	logs := &logCapture{}
	logger := stdlog.New(logs, "", 0)
	manager, inst, repoID, key, codexHome := codexLoopTestInstanceWithOptions(t, managerOptions{warnLog: logger, infoLog: logger})
	dir := inst.GetWorktreePath()

	first := writeHeldTestRollout(t, codexHome, heldMissionThreadID, dir)
	held := &heldOnly{}
	held.set(first)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldMissionThreadID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureCodexRollout,
	})
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	second := writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held.set(second)

	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if conversationBeingPersisted(data).ID == heldUserThreadID {
			return fmt.Errorf("simulated transient fsync failure")
		}
		return nil
	}

	require.NoError(t, manager.followAgentConversation(repoID, key, inst, inst.SessionCodexCaptureScope(), inst.AgentRuntimeToken(), inst.AgentConversation()))
	logged := logs.String()
	require.NotContains(t, logged, "recorded the thread", "a failed write must not be logged as recorded")
	require.Contains(t, logged, "moved from conversation "+heldMissionThreadID+" to "+heldUserThreadID, "the log names the move that was seen")
	require.Contains(t, logged, "could not be saved", "the log says the move was not saved")
	require.Contains(t, logged, "simulated transient fsync failure", "the log carries the write's error")
	require.Contains(t, logged, "next follow check", "the log says when it is retried")
}
