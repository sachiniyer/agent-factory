package daemon

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7101 daemon suite. A Codex session handed to another account 18 s
// after create ran 54 minutes and was restored fresh: the record could not
// follow the pane to the thread it moved to, and every capture armed after a
// rollout existed — a daemon restart, a deferred arm that claimed the swap's
// replacement generation after its mission minted the file — was blind to it.
// Each test below names one of those shapes; the rollout the runtime holds
// open is what must end up recorded.

const (
	heldMissionThreadID = "01a1cc95-f883-7252-b370-6e9ab3c41145"
	heldUserThreadID    = "01a1cc97-42b0-7022-9c38-f9b73b6425d3"
)

// writeHeldTestRollout writes a top-level Codex rollout correlated to dir and
// returns its path.
func writeHeldTestRollout(t *testing.T, codexHome, id, dir string) string {
	t.Helper()
	name := "rollout-2026-11-11T13-49-03-" + id + ".jsonl"
	writeDaemonCodexRolloutFileWithCwd(t, codexHome, name, dir)
	return filepath.Join(configAgentCodexDayDir(codexHome, time.Now()), name)
}

// heldOnly hands attribution a runtime that holds exactly the given files,
// replaceable mid-test the way a pane's process moves between threads.
type heldOnly struct {
	mu   sync.Mutex
	held map[string]struct{}
}

func (h *heldOnly) set(paths ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = make(map[string]struct{}, len(paths))
	for _, path := range paths {
		h.held[path] = struct{}{}
	}
}

func (h *heldOnly) snapshot() (map[string]struct{}, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make(map[string]struct{}, len(h.held))
	for path := range h.held {
		cp[path] = struct{}{}
	}
	return cp, nil
}

// TestArmConversationCapturesRecordsARolloutMintedBeforeTheArm is the restart
// shape: the pane's rollout existed when the daemon came back, so the deferred
// arm's fresh baseline already lists it. Before #7101 the loop then re-asked
// the store every rearm for the life of the row and always heard "nothing
// new" — silently, by design.
func TestArmConversationCapturesRecordsARolloutMintedBeforeTheArm(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
	writeHeldTestRollout(t, codexHome, heldUserThreadID, inst.GetWorktreePath())

	manager.ArmConversationCaptures()

	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == heldUserThreadID
	}, 3*time.Second, 10*time.Millisecond,
		"a rollout the runtime holds must be recorded even when it predates the arm's baseline")
	require.Eventually(t, func() bool {
		return persistedConversationID(t, repoID) == heldUserThreadID
	}, 3*time.Second, 10*time.Millisecond, "the recorded id must reach disk")
}

// TestHandoffInTheCreateCaptureWindowRecordsTheReplacementRollout is the
// issue's own sequence. The create arms its capture; a handoff replaces the
// runtime inside that window; the replacement's mission mints its rollout;
// the poll's deferred pass claims the new generation (swaps never enter
// killsInFlight) with a baseline that already lists the file; and the swap's
// own post-delivery arm, whose pre-launch baseline would have found it, is
// refused as a duplicate of that generation.
func TestHandoffInTheCreateCaptureWindowRecordsTheReplacementRollout(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)

	manager.captureAgentConversationAsync(repoID, key, inst, inst.SessionCodexCaptureBaseline())
	createToken, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "the create must arm its capture")

	// The handoff's preflight baselines the store, then launches the
	// replacement runtime under a new generation.
	swapSnap := inst.SessionCodexCaptureBaseline()
	inst.MarkAgentRuntimeReplacedForTest(true)
	require.NotEqual(t, createToken, inst.AgentRuntimeToken())
	// The mission lands and Codex mints the replacement's rollout.
	writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())
	// The poll's deferred pass runs before the swap returns...
	manager.ArmConversationCaptures()
	// ...so the swap's post-delivery arm finds its generation already armed.
	manager.captureAgentConversationAsync(repoID, key, inst, swapSnap)

	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == heldMissionThreadID
	}, 3*time.Second, 10*time.Millisecond,
		"the replacement's rollout must be recorded however the arms interleave")
	require.Eventually(t, func() bool {
		return persistedConversationID(t, repoID) == heldMissionThreadID
	}, 3*time.Second, 10*time.Millisecond, "the recorded id must reach disk")
}

// TestRecordedConversationFollowsTheRuntimeToANewThread is the stale half: the
// pane recorded its first thread, then the user started another in the same
// process (`/new`), which the runtime now holds instead. A lost-restore
// refuses the stale id and starts fresh, so the record must follow the
// runtime while it lives.
func TestRecordedConversationFollowsTheRuntimeToANewThread(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
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

	// The user starts a new thread; the runtime now holds only its rollout.
	second := writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held.set(second)

	manager.ArmConversationCaptures()

	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == heldUserThreadID
	}, 3*time.Second, 10*time.Millisecond,
		"the record must follow the runtime to the thread it now holds")
	require.Eventually(t, func() bool {
		return persistedConversationID(t, repoID) == heldUserThreadID
	}, 3*time.Second, 10*time.Millisecond, "the followed id must reach disk")
}
