package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7123 daemon suite. #7120 made the held query judge only from the launch
// evidence af pinned to tab 0's agent process; the new-since-baseline diff
// still fell back to the session's ACTIVE pane, and both daemon entry points
// that run it — the loop's first capture and the account swap's synchronous
// capture — adopted a user's split pane's thread when that evidence was gone.

// activePaneCodexRow is #7120's split-pane fixture at the daemon's capture
// entry points: a tracked, unrecorded codex row with no launch evidence whose
// tab 0 tmux session answers "the active pane" with a user's split — a real
// process, in its own kernel session, holding a thread it minted after the
// baseline in the same worktree and store. asked counts the pane-root queries
// put to tmux; no attribution path may make one.
func activePaneCodexRow(t *testing.T) (*Manager, *session.Instance, string, string, session.ConversationCaptureSnapshot, *atomic.Int32) {
	t.Helper()
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	// The real process walk, not the fixture's stand-in: what is under test
	// is which process the walk starts from.
	inst.SetCodexRuntimeHeldOverrideForTest(nil)
	snap := inst.SessionCodexCaptureBaseline()
	require.NoError(t, snap.CodexBaselineErr(), "fixture: the baseline must hold")
	time.Sleep(20 * time.Millisecond)
	split := writeHeldTestRollout(t, codexHome, heldMissionThreadID, inst.GetWorktreePath())

	holder := exec.Command("tail", "-f", split)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})
	require.Eventually(t, func() bool {
		_, err := proctree.Lookup(holder.Process.Pid)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the split's process must be visible")

	asked := &atomic.Int32{}
	executor := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(c.String(), "display-message") && strings.Contains(c.String(), "pane_pid") {
				asked.Add(1)
				return []byte(fmt.Sprintf("%d\n", holder.Process.Pid)), nil
			}
			return nil, nil
		},
	}
	// SetTmuxSession records the program alone — no (pid, start-stamp) pair,
	// which is exactly a launch whose evidence was retired or never recorded.
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_codex-worker", tmux.ProgramCodex, nil, executor))
	return manager, inst, repoID, key, snap, asked
}

// TestConversationCaptureNeverAdoptsTheActivePanesThread is the daemon loop's
// first capture attempt (#7123): with no evidence the diff adopts nothing, and
// the attempt reports why rather than committing the split's thread.
func TestConversationCaptureNeverAdoptsTheActivePanesThread(t *testing.T) {
	manager, inst, repoID, key, snap, asked := activePaneCodexRow(t)

	err := manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), conversationCaptureTimeout)
	require.Error(t, err, "an attempt that cannot attribute says why")
	require.False(t, inst.AgentConversation().HasID(),
		"the loop recorded %s, the active pane's thread", inst.AgentConversation().ID)
	require.Empty(t, persistedConversationID(t, repoID))
	require.Zero(t, asked.Load(), "the capture must never ask tmux which pane is active")
}

// TestAccountSwapCaptureNeverAdoptsTheActivePanesThread is the swap's
// synchronous pre-delivery capture (#7123): with no evidence it records
// nothing and does not fail the handoff — it defers to the post-delivery
// capture, exactly as it does when no rollout exists yet (#4712).
func TestAccountSwapCaptureNeverAdoptsTheActivePanesThread(t *testing.T) {
	_, inst, _, _, snap, asked := activePaneCodexRow(t)

	captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
	require.NoError(t, err, "missing evidence must not fail a committed handoff")
	require.True(t, captureAfterDelivery, "the swap defers to the post-delivery capture")
	require.False(t, inst.AgentConversation().HasID(),
		"the swap recorded %s, the active pane's thread", inst.AgentConversation().ID)
	require.Zero(t, asked.Load(), "the swap capture must never ask tmux which pane is active")
}

// TestUnrecordedRowWithAFailedBaselineIsAnsweredByTheHeldQuery is #7123's
// property 3. A failed before-image can never answer the diff, but the held
// query needs no baseline. The attempt used to return the diff's error before
// the held query could answer, and the loop then ended, handing the row back
// to the poll — which re-walked the store for a fresh baseline every cycle.
// The loop must stay armed and record the runtime's thread through the held
// query once the runtime holds one.
func TestUnrecordedRowWithAFailedBaselineIsAnsweredByTheHeldQuery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
	held := &heldOnly{}
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot) // unprompted: holds nothing yet
	// A day directory inside the scan window that the walk cannot enter.
	blocked := configAgentCodexDayDir(codexHome, time.Now().AddDate(0, 0, -1))
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.Chmod(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	walked := inst.SessionCodexCaptureBaseline()
	require.Error(t, walked.CodexBaselineErr(), "fixture: a store walk now fails")

	manager.ArmConversationCaptures()
	token, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "an unrecorded row must gain its capture loop")

	time.Sleep(10 * conversationCaptureRearmInterval)
	still, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "a failed baseline must not end the loop: the poll would re-walk the store to re-arm it")
	require.Equal(t, token, still)

	// The pane's first prompt mints its thread, which the runtime holds open.
	own := writeHeldTestRollout(t, codexHome, heldUserThreadID, inst.GetWorktreePath())
	held.set(own)
	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == heldUserThreadID
	}, 3*time.Second, 10*time.Millisecond, "the held query must answer without a baseline")
	require.Eventually(t, func() bool {
		return persistedConversationID(t, repoID) == heldUserThreadID
	}, 3*time.Second, 10*time.Millisecond, "the recorded id must reach disk")
	final, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed)
	require.Equal(t, token, final, "the same loop recorded it; nothing re-armed")
}
