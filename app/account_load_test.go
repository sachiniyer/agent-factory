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
		switch r.URL.Path {
		case "/v1/GetConfig":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.GetConfigResponse{
				Path: "/remote/config.toml", Entries: []config.ConfigEntry{{Key: "default_program", Value: "codex", Tier: 1}},
			}))
		case "/v1/QuotaReport":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.QuotaReportResponse{}))
		case "/v1/ListAccounts":
			// remoteSectionsLoadCmd reads Accounts and Usage in one batched
			// command, so a remote Usage test still owes this route a stub even
			// when the accounts seam is the one under test (#4361).
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.ListAccountsResponse{}))
		default:
			t.Errorf("unexpected remote request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	previousURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = server.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = previousURL })
	h.configPane.SetAccounts([]ui.AccountRow{{Agent: "codex", Name: "previous-opening"}}, []string{"codex"}, nil)
	return h
}

// sectionsMsgs runs a remoteSectionsLoadCmd the way the runtime does: the two
// section reads are batched, so the command's message is a tea.BatchMsg whose
// inner commands each produce their own section's message. Running the inner
// commands is what performs the reads.
func sectionsMsgs(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	require.NotNil(t, cmd)
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	require.True(t, ok, "remoteSectionsLoadCmd must batch the per-section reads, got %T", msg)
	msgs := make([]tea.Msg, 0, len(batch))
	for _, inner := range batch {
		msgs = append(msgs, inner())
	}
	return msgs
}

func updateAll(h *home, msgs []tea.Msg) {
	for _, msg := range msgs {
		h.Update(msg)
	}
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
	msgs := sectionsMsgs(t, cmd)
	require.Equal(t, 1, calls)
	require.Equal(t, before, h.configPane.String(), "the command must not mutate the pane")
	updateAll(h, msgs)
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
	batched := make(chan tea.Msg, 1)
	go func() { batched <- cmd() }()
	var batch tea.BatchMsg
	select {
	case msg := <-batched:
		var ok bool
		batch, ok = msg.(tea.BatchMsg)
		require.True(t, ok, "remote sections load must batch its per-section reads, got %T", msg)
	case <-time.After(5 * time.Second):
		t.Fatal("remote command did not return its batch")
	}
	// Drive the per-section reads the way the runtime does — concurrently —
	// and collect each section's own completion.
	sectionMsgs := make(chan tea.Msg, len(batch))
	for _, inner := range batch {
		go func(c tea.Cmd) { sectionMsgs <- c() }(inner)
	}
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
	for range batch {
		select {
		case msg := <-sectionMsgs:
			h.Update(msg)
		case <-time.After(5 * time.Second):
			t.Fatal("the section reads did not complete after release")
		}
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
	msgs := sectionsMsgs(t, cmd)
	require.Equal(t, before, h.configPane.String(), "the command must not display the error itself")
	updateAll(h, msgs)
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
	stale := sectionsMsgs(t, first)
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_, second := h.showConfigEditor()
	require.NotNil(t, second)
	sizeConfigPane(h)
	pending := h.configPane.String()
	updateAll(h, stale)
	require.Equal(t, pending, h.configPane.String(), "an earlier opening's completion must not replace the pending load")
	updateAll(h, sectionsMsgs(t, second))
	current := h.configPane.String()
	require.Contains(t, current, "current-opening")
	updateAll(h, stale)
	require.Equal(t, current, h.configPane.String(), "an earlier opening's completion must not replace current accounts")
	require.True(t, h.configPane.HasFocus())
	require.Equal(t, stateConfigEditor, h.state)
}
