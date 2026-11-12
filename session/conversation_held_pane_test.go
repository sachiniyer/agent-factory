package session

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

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

	// The user's split pane: a separate process holding its own thread.
	other := exec.Command("tail", "-f", split)
	other.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, other.Start())
	t.Cleanup(func() {
		_ = other.Process.Kill()
		_, _ = other.Process.Wait()
	})
	require.Eventually(t, func() bool {
		_, err := proctree.Lookup(other.Process.Pid)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)

	// tmux answers "the active pane" with the split's process.
	executor := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(c.String(), "display-message") {
				return []byte(fmt.Sprintf("%d\n", other.Process.Pid)), nil
			}
			return nil, errors.New("unexpected tmux command in fixture: " + c.String())
		},
	}
	inst := &Instance{Tabs: []*Tab{newAgentTab(
		tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_split_pane", tmux.ProgramCodex, nil, executor))}}
	// No (runtimePID, runtimeStartID): the launch evidence is gone.

	conv, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.Error(t, err, "without verified launch evidence af cannot tell which process is the agent")
	require.False(t, conv.HasID(), "the active pane's thread must never be named as the session's")
}
