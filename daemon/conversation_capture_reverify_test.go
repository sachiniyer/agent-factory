package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7123 property-2 daemon suite, with the #7124 review's F1, F2, F3 and F5.
// A reattach probe that could not answer leaves the launch identity kept for
// capture alone; every capture entry point re-verifies it with the
// session-wide pane scan before judging from it, and a row that still cannot
// be attributed says what that costs.

// heldThirdThreadID is a third thread for the failed-baseline shape, which
// needs the runtime to hold two others at once.
const heldThirdThreadID = "01a1cc9a-5c2e-7d41-8b3f-2c6d7e8f9a0b"

// keptEvidenceCodexRow is a tracked, unrecorded codex row after a daemon
// restart whose reattach probe could not answer: no launch evidence, but the
// agent's identity kept for capture. The agent — a real process in its own
// kernel session — holds the thread it minted after the baseline; tab 0's
// list-panes finds it rooting one of the session's panes, and the active-pane
// query, which nothing may make, is only counted.
func keptEvidenceCodexRow(t *testing.T) (*Manager, *session.Instance, string, string, session.ConversationCaptureSnapshot, proctree.Process, *atomic.Int32) {
	t.Helper()
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	inst.SetCodexRuntimeHeldOverrideForTest(nil)
	snap := inst.SessionCodexCaptureBaseline()
	require.NoError(t, snap.CodexBaselineErr(), "fixture: the baseline must hold")
	time.Sleep(20 * time.Millisecond)
	own := writeHeldTestRollout(t, codexHome, heldUserThreadID, inst.GetWorktreePath())

	holder := exec.Command("tail", "-f", own)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})
	var agent proctree.Process
	require.Eventually(t, func() bool {
		p, err := proctree.Lookup(holder.Process.Pid)
		agent = p
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the agent's process must be visible")

	asked := &atomic.Int32{}
	executor := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			switch s := c.String(); {
			case strings.Contains(s, "list-panes"):
				return []byte(fmt.Sprintf("%d|0|\n", agent.PID)), nil
			case strings.Contains(s, "display-message"):
				asked.Add(1)
			}
			return nil, nil
		},
	}
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_codex-worker", tmux.ProgramCodex, nil, executor))
	// After SetTmuxSession, which is a launch boundary and clears it.
	inst.KeepCaptureEvidenceForTest(agent)
	return manager, inst, repoID, key, snap, agent, asked
}

// persistedRow reads the single persisted row back from disk.
func persistedRow(t *testing.T, repoID string) session.InstanceData {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var stored []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Len(t, stored, 1)
	return stored[0]
}

// TestConversationCaptureReverifiesAKeptIdentityAndRecordsTheAgentsThread is
// property 2 through the daemon loop: the kept identity is re-verified by the
// session-wide scan, the runtime's own thread is recorded and persisted, and
// the launch evidence every other consumer reads stays retired.
func TestConversationCaptureReverifiesAKeptIdentityAndRecordsTheAgentsThread(t *testing.T) {
	manager, inst, repoID, key, snap, agent, asked := keptEvidenceCodexRow(t)

	require.NoError(t, manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), conversationCaptureTimeout))
	require.Equal(t, heldUserThreadID, inst.AgentConversation().ID,
		"an unanswerable reattach probe must not turn capture off for the rest of the runtime")
	row := persistedRow(t, repoID)
	require.NotNil(t, row.AgentConversation)
	require.Equal(t, heldUserThreadID, row.AgentConversation.ID)
	require.Zero(t, row.RuntimePID, "capture-only: the retired launch evidence is not re-enabled")
	require.Equal(t, agent.PID, row.CaptureRuntimePID, "the kept identity rides the durable record")
	require.Zero(t, asked.Load(), "re-verification scans the session's panes, never the active one")
}

// TestConversationFollowReverifiesAKeptIdentity is the follow's half: a
// recorded row whose evidence the probe could not confirm still follows the
// pane to the thread its runtime now holds.
func TestConversationFollowReverifiesAKeptIdentity(t *testing.T) {
	manager, inst, repoID, key, snap, _, asked := keptEvidenceCodexRow(t)
	token := inst.AgentRuntimeToken()
	require.True(t, inst.SetAgentConversationForRuntime(token, session.AgentConversationData{
		Agent:       tmux.ProgramCodex,
		ID:          heldMissionThreadID,
		CapturedAt:  time.Now(),
		CaptureKind: session.ConversationCaptureCodexRollout,
	}))

	require.NoError(t, manager.captureAgentConversation(repoID, key, inst, snap, token, conversationCaptureTimeout))
	require.Equal(t, heldUserThreadID, inst.AgentConversation().ID,
		"the follow re-points the record at the thread the re-verified runtime holds")
	require.Zero(t, asked.Load())
}

// TestConversationCaptureWithoutEvidenceNamesTheRestoreAndTheRepair is the
// #7124 review's F1: an unrecorded row that cannot be attributed restores into
// a fresh conversation, and the one warning the loop logs must say so and name
// the repair, not only that capture failed.
func TestConversationCaptureWithoutEvidenceNamesTheRestoreAndTheRepair(t *testing.T) {
	manager, inst, repoID, key, snap, _ := activePaneCodexRow(t)

	err := manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), conversationCaptureTimeout)
	require.ErrorIs(t, err, session.ErrNoConversationCaptureEvidence)
	require.Contains(t, err.Error(), "restoring this session starts a fresh conversation")
	require.Contains(t, err.Error(), "af sessions set-conversation -- codex-worker <id>")
}

// TestCaptureWithoutEvidenceEndsTheAttemptAtOnce is the #7124 review's F2 at
// both daemon entry points that run the diff: with nothing to attribute from,
// neither spends its absent window walking the store.
func TestCaptureWithoutEvidenceEndsTheAttemptAtOnce(t *testing.T) {
	manager, inst, repoID, key, snap, _ := activePaneCodexRow(t)
	const window = 3 * time.Second

	start := time.Now()
	require.Error(t, manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), window))
	require.Less(t, time.Since(start), window/3, "the loop's attempt must not poll for an answer that cannot come")

	old := conversationCaptureTimeout
	conversationCaptureTimeout = window
	t.Cleanup(func() { conversationCaptureTimeout = old })
	start = time.Now()
	captureAfterDelivery, err := captureAccountSwapConversation(inst, snap)
	require.NoError(t, err)
	require.True(t, captureAfterDelivery)
	require.Less(t, time.Since(start), window/3, "the swap must not hold its op-lock for an answer that cannot come")
}

// TestAFailedBaselineIsRetakenOnceTheStoreHeals is the #7124 review's F3. The
// loop kept a failed snapshot for the runtime's life, so a transient store
// failure removed the diff channel for good — and the diff is the only channel
// that can answer while the runtime holds more than one top-level thread. The
// next attempt after a failure must take the baseline again.
func TestAFailedBaselineIsRetakenOnceTheStoreHeals(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	shrinkConversationCaptureForTest(t)
	manager, inst, _, _, codexHome := codexLoopTestInstance(t)
	worktree := inst.GetWorktreePath()
	// The runtime holds two threads that predate everything: the held query is
	// ambiguous for the whole test, so only the diff can record anything.
	older := writeHeldTestRollout(t, codexHome, heldMissionThreadID, worktree)
	oldest := writeHeldTestRollout(t, codexHome, heldUserThreadID, worktree)
	held := &heldOnly{}
	held.set(older, oldest)
	inst.SetCodexRuntimeHeldOverrideForTest(held.snapshot)

	blocked := configAgentCodexDayDir(codexHome, time.Now().AddDate(0, 0, -1))
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.Chmod(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	manager.ArmConversationCaptures()
	_, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed)
	time.Sleep(5 * conversationCaptureRearmInterval)

	// The store heals; give the loop a few attempts to take a good baseline.
	require.NoError(t, os.Chmod(blocked, 0o755))
	time.Sleep(10 * conversationCaptureRearmInterval)

	// The runtime mints and holds a third thread — new since the retaken
	// baseline, so the diff can name it.
	minted := filepath.Join(configAgentCodexDayDir(codexHome, time.Now()), "rollout-2026-11-11T13-49-03-"+heldThirdThreadID+".jsonl")
	held.set(older, oldest, minted)
	require.Equal(t, minted, writeHeldTestRollout(t, codexHome, heldThirdThreadID, worktree))
	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == heldThirdThreadID
	}, 5*time.Second, 10*time.Millisecond, "a transient baseline failure must not remove the diff for the runtime's life")
}

// TestAgentServerCaptureSaysWhyItCouldNotAttribute is the #7124 review's F5:
// the in-sandbox agent-server's one-shot capture goes through the same
// chokepoint, and a row it cannot attribute must not be silent there either.
func TestAgentServerCaptureSaysWhyItCouldNotAttribute(t *testing.T) {
	_, inst, _, _, snap, _ := activePaneCodexRow(t)
	warnings := captureWarnings(t)
	hs := &headlessServer{inst: inst}

	hs.captureCodexConversation(snap)
	require.Eventually(t, func() bool {
		out := warnings.String()
		return strings.Contains(out, "agent-server") && strings.Contains(out, "codex-worker") &&
			strings.Contains(out, "restoring this session starts a fresh conversation")
	}, 3*time.Second, 20*time.Millisecond, "the agent-server must log why it recorded nothing")
	require.False(t, inst.AgentConversation().HasID())
	// #7131 R2: the host refuses set-conversation for a row that runs off this
	// host, and the in-sandbox agent-server serves no such route, so the warning
	// must not name that repair — nor promise a retry its one-shot capture
	// never makes.
	out := warnings.String()
	require.NotContains(t, out, "set-conversation", "the sandbox warning names a repair that would be refused")
	require.NotContains(t, out, "Until it can", "the one-shot capture never tries again")
}
