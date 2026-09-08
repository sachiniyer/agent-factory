package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

func TestHandoffCompletionReportsAccountPair(t *testing.T) {
	for _, tc := range []struct{ name, from, to, fromAccount, toAccount, want string }{
		{"same agent", "claude", "claude", "work", "personal", "'worker' handed from claude (work) to claude (personal)"},
		{"cross agent", "codex", "claude", "work", "personal", "'worker' handed from codex (work) to claude (personal)"},
		{"ambient", "codex", "claude", "", "", "'worker' handed from codex to claude"},
		{"ambient to account", "codex", "claude", "", "personal", "'worker' handed from codex to claude (personal)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/v1/HandoffSession", r.URL.Path)
				require.NoError(t, apiproto.WriteEnvelope(w, apiproto.Success(daemon.HandoffSessionResponse{OK: true, From: tc.from, To: tc.to, FromAccount: tc.fromAccount, ToAccount: tc.toAccount})))
			}))
			defer server.Close()
			previousURL := apiclient.FlagDaemonURL
			apiclient.FlagDaemonURL = server.URL
			defer func() { apiclient.FlagDaemonURL = previousURL }()
			msg := h.handoffCmd(daemon.HandoffSessionRequest{Title: "worker", To: tc.to, Account: tc.toAccount})().(handoffDoneMsg)
			require.NoError(t, msg.err)
			h.handleHandoffDone(msg)
			require.Equal(t, tc.want, h.errBox.FullError())
		})
	}
}

func TestHandoffAmbientOffersTargetAccounts(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", "claude"))
	h.sidebar.SetSelectedInstance(0)
	query := "not called"
	defer SetAccountListerForTest(func(agent, _ string) (daemon.ListAccountsResponse, error) {
		query = agent
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: "codex", Name: "work", LoggedIn: true},
			{Agent: "codex", Name: "personal", LoggedIn: false},
			{Agent: "codex", Name: "unavailable", RegistrationOnly: true},
		}, Defaults: map[string]string{"codex": "work"}}, nil
	})()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Empty(t, query, "ambient handoffs must load every target agent's accounts")
	require.Equal(t, []string{"codex", "codex", "codex"}, h.handoffChoices[:3])
	require.Equal(t, []string{"", "work", "personal"}, h.handoffAccounts[:3], "ambient option stays first")
	require.Equal(t, 1, h.selectionOverlay.GetSelectedIndex(), "logged-in project default is selected")
	require.Contains(t, h.selectionOverlay.Render(), "codex: work (project default)")
	require.Contains(t, h.selectionOverlay.Render(), "codex: personal (not logged in)")
	require.NotContains(t, h.selectionOverlay.Render(), "unavailable")
}
