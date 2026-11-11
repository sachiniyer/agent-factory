package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// #7084 review, P3 wording: a LOCAL project whose default program could not be
// read asks the daemon for it (#7079), so its notices must neither name a
// remote daemon nor quote an empty program.

func TestLocalUnknownDefaultSubmitSaysItIsAskingTheDaemon(t *testing.T) {
	clearDaemonTarget(t)
	h := newTestHome(t)
	h.errBox.SetSize(500, 1) // the bar renders nothing at zero size
	switchToUnreadableProject(t, h)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{}, nil
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	got := recordStartRequest(t)

	// The daemon's answer is still in flight: its fetch command is not run.
	_, _ = h.startNewInstance()
	require.True(t, h.remoteDefaultPending, "precondition: the form is waiting on the daemon's answer")
	require.NoError(t, h.namingInstance.SetTitle("demo"))
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	require.Empty(t, got.Title, "the submit waits for the answer")
	notice := h.errBox.String()
	require.Contains(t, notice, "asking the daemon")
	require.NotContains(t, notice, "remote", "a local project has no remote daemon to name")
}

func TestAccountPickerNamesAnUnknownDefaultPlainly(t *testing.T) {
	clearDaemonTarget(t)
	h := newTestHome(t)
	h.errBox.SetSize(500, 1) // the bar renders nothing at zero size
	switchToUnreadableProject(t, h)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{}, nil // the daemon cannot read it either
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))
	_, cmd := h.startNewInstance()
	for _, msg := range drainCmd(t, cmd, time.Second) {
		_, _ = h.Update(msg)
	}
	require.Empty(t, h.pendingProgram, "precondition: no default program is known")

	_, _ = h.openAccountPicker()

	notice := h.errBox.String()
	require.Contains(t, notice, "not known yet")
	require.NotContains(t, notice, `""`, "an empty program is not quoted at the user")
}
