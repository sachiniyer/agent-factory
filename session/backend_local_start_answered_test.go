package session

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

type answeredLaunchPtyFactory struct {
	t       *testing.T
	waitErr error
}

func (f answeredLaunchPtyFactory) Start(*exec.Cmd) (*os.File, error) {
	f.t.Fatal("Start called instead of StartTracked")
	return nil, nil
}

func (f answeredLaunchPtyFactory) StartTracked(*exec.Cmd) (*os.File, <-chan error, error) {
	ptmx, err := os.CreateTemp(f.t.TempDir(), "answered-launch-pty")
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	done <- f.waitErr
	close(done)
	return ptmx, done, nil
}

func (answeredLaunchPtyFactory) Close() {}

// TestLocalBackendAnsweredStartFailurePreservesFreshWorktree is the user-visible
// half of the pre-spawn-only cleanup invariant. A process that started and then
// failed may have launched a pane which is still flushing even after the tmux
// name disappears, so Launch must retain the worktree for explicit cleanup.
func TestLocalBackendAnsweredStartFailurePreservesFreshWorktree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initInPlaceRepo(t, "main")
	gw, _, err := git.NewGitWorktree(repoRoot, "answered-start")
	require.NoError(t, err)
	worktreePath := gw.GetWorktreePath()
	t.Cleanup(func() { _, _ = gw.Cleanup() })

	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ptyFactory := answeredLaunchPtyFactory{
		t:       t,
		waitErr: errors.New("tmux new-session rejected the request"),
	}
	ts := tmux.NewTmuxSessionWithDeps("answered-start", "true", ptyFactory, execu)
	inst := &Instance{
		Title:       "answered-start",
		Path:        repoRoot,
		Program:     "true",
		backend:     &LocalBackend{},
		Tabs:        []*Tab{newAgentTab(ts)},
		gitWorktree: gw,
	}

	err = (&LocalBackend{}).Launch(inst, true)
	require.Error(t, err)
	require.NotErrorIs(t, err, tmux.ErrSessionNotStarted)
	require.ErrorIs(t, err, ErrPaneMayBeLive)
	_, statErr := os.Stat(worktreePath)
	require.NoError(t, statErr,
		"a post-spawn failure deleted a worktree whose pane may still be flushing")
}
