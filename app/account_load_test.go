package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/require"
)

func remoteAccountLoadHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/GetConfig" {
			t.Errorf("unexpected remote request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.GetConfigResponse{
			Path: "/remote/config.toml", Entries: []config.ConfigEntry{{Key: "default_program", Value: "codex", Tier: 1}},
		}))
	}))
	t.Cleanup(server.Close)
	previousURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = server.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = previousURL })
	h.configPane.SetAccounts([]ui.AccountRow{{Agent: "codex", Name: "previous-opening"}}, []string{"codex"}, nil)
	return h
}

func TestAccountLoadRemoteDefersCompletionToUILoop(t *testing.T) {
	h := remoteAccountLoadHome(t)
	calls := 0
	t.Cleanup(SetAccountSeamsForTest(func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
		calls++
		return daemon.ListAccountsResponse{
			Entries: []daemon.AccountEntry{{Agent: "codex", Name: "fresh-account"}}, Agents: []string{"codex"},
		}, nil
	}, nil, nil))
	_, cmd := h.showConfigEditor()
	require.Zero(t, calls, "opening the remote pane must not run ListAccounts on the UI loop")
	require.NotNil(t, cmd)
	sizeConfigPane(h)
	before := h.configPane.String()
	require.Contains(t, before, "Loading accounts…")
	require.NotContains(t, before, "previous-opening", "pending loads must clear the old opening's rows")
	msg := cmd()
	require.Equal(t, 1, calls)
	require.Equal(t, before, h.configPane.String(), "the command must not mutate the pane")
	h.Update(msg)
	require.Contains(t, h.configPane.String(), "fresh-account")
	require.NotContains(t, h.configPane.String(), "Loading accounts…")
	require.True(t, h.configPane.HasFocus())
	require.Equal(t, stateConfigEditor, h.state)
}

func TestAccountLoadRemoteStallLeavesUIResponsive(t *testing.T) {
	h := remoteAccountLoadHome(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	t.Cleanup(SetAccountSeamsForTest(func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
		close(entered)
		<-release
		return daemon.ListAccountsResponse{Agents: []string{"codex"}}, nil
	}, nil, nil))
	opened := make(chan tea.Cmd, 1)
	go func() {
		_, cmd := h.showConfigEditor()
		opened <- cmd
	}()
	var cmd tea.Cmd
	select {
	case cmd = <-opened:
	case <-time.After(time.Second):
		// Drain the pre-fix synchronous path before cleanup restores shared seams.
		unblock()
		<-opened
		t.Fatal("opening the pane blocked on remote ListAccounts")
	}
	require.NotNil(t, cmd)
	completed := make(chan tea.Msg, 1)
	go func() { completed <- cmd() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("remote command did not reach ListAccounts")
	}
	sizeConfigPane(h)
	require.Contains(t, h.configPane.String(), "Loading accounts…", "the pane can redraw while ListAccounts stalls")
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.False(t, h.configPane.HasFocus(), "Esc must close the pane during ListAccounts")
	require.Equal(t, stateDefault, h.state)
	closed := h.configPane.String()
	unblock()
	select {
	case msg := <-completed:
		h.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("ListAccounts did not complete after release")
	}
	require.Equal(t, closed, h.configPane.String(), "late completion must not mutate a closed pane")
	require.False(t, h.configPane.HasFocus())
	require.Equal(t, stateDefault, h.state)
}

func TestAccountLoadRemoteErrorStaysInSection(t *testing.T) {
	h := remoteAccountLoadHome(t)
	t.Cleanup(SetAccountSeamsForTest(func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, errors.New("remote account listing unavailable")
	}, nil, nil))
	_, cmd := h.showConfigEditor()
	sizeConfigPane(h)
	require.NotContains(t, h.configPane.String(), "remote account listing unavailable", "listing is deferred until the command runs")
	require.NotNil(t, cmd)
	before := h.configPane.String()
	msg := cmd()
	require.Equal(t, before, h.configPane.String(), "the command must not display the error itself")
	h.Update(msg)
	require.Contains(t, h.configPane.String(), "Accounts could not be read: remote account listing unavailable")
	require.NotContains(t, h.configPane.String(), "Loading accounts…")
	require.True(t, h.configPane.HasFocus())
	require.Equal(t, stateConfigEditor, h.state)
}

func TestAccountLoadRemoteIgnoresCompletionAfterReopen(t *testing.T) {
	h := remoteAccountLoadHome(t)
	calls := 0
	t.Cleanup(SetAccountSeamsForTest(func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
		calls++
		name := "old-opening"
		if calls > 1 {
			name = "current-opening"
		}
		return daemon.ListAccountsResponse{
			Entries: []daemon.AccountEntry{{Agent: "codex", Name: name}}, Agents: []string{"codex"},
		}, nil
	}, nil, nil))
	_, first := h.showConfigEditor()
	require.NotNil(t, first, "remote opening must return its load command")
	old := first()
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_, second := h.showConfigEditor()
	require.NotNil(t, second)
	sizeConfigPane(h)
	pending := h.configPane.String()
	h.Update(old)
	require.Equal(t, pending, h.configPane.String(), "an earlier opening's completion must not replace the pending load")
	h.Update(second())
	current := h.configPane.String()
	require.Contains(t, current, "current-opening")
	h.Update(old)
	require.Equal(t, current, h.configPane.String(), "an earlier opening's completion must not replace current accounts")
	require.True(t, h.configPane.HasFocus())
	require.Equal(t, stateConfigEditor, h.state)
}
