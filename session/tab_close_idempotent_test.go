package session

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

// deadAwareExec is like nameKeyedExec but models tmux's real kill-session
// behavior: killing a live session succeeds and removes it; killing a session
// that is already gone exits 1 ("can't find session"). This is what lets the
// #967 regression actually fire — nameKeyedExec makes every kill-session
// succeed, so it can't reproduce the spurious-error path.
func deadAwareExec(alive map[string]bool) cmd_test.MockCmdExec {
	existing := map[string]bool{}
	for k, v := range alive {
		existing[k] = v
	}
	nameOf := func(c *exec.Cmd) string {
		for i, a := range c.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(c.Args):
				// Strip the exact-match `=name:` wrapper (`-t =name:`) so the modeled
				// session name matches the bare key tmux resolves to (#1006).
				return strings.TrimSuffix(strings.TrimPrefix(c.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := c.String()
			n := nameOf(c)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				return errors.New("can't find session")
			case strings.Contains(s, "new-session"):
				existing[n] = true
				return nil
			case strings.Contains(s, "kill-session"):
				if existing[n] {
					delete(existing, n)
					return nil
				}
				return errors.New("exit status 1") // already gone -> tmux exits 1
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return []byte("content"), nil
		},
	}
}

// TestCloseTab_StaleSessionAlreadyDead is the #967 end-to-end check: a tab whose
// tmux session died externally (box reboot, manual kill-server) is surfaced from
// the daemon Snapshot, the user presses close, and kill-session exits 1. CloseTab
// must still return nil and remove the tab — a dead session is the goal of a
// close, not an error to wedge the tab list.
func TestCloseTab_StaleSessionAlreadyDead(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_stale_close"
	deadShell := agentName + shellTmuxSuffix
	// Agent session is alive; the shell tab's session is already dead.
	exec := deadAwareExec(map[string]bool{agentName: true})
	pty := persistPtyFactory{t: t, cmdExec: exec}

	gw, err := git.NewGitWorktreeFromStorage(
		"/tmp/stale-close-"+agentName, filepath.Join(t.TempDir(), "wt"),
		agentName, agentName+"-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	deadTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(deadShell, "/bin/sh", pty, exec)
	inst := &Instance{
		Title:       agentName,
		Path:        "/tmp/stale-close-" + agentName,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs), newShellTab(deadTs)},
	}
	require.Equal(t, 2, inst.TabCount())

	require.NoError(t, inst.CloseTab(1),
		"closing a tab whose tmux already died must succeed, not error (#967)")
	require.Equal(t, 1, inst.TabCount(), "the stale tab must be removed")
}
