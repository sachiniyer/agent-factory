package session

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The active-pane suite (#7120 review F2, #7123). The session's ACTIVE pane is
// not evidence of which process is its agent: a user's split, or a new window,
// can run its own codex in the same worktree against the same account-scoped
// store. Every path that attributes a rollout to the session — the held query,
// the daemon loop's new-since-baseline diff, the swap's synchronous capture —
// must judge from the launch evidence af pinned to tab 0's agent process, or
// fail closed. None of them may ask tmux which pane is active.

// startRolloutHolder starts a real process, in its own kernel session, that
// holds path open the way a Codex runtime holds its thread's rollout, and
// waits until the process table can see it.
func startRolloutHolder(t *testing.T, path string) proctree.Process {
	t.Helper()
	holder := exec.Command("tail", "-f", path)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})
	var proc proctree.Process
	require.Eventually(t, func() bool {
		p, err := proctree.Lookup(holder.Process.Pid)
		proc = p
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the holder must be visible in the process table")
	return proc
}

// exitedProcess returns the identity of a process that has already exited:
// launch evidence naming an agent that is gone.
func exitedProcess(t *testing.T) proctree.Process {
	t.Helper()
	gone := exec.Command("sleep", "30")
	require.NoError(t, gone.Start())
	var proc proctree.Process
	require.Eventually(t, func() bool {
		p, err := proctree.Lookup(gone.Process.Pid)
		proc = p
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, gone.Process.Kill())
	_, _ = gone.Process.Wait()
	require.Eventually(t, func() bool {
		p, err := proctree.Lookup(proc.PID)
		return err != nil || p.StartID != proc.StartID
	}, 5*time.Second, 20*time.Millisecond, "the exited process must leave the process table")
	return proc
}

// activePaneInstance is #7120's split-pane fixture: tab 0's tmux session
// answers "the active pane" with active. asked counts every time anything
// asks, because no attribution path may.
func activePaneInstance(t *testing.T, active proctree.Process) (*Instance, *atomic.Int32) {
	t.Helper()
	asked := &atomic.Int32{}
	executor := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(c.String(), "display-message") {
				asked.Add(1)
				return []byte(fmt.Sprintf("%d\n", active.PID)), nil
			}
			return nil, errors.New("unexpected tmux command in fixture: " + c.String())
		},
	}
	inst := &Instance{Tabs: []*Tab{newAgentTab(
		tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_split_pane", tmux.ProgramCodex, nil, executor))}}
	return inst, asked
}

// TestHeldAgentConversation_NeverJudgesTheActivePane is the #7120 review's
// F2. With the launch evidence retired (an unanswerable reattach probe) or
// never recorded (a pane query that failed at launch), the runtime root used
// to fall back to the session's ACTIVE pane — which can be a user's split or a
// new window running its own codex in the same worktree and store. Its thread
// is not this session's, and the held query must refuse to name it rather than
// record it (and re-record it every interval). "Cannot tell which process is
// the agent" is an error the loop logs, never an answer.
func TestHeldAgentConversation_NeverJudgesTheActivePane(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	split := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	inst, asked := activePaneInstance(t, startRolloutHolder(t, split))
	// No (runtimePID, runtimeStartID): the launch evidence is gone.

	conv, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.Error(t, err, "without verified launch evidence af cannot tell which process is the agent")
	require.False(t, conv.HasID(), "the active pane's thread must never be named as the session's")
	require.Zero(t, asked.Load(), "the held query must never ask tmux which pane is active")
}

// TestCaptureAgentConversation_NeverJudgesTheActivePane is #7123: the
// new-since-baseline diff — the daemon loop's first capture and the account
// swap's synchronous pre-delivery capture, both of which call
// CaptureAgentConversation on a bound snapshot — resolved its runtime through
// the same active-pane fallback the held query lost in #7120. With the split's
// rollout minted after the baseline, the diff adopted the split's thread, the
// blind follow could never correct it, and the next restore resumed it.
// Without verified evidence the diff fails closed: nothing is adopted, and the
// pane is never asked.
func TestCaptureAgentConversation_NeverJudgesTheActivePane(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence func(t *testing.T) proctree.Process
	}{
		{"no launch record", func(*testing.T) proctree.Process { return proctree.Process{} }},
		{"launch record names an agent that exited", exitedProcess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexHome := t.TempDir()
			workDir := t.TempDir()
			snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
			time.Sleep(20 * time.Millisecond)
			// The split mints its rollout after the baseline, so the diff —
			// not the held query — is what sees it.
			split := writeCodexRolloutFileWithCwd(t, codexHome,
				"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", workDir)
			inst, asked := activePaneInstance(t, startRolloutHolder(t, split))
			evidence := tc.evidence(t)
			inst.runtimePID, inst.runtimeStartID = evidence.PID, evidence.StartID

			conv, err := CaptureAgentConversation(tmux.ProgramCodex,
				inst.BindConversationCaptureRuntime(snap), 300*time.Millisecond)
			require.NoError(t, err, "no evidence is not a broken store: the capture fails closed, quietly")
			require.False(t, conv.HasID(),
				"the diff adopted %s, the active pane's thread, as the session's conversation", conv.ID)

			_, err = HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
			require.Error(t, err, "the held query names why nothing can be attributed")
			require.Zero(t, asked.Load(), "no attribution path may ask tmux which pane is active")
		})
	}
}

// TestCaptureAgentConversation_JudgesTheVerifiedAgentProcess is the other half:
// failing closed without evidence must not cost the session its own thread
// when the evidence verifies. Tab 0's agent process and the user's split each
// hold a rollout minted after the baseline in the same worktree; the split is
// the active pane. The diff and the held query both name the agent's thread,
// from the launch record alone.
func TestCaptureAgentConversation_JudgesTheVerifiedAgentProcess(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	time.Sleep(20 * time.Millisecond)
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", workDir)
	split := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", workDir)
	agent := startRolloutHolder(t, own)
	inst, asked := activePaneInstance(t, startRolloutHolder(t, split))
	inst.runtimePID, inst.runtimeStartID = agent.PID, agent.StartID

	var conv AgentConversationData
	var err error
	require.Eventually(t, func() bool {
		conv, err = CaptureAgentConversation(tmux.ProgramCodex,
			inst.BindConversationCaptureRuntime(snap), 300*time.Millisecond)
		return err == nil && conv.HasID()
	}, 5*time.Second, 50*time.Millisecond, "the diff must attribute the agent's own rollout: %v", err)
	require.Equal(t, heldTestOwnID, conv.ID, "the diff names the verified agent's thread, not the active pane's")

	held, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, held.ID, "the held query names the verified agent's thread")
	require.Zero(t, asked.Load(), "attribution reads the launch record, never the active pane")
}
