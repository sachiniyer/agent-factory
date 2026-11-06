package session

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #6892: a tmux respawn is the SAME tab with a new process — identity (ID) is
// minted once at creation; incarnation (gen, #5571) is what the restart changes.
// The deterministic repro of the run-42244518786 shape: every sibling session is
// definitively absent (the tmux outage), setupTabs brings the roster back, then
// an unrelated CloseTab republishes it — and every surviving id must be exactly
// the one minted at creation, on the in-memory roster AND on the serialized
// TabData clients reconcile against. A lazy re-mint deferred to a later roster
// publish fails here.
func TestTabIDsSurviveRespawnAndUnrelatedClose(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, t.TempDir(), "id-stability", "id-stability-branch", "", false, false)
	require.NoError(t, err)

	// Every session reports absent — the respawn shape: the shell takes the
	// dead-shell replacement path (bindings{id: tab.ID}), the process tab goes
	// through restoreProcessTab; the agent itself is already re-established
	// before setupTabs runs.
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), "has-session") {
				return errors.New("can't find session")
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	pty := &recordingPtyFactory{t: t}
	agentTmux := tmux.NewTmuxSessionWithDeps("id-stability", tmux.ProgramClaude, pty, cmdExec)
	agentTab := &Tab{ID: "agent-id", Name: agentTabName, Kind: TabKindAgent, tmux: agentTmux}
	shellTmux := agentTmux.NewSiblingSession("id-stability"+tmuxTabSeparator+"shell", "bash")
	shellTab := &Tab{ID: "shell-id", Name: "peer-fit-2347", Kind: TabKindShell, tmux: shellTmux}
	procTmux := agentTmux.NewSiblingSession("id-stability"+tmuxTabSeparator+"scrollkeep", "sleep 300")
	procTab := &Tab{ID: "proc-id", Name: "scrollkeep-2933", Kind: TabKindProcess, Command: "sleep 300", tmux: procTmux}

	inst := &Instance{
		ID:          "id-stability-id",
		Title:       "id-stability",
		Path:        repoRoot,
		Program:     tmux.ProgramClaude,
		backend:     &LocalBackend{},
		Tabs:        []*Tab{agentTab, shellTab, procTab},
		gitWorktree: gw,
		started:     true,
		liveness:    LiveRunning,
	}

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))

	for i, want := range []string{"agent-id", "shell-id", "proc-id"} {
		assert.Equal(t, want, inst.Tabs[i].ID, "tab %d's identity must carry across the respawn", i)
	}
	ids := func() []string {
		var out []string
		for _, td := range inst.TabsData() {
			out = append(out, td.ID)
		}
		return out
	}
	assert.Equal(t, []string{"agent-id", "shell-id", "proc-id"}, ids(),
		"the serialized roster must carry the same ids it had at creation")

	// The later unrelated mutation: closing scrollkeep-2933 republishes the
	// roster — and must not re-mint anything it leaves behind.
	require.NoError(t, inst.CloseTabByID("proc-id"))
	assert.Equal(t, []string{"agent-id", "shell-id"}, ids(),
		"a surviving tab's id may never change as a side effect of an unrelated close")
}
