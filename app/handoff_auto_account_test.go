package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// handoffAutoAccountInstance returns a ready, handoff-capable session whose
// account was chosen by the limit scheduler (accountAutoSelected=true), mirrored
// onto a client projection the way sync.go mirrors the daemon snapshot via
// ReconcileAccountHandoffSnapshot. An auto account is reversible, so the TUI
// must treat it like a no-account session for handoff admission — mirroring the
// daemon's `account != "" && !automatic` gate in daemon/handoff.go.
func handoffAutoAccountInstance(t *testing.T, title, program, account string) *session.Instance {
	t.Helper()
	inst := handoffActionInstance(t, title, program)
	require.True(t, inst.ReconcileAccountHandoffSnapshot(account, true, nil),
		"fixture requires the auto account to actually change the projection")
	acct, automatic := inst.AccountSelection()
	require.Equal(t, account, acct)
	require.True(t, automatic, "fixture requires the account to be scheduler-selected, not a manual pin")
	return inst
}

// TestHandoffAutoAccountOffersAmbientRows is the core regression: a scheduler-
// selected account must NOT be treated as a manual pin, so agent-only ("ambient")
// handoff rows stay available for every other supported agent — matching the
// daemon, which admits `af sessions handoff <title> --to <agent>` (no --account)
// for auto accounts. Before the fix the TUI suppressed these rows and the agent
// picker offered only registered account rows.
func TestHandoffAutoAccountOffersAmbientRows(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffAutoAccountInstance(t, "worker", tmux.ProgramClaude, "work"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: tmux.ProgramClaude, Name: "work", LoggedIn: true},
			{Agent: tmux.ProgramClaude, Name: "personal", LoggedIn: true},
			{Agent: tmux.ProgramCodex, Name: "spare", LoggedIn: true},
		}}, nil
	})
	defer restore()

	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd)
	h.Update(cmd())

	require.NotNil(t, h.selectionOverlay, "an auto account must not refuse the handoff")
	require.Equal(t, stateSelectHandoffAgent, h.state)
	rendered := h.selectionOverlay.Render()
	require.Contains(t, rendered, tmux.ProgramCodex+" (ambient)",
		"auto-accounted sessions must keep the agent-only (ambient) handoff option")
	require.Contains(t, rendered, "codex: spare", "registered target accounts are still offered")
	require.Contains(t, h.handoffAccounts, "", "the ambient row carries an empty (agent-only) account")
	require.NotContains(t, rendered, "Loading accounts…")
}

// TestHandoffAutoAccountKeepsAgentWithoutRegisteredAccount verifies the exact
// "drops every target agent that has no registered account" consequence: with an
// auto account and NO codex account registered anywhere, codex must still be
// offered through its ambient (agent-only) row, the way `af sessions handoff --to
// codex` would succeed against the daemon.
func TestHandoffAutoAccountKeepsAgentWithoutRegisteredAccount(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffAutoAccountInstance(t, "worker", tmux.ProgramClaude, "work"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: tmux.ProgramClaude, Name: "work", LoggedIn: true},
			{Agent: tmux.ProgramClaude, Name: "personal", LoggedIn: true},
		}}, nil
	})
	defer restore()

	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd)
	h.Update(cmd())

	require.NotNil(t, h.selectionOverlay, "an auto account must not refuse the handoff")
	rendered := h.selectionOverlay.Render()
	require.Contains(t, rendered, tmux.ProgramCodex+" (ambient)",
		"a target agent with no registered account must still be offered via its ambient row")
	require.NotContains(t, rendered, "codex:", "no codex account is registered, so no codex account row should appear")
}

// TestHandoffAutoAccountNotRefusedWithOnlyCurrentAccount verifies the "refuses
// the handoff entirely when no other account is registered" consequence. An auto
// account is reversible: even if the only registered account is the one the
// scheduler chose, the TUI must keep offering agent-only handoff rather than a
// dead-end refusal — the daemon admits the same handoff.
func TestHandoffAutoAccountNotRefusedWithOnlyCurrentAccount(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffAutoAccountInstance(t, "worker", tmux.ProgramClaude, "work"))
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: tmux.ProgramClaude, Name: "work", LoggedIn: true},
		}}, nil
	})
	defer restore()

	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd)
	h.Update(cmd())

	require.NotNil(t, h.selectionOverlay,
		"an auto account with no other registered accounts must not be a dead-end refusal")
	require.Equal(t, stateSelectHandoffAgent, h.state)
	require.NotContains(t, h.errBox.FullError(), "no registered target account",
		"the daemon-admitted agent-only handoff must not be refused by the TUI")
	require.Contains(t, h.selectionOverlay.Render(), tmux.ProgramCodex+" (ambient)")
}

// TestHandoffAutoAccountDoesNotForceAccountPickerWhileLoading locks the
// handleHandoff gate (app/handle_handoff.go): only a manual pin clears
// handoffChoices and shows the "Loading accounts…" placeholder. An auto account
// keeps the full agent list so the picker can be rebuilt with ambient rows.
func TestHandoffAutoAccountDoesNotForceAccountPickerWhileLoading(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffAutoAccountInstance(t, "worker", tmux.ProgramClaude, "work"))
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.handleHandoff()
	require.NotEmpty(t, h.handoffChoices,
		"an auto account must not be forced into the account-picker loading state")
	require.NotContains(t, h.selectionOverlay.Render(), "Loading accounts…")
}

// TestHandoffAutoAccountAmbientSubmitDispatchesAgentOnly is the end-to-end
// parity check: picking an ambient (agent-only) row for an auto-accounted
// session and confirming must dispatch a handoff request with an empty account —
// exactly what `af sessions handoff <title> --to codex` sends and the daemon
// admits under its `account != "" && !automatic` guard.
func TestHandoffAutoAccountAmbientSubmitDispatchesAgentOnly(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffAutoAccountInstance(t, "worker", tmux.ProgramClaude, "work"))
	h.sidebar.SetSelectedInstance(0)
	var got daemon.HandoffSessionRequest
	restore := SetHandoffRunnerForTest(func(req daemon.HandoffSessionRequest) (daemon.HandoffSessionResponse, error) {
		got = req
		return daemon.HandoffSessionResponse{From: tmux.ProgramClaude, To: req.To}, nil
	})
	defer restore()
	restoreList := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: tmux.ProgramClaude, Name: "work", LoggedIn: true},
			{Agent: tmux.ProgramCodex, Name: "spare", LoggedIn: true},
		}}, nil
	})
	defer restoreList()

	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd)
	h.Update(cmd())

	idx := -1
	for i, agent := range h.handoffChoices {
		if agent == tmux.ProgramCodex && i < len(h.handoffAccounts) && h.handoffAccounts[i] == "" {
			idx = i
			break
		}
	}
	require.GreaterOrEqual(t, idx, 0, "codex ambient row must be present")
	h.selectionOverlay.SetSelectedIndex(idx)
	_, _ = h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateConfirm, h.state, "picking raises the confirmation dialog")

	_, cmd = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd)
	start, ok := cmd().(startHandoffMsg)
	require.True(t, ok)
	require.Equal(t, tmux.ProgramCodex, start.request.To)
	require.Empty(t, start.request.Account, "an agent-only handoff carries no account (CLI parity)")

	msg := h.handoffCmd(start.request)()
	done, ok := msg.(handoffDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, tmux.ProgramCodex, got.To)
	require.Empty(t, got.Account, "the dispatched handoff request is agent-only")
}

// TestHandoffManualPinStillRefusedWithNoOtherAccounts is the regression guard
// that the fix only relaxes the AUTO path: a manually-pinned session (an explicit
// --account) with no other registered account must still be refused — the
// daemon's `account != "" && !automatic` guard rejects an agent-only handoff for
// a manual pin, so the TUI must not open the picker either.
func TestHandoffManualPinStillRefusedWithNoOtherAccounts(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{Entries: []daemon.AccountEntry{
			{Agent: tmux.ProgramClaude, Name: "work", LoggedIn: true},
		}}, nil
	})
	defer restore()

	_, cmd := h.handleHandoff()
	require.NotNil(t, cmd)
	h.Update(cmd())

	require.Nil(t, h.selectionOverlay,
		"a manual pin with no other registered account is a dead end the TUI must refuse")
	require.Equal(t, stateDefault, h.state)
	require.Contains(t, h.errBox.FullError(), "no registered target account")
}
