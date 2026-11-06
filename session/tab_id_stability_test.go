package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// spawningPtyFactory records each exec.Cmd like recordingPtyFactory and, when
// the command is a `tmux new-session`, registers the session name as alive in
// the shared set the mock executor's has-session consults — so a session
// reports presence after a successful spawn, matching real tmux. (#6893 Codex
// P2: a mock that never reports presence lets the respawn time out and binds
// nothing, so the test passed without exercising the respawn at all.)
type spawningPtyFactory struct {
	t    *testing.T
	cmds []*exec.Cmd
	live map[string]bool
}

func (p *spawningPtyFactory) Start(c *exec.Cmd) (*os.File, error) {
	p.cmds = append(p.cmds, c)
	if strings.Contains(c.String(), "new-session") {
		if n := tmuxSessionArg(c); n != "" {
			p.live[n] = true
		}
	}
	path := filepath.Join(p.t.TempDir(), fmt.Sprintf("pty-%s-%d", p.t.Name(), len(p.cmds)))
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
}

// tmuxSessionArg extracts the session name from a tmux command's -t/-s arg,
// stripping the exact-match `=name:` wrapper (#1006, mirrors countingExec).
func tmuxSessionArg(c *exec.Cmd) string {
	for i, a := range c.Args {
		switch {
		case (a == "-t" || a == "-s") && i+1 < len(c.Args):
			return strings.TrimSuffix(strings.TrimPrefix(c.Args[i+1], "="), ":")
		case strings.HasPrefix(a, "-t="):
			return strings.TrimPrefix(a, "-t=")
		case strings.HasPrefix(a, "-s="):
			return strings.TrimPrefix(a, "-s=")
		}
	}
	return ""
}

func newSessionCmdCount(pty *spawningPtyFactory, name string) int {
	n := 0
	for _, c := range pty.cmds {
		if strings.Contains(c.String(), "new-session") && tmuxSessionArg(c) == name {
			n++
		}
	}
	return n
}

// #6892: a tmux respawn is the SAME tab with a new process — identity (ID) is
// minted once at creation; incarnation (gen, #5571) is what the restart
// changes. The deterministic repro of the run-42244518786 shape: every sibling
// session is definitively absent (the tmux outage), setupTabs brings the roster
// back (a re-spawn IS emitted and observable), then an unrelated CloseTab
// republishes it — and every surviving id, in memory and in the serialized
// TabData clients reconcile on, is the one minted at creation. A lazy re-mint
// deferred to a later roster publish fails here.
func TestTabIDsSurviveRespawnAndUnrelatedClose(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, t.TempDir(), "id-stability", "id-stability-branch", "", false, false)
	require.NoError(t, err)

	live := map[string]bool{}
	pty := &spawningPtyFactory{t: t, live: live}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "has-session") {
				if !live[tmuxSessionArg(c)] {
					return fmt.Errorf("can't find session")
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}

	agentTmux := tmux.NewTmuxSessionWithDeps("id-stability", tmux.ProgramClaude, pty, cmdExec)
	live[agentTmux.SanitizedName()] = true // the respawn already re-established the agent
	agentTab := &Tab{ID: "agent-id", Name: agentTabName, Kind: TabKindAgent, tmux: agentTmux}
	shellName := agentTmux.SanitizedName() + tmuxTabSeparator + "shell"
	shellTab := &Tab{ID: "shell-id", Name: "peer-fit-2347", Kind: TabKindShell,
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(shellName, "bash", pty, cmdExec)}
	procName := agentTmux.SanitizedName() + tmuxTabSeparator + "scrollkeep"
	procTab := &Tab{ID: "proc-id", Name: "scrollkeep-2933", Kind: TabKindProcess, Command: "sleep 300",
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(procName, "sleep 300", pty, cmdExec)}

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

	// The respawn was actually exercised: a replacement process was launched for
	// the dead shell, and the roster now probes live.
	assert.GreaterOrEqual(t, newSessionCmdCount(pty, shellName), 1,
		"setupTabs must re-spawn the absent shell session, not leave the test on the unchanged tab")
	if exists, known := inst.Tabs[1].tmux.ProbeSession(); assert.True(t, known) {
		assert.True(t, exists, "the shell tab's session must exist after setupTabs")
	}

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
