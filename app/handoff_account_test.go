package app

import (
	"encoding/base64"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestHandoffOffersAgentAccounts(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", "claude"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "work"}, {Agent: "codex", Name: "foreign"}, {Agent: "claude", Name: "personal"}}, Defaults: map[string]string{"claude": "personal"}}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd, "handoff must load all target agents' account choices")
	h.Update(cmd())
	require.Contains(t, h.selectionOverlay.Render(), "personal")
	require.Contains(t, h.selectionOverlay.Render(), "codex: foreign")
	require.Contains(t, h.selectionOverlay.Render(), "project default")
}

// 80x24 before/after evidence uses the same deterministic renderer and clock
// as the design stills. Capture only inside the test container.
func TestHandoffAccountDesignScenes(t *testing.T) {
	configureDesignStillsOutput(t)
	for _, mode := range []string{"dark", "light"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := newDesignDriverSceneHome(t, mode, nil)
			inst := handoffActionInstance(t, "Continue migration", "claude")
			inst.CreatedAt = designStillsNow(t)
			h.store.AddInstance(inst)
			h.sidebar.SelectInstance(inst)
			h.termWidth, h.termHeight = 80, 24
			h.relayout()
			restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
				return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "personal"}, {Agent: "codex", Name: "work", LoggedIn: true}}, Defaults: map[string]string{"claude": "personal", "codex": "work"}}, nil
			})
			defer restore()
			_, cmd := h.handleHandoff()
			require.NotNil(t, cmd)
			for _, phase := range []string{"before", "after"} {
				if phase == "after" {
					h.Update(cmd())
				}
				frame := h.View()
				svg := recoverySVG(frame, mode, 80, 24)
				name := "handoff-account-" + phase + "-" + mode + ".svg"
				if out := os.Getenv("AF_TUI_DESIGN_CAPTURE"); out != "" {
					require.NoError(t, os.MkdirAll(out, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(out, name), []byte(svg), 0644))
					continue
				}
				golden, err := os.ReadFile(filepath.Join("testdata", "design", name))
				if err != nil || string(golden) != svg {
					t.Logf("CAPTURE %s %s", name, base64.StdEncoding.EncodeToString([]byte(svg)))
				}
				require.NoError(t, err)
				require.Equal(t, string(golden), svg)
			}
		})
	}
}

func TestHandoffPinnedAccountsAndCredentialWarning(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", "claude")
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(agent, _ string) (daemon.ListAccountsResponse, error) {
		require.Empty(t, agent, "pinned handoff needs the target agents' registry too")
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: "claude", Name: "work", LoggedIn: true},
			{Agent: "claude", Name: "personal", LoggedIn: false},
			{Agent: "codex", Name: "spare", LoggedIn: true},
		}, Defaults: map[string]string{"claude": "personal"}}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	require.Empty(t, h.handoffChoices, "pinned sessions cannot submit ambient rows while loading")
	h.Update(cmd())
	require.Equal(t, []string{"claude", "codex"}, h.handoffChoices)
	require.Equal(t, []string{"personal", "spare"}, h.handoffAccounts)
	require.Contains(t, h.selectionOverlay.Render(), "not logged in")
	require.Equal(t, 1, h.selectionOverlay.GetSelectedIndex(), "unauthenticated default must not be preselected")
	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	require.Contains(t, h.confirmationOverlay.Render(), "no claude credential yet")
}

func TestHandoffCredentialWarning(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", "claude"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{{Agent: "claude", Name: "personal"}}, Defaults: map[string]string{"claude": "personal"}}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Contains(t, h.selectionOverlay.Render(), "not logged in")
	require.NotEqual(t, 0, h.selectionOverlay.GetSelectedIndex())
	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	require.Contains(t, h.confirmationOverlay.Render(), "no claude credential yet")
}

// TestHandoffReservedRootOffersAccountOnlyRows pins #4433's picker mode: the
// reserved root can never change agent, so `F` on it opens the account-only
// form — the running agent's other accounts, named plainly, with no agent rows
// to refuse at submit and no ambient row. The confirmation names the operation
// as an account switch and says the conversation does not carry.
func TestHandoffReservedRootOffersAccountOnlyRows(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, session.RootSessionTitle, "codex")
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: "codex", Name: "work", LoggedIn: true},
			{Agent: "codex", Name: "personal", LoggedIn: true},
			{Agent: "claude", Name: "other-agent-account", LoggedIn: true},
		}, Defaults: map[string]string{"codex": "personal"}}, nil
	})
	defer restore()

	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd, "the reserved root must reach the account-only picker, not the agent refusal")
	require.Equal(t, stateSelectHandoffAgent, h.state)
	require.True(t, h.handoffAccountOnly)
	require.Empty(t, h.handoffChoices, "account-only mode never submits agent rows while loading")
	h.Update(cmd())

	rendered := h.selectionOverlay.Render()
	require.Contains(t, rendered, "Switch account to")
	require.Contains(t, rendered, "personal")
	require.NotContains(t, rendered, "other-agent-account", "another agent's account is not a same-agent move")
	require.NotContains(t, rendered, "work", "the account root already runs as is not a move")
	require.NotContains(t, rendered, "ambient", "descoping root is not an operation this surface offers")
	require.Equal(t, []string{"codex"}, h.handoffChoices, "every row resolves to the running agent")
	require.Equal(t, []string{"personal"}, h.handoffAccounts)

	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	confirm := h.confirmationOverlay.Render()
	require.Contains(t, confirm, `Switch "root" to codex account "personal"?`)
	require.Contains(t, confirm, "codex stays the agent")
	require.Contains(t, confirm, "fresh conversation")
}
