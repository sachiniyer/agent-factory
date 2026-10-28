package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

func remoteAccountRegisterHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	previousURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = "https://accounts.example.test"
	t.Cleanup(func() { apiclient.FlagDaemonURL = previousURL })
	sizeConfigPane(h)
	h.configPane.SetAccounts([]ui.AccountRow{{Agent: "codex", Name: "existing"}}, []string{"codex"}, nil)
	h.configPane.SetFocus(true)
	h.state = stateConfigEditor
	return h
}

func registeredAccountResponse(name string) daemon.RegisterAccountResponse {
	return daemon.RegisterAccountResponse{
		Entry:   daemon.AccountEntry{Agent: "codex", Name: name, Dir: "/remote/accounts/" + name},
		Notices: []string{"Whole home relocation."},
	}
}

func TestAccountRegisterRemoteDefersRegisterAndRefreshUntilCommand(t *testing.T) {
	h := remoteAccountRegisterHome(t)
	var calls []string
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			calls = append(calls, "list")
			return daemon.ListAccountsResponse{
				Entries: []daemon.AccountEntry{{Agent: "codex", Name: "fresh"}}, Agents: []string{"codex"},
			}, nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			require.Equal(t, daemon.RegisterAccountRequest{Agent: "codex", Name: "fresh"}, req)
			calls = append(calls, "register")
			return registeredAccountResponse(req.Name), nil
		}, nil,
	))
	cmd := h.handleAccountRequest(ui.AccountRequest{Kind: ui.AccountRequestRegister, Agent: "codex", Name: "fresh"})
	require.NotNil(t, cmd)
	require.Empty(t, calls, "neither remote round trip may run on the UI loop")
	before := h.configPane.String()
	msg := cmd()
	require.Equal(t, []string{"register", "list"}, calls)
	require.Equal(t, before, h.configPane.String(), "the worker must not mutate the pane")
	h.Update(msg)
	view := h.configPane.String()
	require.Contains(t, view, `Registered codex account "fresh"`)
	require.Contains(t, view, "Whole home relocation.")
	require.NotContains(t, view, "existing")
	require.True(t, h.configPane.HasFocus())
	require.Equal(t, stateConfigEditor, h.state)
}

func TestAccountRegisterRemoteStallsLeaveUIResponsive(t *testing.T) {
	for _, stage := range []string{"register", "refresh"} {
		t.Run(stage, func(t *testing.T) {
			h := remoteAccountRegisterHome(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			block := func() {
				close(entered)
				<-release
			}
			t.Cleanup(SetAccountSeamsForTest(
				func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
					if stage == "refresh" {
						block()
					}
					return daemon.ListAccountsResponse{Agents: []string{"codex"}}, nil
				},
				func(daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
					if stage == "register" {
						block()
					}
					return registeredAccountResponse("fresh"), nil
				}, nil,
			))
			cmd := h.handleAccountRegister("codex", "fresh")
			require.NotNil(t, cmd)
			completed := make(chan tea.Msg, 1)
			go func() { completed <- cmd() }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("remote command did not reach the stalled call")
			}
			require.Contains(t, h.configPane.String(), "Accounts", "the pane can redraw during the round trip")
			h.Update(tea.KeyMsg{Type: tea.KeyEsc})
			require.False(t, h.configPane.HasFocus(), "Esc must close the pane during either remote wait")
			require.Equal(t, stateDefault, h.state)
			closed := h.configPane.String()
			unblock()
			select {
			case msg := <-completed:
				h.Update(msg)
			case <-time.After(5 * time.Second):
				t.Fatal("remote command did not complete after release")
			}
			require.Equal(t, closed, h.configPane.String(), "late completion must not change a closed pane")
			require.False(t, h.configPane.HasFocus())
			require.Equal(t, stateDefault, h.state)
		})
	}
}

func TestAccountRegisterRemoteErrorsStayInTheSection(t *testing.T) {
	for _, stage := range []string{"register", "refresh"} {
		t.Run(stage, func(t *testing.T) {
			h := remoteAccountRegisterHome(t)
			var listCalls atomic.Int32
			t.Cleanup(SetAccountSeamsForTest(
				func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
					listCalls.Add(1)
					return daemon.ListAccountsResponse{}, errors.New("account refresh unavailable")
				},
				func(daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
					if stage == "register" {
						return daemon.RegisterAccountResponse{}, errors.New("account registration refused")
					}
					return registeredAccountResponse("fresh"), nil
				}, nil,
			))
			cmd := h.handleAccountRegister("codex", "fresh")
			require.NotNil(t, cmd)
			before := h.configPane.String()
			msg := cmd()
			require.Equal(t, before, h.configPane.String())
			h.Update(msg)
			if stage == "register" {
				require.Contains(t, h.configPane.String(), "account registration refused")
				require.Zero(t, listCalls.Load(), "failed registration must not trigger a refresh")
			} else {
				require.Contains(t, h.configPane.String(), "account refresh unavailable")
				require.Contains(t, h.configPane.String(), `Registered codex account "fresh"`)
				require.Equal(t, int32(1), listCalls.Load())
			}
			require.True(t, h.configPane.HasFocus())
			require.Equal(t, stateConfigEditor, h.state)
		})
	}
}

func TestAccountRegisterRemoteIgnoresCompletionAfterReopen(t *testing.T) {
	h := remoteAccountRegisterHome(t)
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			return daemon.ListAccountsResponse{Agents: []string{"codex"}}, nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			return registeredAccountResponse(req.Name), nil
		}, nil,
	))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/GetConfig" {
			t.Errorf("unexpected remote request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.GetConfigResponse{
			Path:    "/remote/config.toml",
			Entries: []config.ConfigEntry{{Key: "default_program", Value: "codex", Tier: 1}},
		}))
	}))
	t.Cleanup(server.Close)
	apiclient.FlagDaemonURL = server.URL
	cmd := h.handleAccountRegister("codex", "old")
	require.NotNil(t, cmd)
	msg := cmd()
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	h.showConfigEditor()
	sizeConfigPane(h)
	require.True(t, h.configPane.HasFocus())
	h.configPane.SetAccountStatus("Current opening", false)
	before := h.configPane.String()
	h.Update(msg)
	require.Equal(t, before, h.configPane.String(), "a previous opening's result must not overwrite current feedback")
	require.Equal(t, stateConfigEditor, h.state)
}

func TestAccountRegisterRemoteRejectsOverlappingRequest(t *testing.T) {
	h := remoteAccountRegisterHome(t)
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			return daemon.ListAccountsResponse{Agents: []string{"codex"}}, nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			return registeredAccountResponse(req.Name), nil
		}, nil,
	))
	first := h.handleAccountRegister("codex", "first")
	second := h.handleAccountRegister("codex", "second")
	require.NotNil(t, first)
	require.Nil(t, second, "one remote mutation may run at a time")
	require.Contains(t, h.configPane.String(), `Registering codex account "first"…`)
	h.Update(first())
	require.Contains(t, h.configPane.String(), `Registered codex account "first"`)
}
