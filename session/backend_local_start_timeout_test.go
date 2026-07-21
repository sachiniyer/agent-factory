package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// renamedTmuxWorld models the #2207 failure without touching a real tmux
// server: new-session succeeds but tmux stores a different spelling, so every
// probe and cleanup command using af's requested spelling gets a confident
// "not found" while the created session remains live.
type renamedTmuxWorld struct {
	t *testing.T

	mu      sync.Mutex
	running map[string]bool
}

func newRenamedTmuxWorld(t *testing.T) *renamedTmuxWorld {
	return &renamedTmuxWorld{t: t, running: make(map[string]bool)}
}

func (w *renamedTmuxWorld) Start(c *exec.Cmd) (*os.File, error) {
	requested := argAfter(c.Args, "-s")
	if requested == "" {
		return nil, fmt.Errorf("new-session command has no -s name: %v", c.Args)
	}

	// tmux 3.4's utf8_stravis doubles a literal backslash while cleaning a
	// session name. The requested spelling is therefore never found again.
	stored := strings.ReplaceAll(requested, `\`, `\\`)
	w.mu.Lock()
	w.running[stored] = true
	w.mu.Unlock()

	return os.OpenFile(filepath.Join(w.t.TempDir(), "pty"), os.O_CREATE|os.O_RDWR, 0o600)
}

func (w *renamedTmuxWorld) Close() {}

func (w *renamedTmuxWorld) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			target := tmuxTargetName(c.Args)
			if target == "" {
				return nil
			}

			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.running[target] {
				return errors.New("can't find session")
			}
			if slices.Contains(c.Args, "kill-session") {
				delete(w.running, target)
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
}

func (w *renamedTmuxWorld) isRunning(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running[name]
}

func tmuxTargetName(args []string) string {
	for i, arg := range args {
		var target string
		switch {
		case strings.HasPrefix(arg, "-t="):
			target = strings.TrimPrefix(arg, "-t=")
		case arg == "-t" && i+1 < len(args):
			target = strings.TrimPrefix(args[i+1], "=")
		default:
			continue
		}
		return strings.TrimSuffix(target, ":")
	}
	return ""
}

func TestLocalBackendLaunchReadinessTimeoutPreservesWorktree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initInPlaceRepo(t, "main")
	gw, _, err := git.NewGitWorktree(repoRoot, "start-timeout")
	require.NoError(t, err)
	worktreePath := gw.GetWorktreePath()
	t.Cleanup(func() { _, _ = gw.Cleanup() })

	const requestedName = `af_start\timeout`
	const storedName = `af_start\\timeout`
	world := newRenamedTmuxWorld(t)
	ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(requestedName, "true", world, world.exec())
	inst := &Instance{
		Title:       "start-timeout",
		Path:        repoRoot,
		Program:     "true",
		backend:     &LocalBackend{},
		Tabs:        []*Tab{newAgentTab(ts)},
		gitWorktree: gw,
	}

	err = (&LocalBackend{}).Launch(inst, true)
	require.Error(t, err)
	require.True(t, world.isRunning(storedName),
		"the differently spelled tmux session should still be running in the model")
	_, statErr := os.Stat(worktreePath)
	require.NoError(t, statErr,
		"a readiness timeout is unknown, not proof the session failed to start; Launch must preserve its worktree")
	require.ErrorIs(t, err, tmux.ErrTmuxTimeout)
	require.NotErrorIs(t, err, tmux.ErrSessionNotStarted)
	require.ErrorIs(t, err, ErrPaneMayBeLive)
}
