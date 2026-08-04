package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// failSettlementWrite fails exactly one persist for title: the next write that
// no longer carries a pending mission once an obligation is outstanding — which
// is the settlement write and nothing else. Writes before it land, and so does
// every write after it, including the poll's retry, so a test can tell "the
// settlement never became durable" apart from "nothing was written at all".
//
// armed says whether an obligation is outstanding when the hook is installed. A
// handoff installs it unarmed: the swap persists the row once BEFORE recording
// the mission, and that write is mission-free too. Seeing the post-swap
// checkpoint — the write that records the obligation — is what arms it. A test
// that installs the hook when the mission is already pending starts armed.
//
// Returns a func reporting whether the injected failure actually fired; an
// injection that never runs makes every assertion downstream of it decorative.
func failSettlementWrite(t *testing.T, title string, failure error, armed bool) func() bool {
	t.Helper()
	var mu sync.Mutex
	fired := false
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		mu.Lock()
		defer mu.Unlock()
		if fired || data.Title != title {
			return nil
		}
		if data.PendingHandoffMission != "" {
			armed = true
			return nil
		}
		if !armed {
			return nil
		}
		fired = true
		return failure
	}
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fired
	}
}

// reloadedHandoffRow materializes the row a restarted daemon would build from a
// record on disk, and registers it in place of the one this process held.
//
// A real load runs session.FromInstanceData, which carries the durable
// PendingHandoffMission across and reconstructs the OpReplacing fence from it
// (pinned by session.TestPendingHandoffMissionReconstructsDurableFence) — that
// reconstruction is precisely what authorizes the recovery pass to deliver. The
// loader itself cannot run here: for a local session it ends in Instance.Start
// against real tmux. So this rebuilds the same two durable facts and hands the
// row to the unmodified production recovery pass, which is where the duplicate
// delivery would happen.
func reloadedHandoffRow(t *testing.T, m *Manager, repoID, repoPath string, rec *session.InstanceData, backend session.Backend) *session.Instance {
	t.Helper()
	if rec == nil {
		t.Fatal("no record on disk to reload: the restarted daemon would have no session at all")
	}
	inst, err := session.NewInstance(session.InstanceOptions{Title: rec.Title, Path: repoPath, Program: rec.Program})
	if err != nil {
		t.Fatalf("NewInstance(reload): %v", err)
	}
	inst.SetBackend(backend)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	inst.SetTmuxSession(tmux.NewTmuxSession(rec.Title, rec.Program))
	if rec.PendingHandoffMission != "" {
		inst.SetPendingHandoffMission(rec.PendingHandoffMission)
		if err := inst.Transition(session.BeginHandoff()); err != nil {
			t.Fatalf("reconstruct replacement fence: %v", err)
		}
	}
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, rec.Title)] = inst
	m.mu.Unlock()
	return inst
}

// A handoff mission must be delivered EXACTLY once (#2781).
//
// The marker is written to disk BEFORE the readiness wait on purpose: if the
// daemon dies in that window, the restored row carries an explicit delivery
// obligation instead of silently claiming an instruction-less agent is fine. The
// settlement write after a confirmed delivery is the other half of that bargain —
// it is what retires the obligation.
//
// That write used persistInstance, whose failure is logged and dropped, so a
// failed settlement left the obligation standing over a mission the agent had
// already run. Nothing downstream repaired it: persistPollChange only writes on a
// liveness or reset-time change and never inspects PendingHandoffMission, and the
// whole-state shutdown checkpoint is exactly what an unclean exit skips. The next
// daemon rebuilt the replacement fence from the stale marker and sent the same
// brief again — the agent redoing work it had already done, on a branch its first
// run had already changed.
//
// Note which assertion carries the fix. Surfacing the error (the obvious change)
// makes the FIRST check pass while the mission still gets delivered twice; only
// retiring the obligation on disk stops the second delivery.
func TestHandoffSession_SettlementPersistFailureCannotRedeliverTheMission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "settle-once", backend)
	inst.SetPrompt("finish the half-applied migration")

	diskFull := errors.New("no space left on device")
	injected := failSettlementWrite(t, "settle-once", diskFull, false)

	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "settle-once", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if !injected() {
		t.Fatal("no settlement write was ever attempted, so this test exercised nothing")
	}
	if err == nil || !errors.Is(err, diskFull) {
		t.Fatalf("HandoffSession error = %v, want the settlement write failure surfaced (%v): "+
			"a settlement that never reached disk is not a completed handoff, and reporting success "+
			"hides an on-disk mission marker that outlives the mission it describes", err, diskFull)
	}

	_, prompts := backend.snapshot()
	if len(prompts) != 1 {
		t.Fatalf("delivered %d prompts during the handoff, want 1", len(prompts))
	}

	// The daemon's next poll. Its handoff pass owns this obligation, and a
	// settlement that did not land is the same job as a mission that did not:
	// finish it before anything can replay it.
	manager.ResumePendingHandoffs()

	rec := recordFor(t, repoID, "settle-once")
	if rec == nil || rec.PendingHandoffMission != "" {
		t.Fatalf("record after the poll = %+v, want no pending mission: the mission was delivered, "+
			"so leaving its obligation on disk is a standing instruction to deliver it again", rec)
	}

	// The unclean exit the marker exists for. Rebuild the row from disk exactly as
	// a restarted daemon would, then run the same recovery pass.
	reloadedHandoffRow(t, manager, repoID, repoPath, rec, backend)
	manager.ResumePendingHandoffs()

	_, prompts = backend.snapshot()
	if len(prompts) != 1 {
		t.Fatalf("mission delivered %d times (%q), want exactly 1: the settlement write failed, the "+
			"delivery obligation survived on disk, and the restarted daemon replayed a mission the "+
			"agent had already executed", len(prompts), prompts)
	}
}

// The same obligation, discharged by the recovery pass instead of the handoff
// itself (#2781). A mission that reaches its agent on a later poll settles
// through the identical marker-clearing write, so a failure there replays it for
// the identical reason — and this path has no RPC caller to report the failure
// to, which is exactly why the retry, not the error return, is what protects it.
func TestResumePendingHandoffs_SettlementPersistFailureCannotRedeliverTheMission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	sendErr := errors.New("paste transport failed")
	backend := &handoffBackend{FakeBackend: session.NewFakeBackend(), sendErr: sendErr}
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "settle-once-on-retry", backend)

	// Leave the mission pending behind the replacement fence: the paste fails, so
	// the handoff never settles and the recovery pass inherits the obligation.
	_, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "settle-once-on-retry", RepoID: repoID, To: tmux.ProgramGemini,
	})
	if !errors.Is(err, task.ErrPromptDelivery) {
		t.Fatalf("HandoffSession error = %v, want a post-ready prompt-delivery failure", err)
	}
	mission := inst.PendingHandoffMission()
	if mission == "" {
		t.Fatal("the undelivered mission was not retained; there is no recovery to test")
	}

	diskFull := errors.New("no space left on device")
	injected := failSettlementWrite(t, "settle-once-on-retry", diskFull, true)
	backend.setSendErr(nil)

	manager.ResumePendingHandoffs()
	if !injected() {
		t.Fatal("no settlement write was ever attempted, so this test exercised nothing")
	}
	if got := inst.PendingHandoffMission(); got != "" {
		t.Fatalf("recovery left the mission pending in memory (%q) after delivering it", got)
	}

	// The next poll has to finish what the failed write left open.
	manager.ResumePendingHandoffs()

	rec := recordFor(t, repoID, "settle-once-on-retry")
	if rec == nil || rec.PendingHandoffMission != "" {
		t.Fatalf("record after the following poll = %+v, want no pending mission", rec)
	}

	reloadedHandoffRow(t, manager, repoID, repoPath, rec, backend)
	manager.ResumePendingHandoffs()

	_, prompts := backend.snapshot()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "continuing work") {
		t.Fatalf("recovery delivered %d prompts (%q), want exactly the one rendered mission: a "+
			"settlement write lost by the recovery pass replays the mission just as one lost by the "+
			"handoff itself does", len(prompts), prompts)
	}
}

// blockingNthSwapBackend parks inside the Nth SwapAgent so a test can hold a
// handoff transaction open at a chosen point. blockingSwapBackend blocks the
// FIRST swap and closes its channel unconditionally; this one has to let an
// earlier handoff through before it stops the next.
type blockingNthSwapBackend struct {
	*handoffBackend
	blockAt int
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (b *blockingNthSwapBackend) SwapAgent(i *session.Instance, plan session.AgentSwapPlan) error {
	b.mu.Lock()
	b.calls++
	block := b.calls == b.blockAt
	b.mu.Unlock()
	if block {
		close(b.entered)
		<-b.release
	}
	return b.handoffBackend.SwapAgent(i, plan)
}

// A settlement retry must not write while the session is inside another
// operation (post-merge Codex finding on #2781).
//
// The retry is a WHOLE-ROW write of live memory. A second handoff raises
// OpReplacing and rewrites Program before it records its own mission marker, and
// disk strips the transient op — so a retry landing in that window persists the
// incoming agent as SETTLED with no obligation at all. A crash before that
// handoff's real checkpoint would then lose its takeover brief outright, which
// trades the duplicate this PR fixes for a silently dropped mission.
//
// So the poll holds off while the row is busy and finishes the write once it is
// free. The obligation is durable in memory; a skipped tick costs nothing.
func TestFlushHandoffSettlements_HoldsOffWhileTheRowIsInsideAnotherHandoff(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := &handoffBackend{FakeBackend: session.NewFakeBackend()}
	backend := &blockingNthSwapBackend{
		handoffBackend: base,
		blockAt:        2,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	registerHandoffSubject(t, manager, repoID, repoPath, "settle-race", backend)

	// First handoff: its settlement write fails, so the row owes one.
	diskFull := errors.New("no space left on device")
	injected := failSettlementWrite(t, "settle-race", diskFull, false)
	if _, err := manager.HandoffSession(HandoffSessionRequest{
		Title: "settle-race", RepoID: repoID, To: tmux.ProgramGemini,
	}); !errors.Is(err, diskFull) {
		t.Fatalf("first HandoffSession error = %v, want the settlement write failure", err)
	}
	if !injected() {
		t.Fatal("no settlement write was ever attempted, so this test exercised nothing")
	}
	owed := recordFor(t, repoID, "settle-race")
	if owed == nil || owed.PendingHandoffMission == "" || owed.Program != tmux.ProgramGemini {
		t.Fatalf("record after the failed settlement = %+v, want gemini with its mission still pending", owed)
	}

	// Second handoff, parked inside SwapAgent: the fence is up and Program already
	// names codex, but this handoff's own mission marker does not exist yet.
	done := make(chan error, 1)
	go func() {
		_, err := manager.HandoffSession(HandoffSessionRequest{
			Title: "settle-race", RepoID: repoID, To: tmux.ProgramCodex,
		})
		done <- err
	}()
	<-backend.entered

	// The poll fires in that window.
	manager.ResumePendingHandoffs()

	mid := recordFor(t, repoID, "settle-race")
	if mid == nil || mid.Program != owed.Program || mid.PendingHandoffMission != owed.PendingHandoffMission {
		t.Fatalf("the retry wrote mid-transaction: record = %+v, want it untouched at %q with its mission "+
			"still pending. Persisting live memory here stores the INCOMING agent as settled with no "+
			"obligation, so a crash before that handoff's checkpoint loses its takeover brief", mid, owed.Program)
	}

	close(backend.release)
	if err := <-done; err != nil {
		t.Fatalf("second HandoffSession: %v", err)
	}

	// Once the row is free again the obligation is discharged — by this handoff's
	// own settlement, or by the next poll's retry if that one also failed.
	manager.ResumePendingHandoffs()
	final := recordFor(t, repoID, "settle-race")
	if final == nil || final.PendingHandoffMission != "" || final.Program != tmux.ProgramCodex {
		t.Fatalf("record after the transaction finished = %+v, want a settled codex row with no pending mission", final)
	}
}
