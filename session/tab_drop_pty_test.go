package session

import (
	"os"
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDropTabTestInstance builds a started local instance with an agent tab and a
// restored shell tab, each owning its own attach PTY (opened by the
// close-tracking factory). files[0] is the agent PTY, files[1] the shell PTY, so
// a test can assert the drop path released the shell's without killing the
// session or touching the agent's.
func newDropTabTestInstance(t *testing.T) (*Instance, *closeTrackingPtyFactory) {
	t.Helper()
	log.Initialize(false)
	t.Cleanup(log.Close)

	cmdExec := cmd_test.MockCmdExec{
		// has-session succeeds → Restore takes the attach branch and opens a PTY.
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}
	pty := &closeTrackingPtyFactory{t: t}

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_drop_agent", "claude", pty, cmdExec)
	shellTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_drop_agent__shell", "/bin/sh", pty, cmdExec)
	for _, ts := range []*tmux.TmuxSession{agentTs, shellTs} {
		require.NoError(t, ts.Restore(""), "restore must attach and open a PTY")
	}
	pty.mu.Lock()
	nOpened := len(pty.files)
	pty.mu.Unlock()
	require.Equal(t, 2, nOpened, "each tab's restore must have opened its own attach PTY")

	shellTab := newShellTab(shellTs)
	shellTab.Name = "shell"
	inst := &Instance{
		Title:   "drop",
		backend: &LocalBackend{},
		started: true,
		Tabs:    []*Tab{newAgentTab(agentTs), shellTab},
	}
	return inst, pty
}

// assertShellPTYReleasedNoKill asserts the shell tab's attach PTY (files[1]) was
// closed while the agent's (files[0]) stayed open, and that no tmux kill-session
// ran — the drop paths only release the TUI's attach resources (#960), the
// daemon owns the teardown.
func assertShellPTYReleasedNoKill(t *testing.T, pty *closeTrackingPtyFactory) {
	t.Helper()
	pty.mu.Lock()
	files := append([]*os.File(nil), pty.files...)
	pty.mu.Unlock()
	require.Len(t, files, 2)
	assert.ErrorIs(t, files[1].Close(), os.ErrClosed,
		"dropping the shell tab must release its attach PTY (CloseAttachOnly), else the ptmx fd leaks (#1539)")
	assert.NoError(t, files[0].Close(),
		"the agent tab's PTY must NOT be touched — only the dropped tab is released")
}

// TestDropClosedTabReleasesAttachPTY is the #1539 leak guard for the DropClosedTab
// path: dropping a daemon-closed tab from the local view must release the TUI's
// attach PTY (ptmx fd + blocked cmd.Wait goroutine), not just splice it out of the
// slice. It must not kill the tmux session — the daemon already tore it down.
func TestDropClosedTabReleasesAttachPTY(t *testing.T) {
	inst, pty := newDropTabTestInstance(t)

	require.NoError(t, inst.DropClosedTab(1))
	require.Len(t, inst.GetTabs(), 1, "the closed tab must be dropped from the local view")

	assertShellPTYReleasedNoKill(t, pty)
}

// TestDropTabByNameReleasesAttachPTY is the same #1539 guard for the
// dropTabByName path (used by ReconcileTabsFromData when the daemon drops a tab
// out-of-band): removing the tab by name must also release its attach PTY.
func TestDropTabByNameReleasesAttachPTY(t *testing.T) {
	inst, pty := newDropTabTestInstance(t)

	require.True(t, inst.dropTabByName("shell"), "the named tab must be found and dropped")
	require.Len(t, inst.GetTabs(), 1, "the dropped tab must leave the local view")

	assertShellPTYReleasedNoKill(t, pty)
}
