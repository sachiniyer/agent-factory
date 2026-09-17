package session

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// processTabRestoreInstance builds a started local instance whose agent tab and
// one process tab share a mock-backed executor and a recording PTY factory, so
// a test can script has-session/display-message answers per session name and
// observe every spawn attempt setupTabs emits. new-session travels through the
// PTY factory, not the executor — spawn detection reads pty.cmds.
func processTabRestoreInstance(t *testing.T, cmdExec cmd_test.MockCmdExec) (*Instance, *Tab, *recordingPtyFactory) {
	t.Helper()
	repoRoot := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, t.TempDir(), "proc-restore", "proc-restore-branch", "", false, false)
	require.NoError(t, err)
	pty := &recordingPtyFactory{t: t}
	agentTmux := tmux.NewTmuxSessionWithDeps("proc-restore", tmux.ProgramClaude, pty, cmdExec)
	procTmux := agentTmux.NewSiblingSession("proc-restore-proc", "echo hi")
	procTab := &Tab{ID: "proc-tab", Name: "proc", Kind: TabKindProcess, Command: "echo hi", tmux: procTmux}
	inst := &Instance{
		ID:          "proc-restore-id",
		Title:       "proc-restore",
		Path:        repoRoot,
		Program:     tmux.ProgramClaude,
		backend:     &LocalBackend{},
		Tabs:        []*Tab{newAgentTab(agentTmux), procTab},
		gitWorktree: gw,
		started:     true,
		liveness:    LiveRunning,
	}
	return inst, procTab, pty
}

func newSessionCount(pty *recordingPtyFactory) int {
	n := 0
	for _, c := range pty.cmds {
		if strings.Contains(strings.Join(c.Args, " "), "new-session") {
			n++
		}
	}
	return n
}

// A process tab whose tmux session is definitively absent must come back inert:
// setupTabs neither re-executes the command nor stamps an exit it never
// observed (#4479).
func TestProcessTabRestoreAbsentSessionStaysInert(t *testing.T) {
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
	inst, procTab, pty := processTabRestoreInstance(t, cmdExec)

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	assert.Zero(t, newSessionCount(pty), "a missing process session must never re-spawn its command")
	assert.Nil(t, inst.Tabs[1].Exit)
	assert.Same(t, procTab.tmux, inst.Tabs[1].tmux, "the inert tab keeps its persisted tmux binding")
}

// A process tab whose held pane is dead gets its exit status and death time
// stamped from pane_dead, and its command is still never re-executed (#4479).
func TestProcessTabRestoreDeadPaneStampsExit(t *testing.T) {
	deadAt := time.Unix(1726000000, 0)
	var remainOnExit bool
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), "remain-on-exit") {
				remainOnExit = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "pane_dead") {
				return []byte(paneExitAnswer(c, "1", "42", "1726000000")), nil
			}
			return nil, nil
		},
	}
	inst, _, pty := processTabRestoreInstance(t, cmdExec)

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	require.NotNil(t, inst.Tabs[1].Exit, "a pane observed dead must record its exit")
	assert.Equal(t, 42, inst.Tabs[1].Exit.Status)
	assert.True(t, inst.Tabs[1].Exit.StatusKnown)
	assert.Equal(t, deadAt, inst.Tabs[1].Exit.At)
	assert.True(t, remainOnExit, "restore must heal remain-on-exit on a pre-#4479 pane")
	assert.Zero(t, newSessionCount(pty))
}

// A live pane stamps nothing and reattaches without spawning.
func TestProcessTabRestoreLivePaneReattachesOnly(t *testing.T) {
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "pane_dead") {
				return []byte("0"), nil
			}
			return nil, nil
		},
	}
	inst, _, pty := processTabRestoreInstance(t, cmdExec)

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	assert.Nil(t, inst.Tabs[1].Exit, "a live pane must not be stamped finished")
	assert.Zero(t, newSessionCount(pty))
}

// The contrast that pins the bug: under the identical missing-session answer a
// SHELL tab still gets a fresh sibling spawned, because an absent interactive
// session is an interruption to repair — while the process tab above stays
// inert (#4479).
func TestShellTabRestoreAbsentSessionStillRespawns(t *testing.T) {
	// new-session rides the PTY factory, so the mock executor cannot observe the
	// spawn directly — has-session answers instead read the factory's recorded
	// commands, flipping once the shell's new-session has been emitted.
	var pty *recordingPtyFactory
	shellSpawned := func() bool {
		if pty == nil {
			return false
		}
		for _, c := range pty.cmds {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "new-session") && strings.Contains(joined, "proc-restore-shell") {
				return true
			}
		}
		return false
	}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "has-session") {
				if strings.Contains(joined, "proc-restore-proc") {
					return errors.New("can't find session")
				}
				if strings.Contains(joined, "proc-restore-shell") && !shellSpawned() {
					return errors.New("can't find session")
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return []byte("output"), nil
		},
	}
	inst, _, p := processTabRestoreInstance(t, cmdExec)
	pty = p
	shellTmux, err := inst.Tabs[0].tmux.NewShellSiblingSession("proc-restore-shell", "/bin/sh")
	require.NoError(t, err)
	inst.Tabs = append(inst.Tabs, &Tab{ID: "shell-tab", Name: "shell", Kind: TabKindShell, tmux: shellTmux})

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	assert.Positive(t, newSessionCount(pty), "a missing shell session must still spawn a fresh shell (#386/#991)")
	assert.Nil(t, inst.Tabs[1].Exit)
}

// TabExit survives the storage round trip in both directions (#4479).
func TestProcessTabExitStorageRoundTrip(t *testing.T) {
	when := time.Unix(1726000000, 0).UTC()
	data := InstanceData{
		Title: "proc-rt",
		Tabs: []TabData{
			{Name: "agent", Kind: TabKindAgent},
			{
				ID:      "p1",
				Name:    "deploy",
				Kind:    TabKindProcess,
				Command: "./deploy.sh",
				Exit:    &TabExitData{Status: 3, StatusKnown: true, At: when},
			},
		},
	}
	inst := &Instance{Title: "proc-rt"}
	restoreLocalTabs(inst, data)
	require.Len(t, inst.Tabs, 2)
	require.NotNil(t, inst.Tabs[1].Exit)
	assert.Equal(t, 3, inst.Tabs[1].Exit.Status)
	assert.True(t, inst.Tabs[1].Exit.StatusKnown)
	assert.Equal(t, when, inst.Tabs[1].Exit.At)

	out := inst.ToInstanceData()
	require.Len(t, out.Tabs, 2)
	require.NotNil(t, out.Tabs[1].Exit)
	assert.Equal(t, 3, out.Tabs[1].Exit.Status)
	assert.True(t, out.Tabs[1].Exit.StatusKnown)
	assert.Equal(t, when, out.Tabs[1].Exit.At)
}
