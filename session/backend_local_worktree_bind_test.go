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

// newBindHookTmux returns a tmux handle whose Start fails immediately through a
// mocked pty factory — enough to drive Launch through gw.Setup() without any
// real tmux server. The launch error it produces is expected; the hook under
// test has already run by then.
func newBindHookTmux(t *testing.T, title string) *tmux.TmuxSession {
	t.Helper()
	execu := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ptyFactory := answeredLaunchPtyFactory{t: t, waitErr: errors.New("tmux refused")}
	return tmux.NewTmuxSessionWithDeps(title, "true", ptyFactory, execu)
}

// TestLocalBackendBeforeWorktreeCreateFiresBeforeWorktreeAdd pins #6259's
// ordering contract: the hook observes the exact worktree path while nothing
// can yet exist at it. A fire binds that path into its durable marker here, so
// a crash between `git worktree add` and the record commit leaves a provable
// leftover.
func TestLocalBackendBeforeWorktreeCreateFiresBeforeWorktreeAdd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initInPlaceRepo(t, "main")
	gw, _, err := git.NewGitWorktree(repoRoot, "bind-hook", "")
	require.NoError(t, err)
	worktreePath := gw.GetWorktreePath()
	t.Cleanup(func() { _, _ = gw.Cleanup() })

	var bound string
	var existedAtHook bool
	inst := &Instance{
		Title:       "bind-hook",
		Path:        repoRoot,
		Program:     "true",
		backend:     &LocalBackend{},
		Tabs:        []*Tab{newAgentTab(newBindHookTmux(t, "bind-hook"))},
		gitWorktree: gw,
		beforeWorktreeCreate: func(path string) {
			bound = path
			_, statErr := os.Stat(path)
			existedAtHook = statErr == nil
		},
	}

	// The tmux handle is built to refuse its start; the hook has already run by
	// the time Launch fails, so the error itself is not under test.
	_ = (&LocalBackend{}).Launch(inst, true)

	require.Equal(t, worktreePath, bound,
		"the hook must observe the exact path gw.Setup() is about to create")
	require.False(t, existedAtHook,
		"the hook must run while the worktree path does not yet exist on disk")
}

// TestLocalBackendBeforeWorktreeCreateSkipsExternalTrees: an in-place session's
// worktree is attached, never created — there is no create crash window to
// prove, and binding the repo's own tree would let a reconcile judge the user's
// checkout a teardown candidate.
func TestLocalBackendBeforeWorktreeCreateSkipsExternalTrees(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initInPlaceRepo(t, "main")
	gw, _, err := git.NewGitWorktreeInPlace(repoRoot)
	require.NoError(t, err)

	fired := false
	inst := &Instance{
		Title:       "bind-hook-inplace",
		Path:        repoRoot,
		Program:     "true",
		backend:     &LocalBackend{},
		Tabs:        []*Tab{newAgentTab(newBindHookTmux(t, "bind-hook-inplace"))},
		gitWorktree: gw,
		inPlace:     true,
		beforeWorktreeCreate: func(string) {
			fired = true
		},
	}

	_ = (&LocalBackend{}).Launch(inst, true)
	require.False(t, fired, "an external worktree is never created, so the hook must not run")
}

// TestNewInstanceCarriesBeforeWorktreeCreate: the option must reach the
// instance the local backend launches, or the daemon's binder silently no-ops.
func TestNewInstanceCarriesBeforeWorktreeCreate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initInPlaceRepo(t, "main")
	hook := func(string) {}
	inst, err := NewInstance(InstanceOptions{
		Title: "carry-hook", Path: repoRoot, Program: "true", Backend: BackendLocal,
		BeforeWorktreeCreate: hook,
	})
	require.NoError(t, err)
	require.NotNil(t, inst.beforeWorktreeCreate)
}
