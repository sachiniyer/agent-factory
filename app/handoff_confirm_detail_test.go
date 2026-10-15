package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

const handoffCarryConversationID = "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"

// recordHandoffConversation seeds the agent tab with a recorded provider
// conversation so the confirm-time carry-intended check has its necessary
// precondition: an outgoing conversation id the daemon would attempt to carry.
func recordHandoffConversation(t *testing.T, inst *session.Instance, agent string) {
	t.Helper()
	inst.AddTabForTest("agent", session.TabKindAgent)
	require.True(t, inst.SetAgentConversation(session.AgentConversationData{
		Agent: agent, ID: handoffCarryConversationID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureInjected,
	}), "the carry-intended fixture needs a recorded conversation on the agent tab")
}

// TestHandoffConfirmDetail pins the consent elaboration's branch for every
// row the handoff picker offers. The fresh-start clause stays the truth for
// everything except a same-agent claude/codex account swap the daemon will
// attempt to carry (#4367/#4504); there the copy hedges toward "intended to
// continue" rather than asserting a fresh start that is wrong for the
// carry-succeeds case.
func TestHandoffConfirmDetail(t *testing.T) {
	claudeOutgoing := session.AgentConversationData{Agent: tmux.ProgramClaude, ID: handoffCarryConversationID}
	codexOutgoing := session.AgentConversationData{Agent: tmux.ProgramCodex, ID: handoffCarryConversationID}

	// A cross-agent handoff (no account) always starts fresh.
	require.Equal(t, handoffFreshStartDetail,
		handoffConfirmDetail("", tmux.ProgramCodex, tmux.ProgramClaude, tmux.ProgramClaude, nil, claudeOutgoing),
		"a cross-agent handoff with no account keeps the fresh-start copy")

	// A cross-agent account swap keeps the fresh-start copy: providers cannot
	// read each other's transcripts, so carry never applies.
	require.Equal(t, handoffFreshStartDetail,
		handoffConfirmDetail("spare", tmux.ProgramCodex, tmux.ProgramClaude, tmux.ProgramClaude, nil, claudeOutgoing),
		"a cross-agent account swap keeps the fresh-start copy")

	// A same-agent claude swap with a recorded conversation is the one branch
	// the fresh-start copy was wrong for: it hedges toward a carry.
	require.Equal(t, handoffCarryIntendedDetail,
		handoffConfirmDetail("personal", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, nil, claudeOutgoing),
		"a same-agent claude swap with a recorded conversation hedges toward the carry")

	// A nil resolvedAgents map falls back to the enum, so an older daemon that
	// did not classify resolved identities keeps the same branch it would have
	// taken under the picker's resolvedFor fallback.
	require.Equal(t, handoffCarryIntendedDetail,
		handoffConfirmDetail("spare", tmux.ProgramCodex, tmux.ProgramCodex, tmux.ProgramCodex, nil, codexOutgoing),
		"a same-agent codex swap stays carry-intended without a resolvedAgents map")

	// A program_overrides redirect makes the target enum differ from the
	// resolved identity. The daemon's resolved_agents map is what tells the
	// confirm dialog that "aider" resolves to the running "codex", so a
	// redirected same-resolved-identity swap is carry-intended despite the
	// enum compare saying otherwise (#4430 review).
	require.Equal(t, handoffCarryIntendedDetail,
		handoffConfirmDetail("spare", tmux.ProgramAider, tmux.ProgramCodex, tmux.ProgramCodex,
			map[string]string{tmux.ProgramAider: tmux.ProgramCodex}, codexOutgoing),
		"a redirected same-resolved-identity swap is carry-intended via the resolvedAgents map")

	// No recorded conversation -> nothing to carry, so the fresh-start copy
	// is the truth, same as a cross-agent handoff.
	require.Equal(t, handoffFreshStartDetail,
		handoffConfirmDetail("personal", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, nil, session.AgentConversationData{}),
		"a same-agent swap with no recorded conversation keeps the fresh-start copy")

	// Gemini is not a carry-supporting agent: even a same-agent swap with a
	// recorded-looking conversation keeps the fresh-start copy.
	require.Equal(t, handoffFreshStartDetail,
		handoffConfirmDetail("personal", tmux.ProgramGemini, tmux.ProgramGemini, tmux.ProgramGemini, nil,
			session.AgentConversationData{Agent: tmux.ProgramGemini, ID: handoffCarryConversationID}),
		"a gemini same-agent swap is never carry-intended")

	// An opaque target resolution ("") is never carry-intended: the daemon
	// cannot prove it launches a carry-supporting provider.
	require.Equal(t, handoffFreshStartDetail,
		handoffConfirmDetail("personal", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude,
			map[string]string{tmux.ProgramClaude: ""}, claudeOutgoing),
		"an opaque target resolution keeps the fresh-start copy")

	// A conversation recorded for a different provider than the target
	// resolves to is not carryable onto the replacement.
	require.Equal(t, handoffFreshStartDetail,
		handoffConfirmDetail("personal", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, nil, codexOutgoing),
		"a conversation recorded for another provider keeps the fresh-start copy")
}

// TestHandoffAccountSwapCarryIntendedShowsContinueCopy drives the real TUI into
// the confirm overlay for a same-agent claude account swap the daemon will
// attempt to carry, and asserts the consent elaboration hedges toward the
// carry instead of asserting the conversation starts fresh — the copy defect
// from #4504.
func TestHandoffAccountSwapCarryIntendedShowsContinueCopy(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	inst.Account = "work"
	recordHandoffConversation(t, inst, tmux.ProgramClaude)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(agent, _ string) (daemon.ListAccountsResponse, error) {
		require.Empty(t, agent, "pinned handoff needs the target agents' registry too")
		return daemon.ListAccountsResponse{
			Agents: []string{"claude", "codex", "gemini"},
			Entries: []daemon.AccountEntry{
				{Agent: "claude", Name: "work", LoggedIn: true},
				{Agent: "claude", Name: "personal", LoggedIn: true},
			},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"claude"}, h.handoffChoices[:1],
		"fixture assumption: the claude→personal account row is the first row")
	require.Equal(t, []string{"personal"}, h.handoffAccounts[:1])

	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	rendered := flatten(h.confirmationOverlay.Render())
	require.Contains(t, rendered, "intended to continue the previous conversation",
		"a carry-intended same-agent swap must describe the carry, not a fresh start")
	require.NotContains(t, rendered, "starts fresh with a summary",
		"the fresh-start clause is wrong for a carry-intended swap and must not appear")
	require.Contains(t, rendered, "Same worktree and branch — nothing is discarded",
		"the load-bearing reassurance is still true in the carry case")
}

// TestHandoffAccountSwapCarryIntendedKeepsTheWarningPrefix asserts the row's
// credential/override warning still prepends to the carry-intended copy, so the
// dialog never trades a real warning for the carry wording or vice versa.
func TestHandoffAccountSwapCarryIntendedKeepsTheWarningPrefix(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	inst.Account = "work"
	recordHandoffConversation(t, inst, tmux.ProgramClaude)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{
			Agents: []string{"claude", "codex", "gemini"},
			Entries: []daemon.AccountEntry{
				{Agent: "claude", Name: "work", LoggedIn: true},
				{Agent: "claude", Name: "personal", LoggedIn: false},
			},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"personal"}, h.handoffAccounts[:1])

	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	rendered := flatten(h.confirmationOverlay.Render())
	require.Contains(t, rendered, "no claude credential yet",
		"the credential warning still prepends to the carry-intended copy")
	require.Contains(t, rendered, "intended to continue the previous conversation",
		"the carry-intended copy follows the warning")
	require.NotContains(t, rendered, "starts fresh with a summary",
		"the fresh-start clause must not appear on a carry-intended row even with a warning")
}

// TestHandoffAccountSwapFreshWhenNoRecordedConversation is the regression guard
// against the carry-intended branch false-positiving: a same-agent claude swap
// with NO recorded conversation has nothing to carry, so the consent copy must
// stay the fresh-start clause it always was.
func TestHandoffAccountSwapFreshWhenNoRecordedConversation(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	inst.Account = "work"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{
			Agents: []string{"claude", "codex", "gemini"},
			Entries: []daemon.AccountEntry{
				{Agent: "claude", Name: "work", LoggedIn: true},
				{Agent: "claude", Name: "personal", LoggedIn: true},
			},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"personal"}, h.handoffAccounts[:1])

	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	rendered := flatten(h.confirmationOverlay.Render())
	require.Contains(t, rendered, "starts fresh with a summary of the work so far",
		"a same-agent swap with no recorded conversation keeps the fresh-start copy")
	require.NotContains(t, rendered, "intended to continue",
		"the carry-intended copy must not appear when there is nothing to carry")
}

// TestHandoffAccountSwapFreshForCrossAgentAccountTarget asserts a cross-agent
// account swap keeps the fresh-start copy: carry never applies across agents,
// so the new branch must not bleed the carry wording onto it.
func TestHandoffAccountSwapFreshForCrossAgentAccountTarget(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	inst.Account = "work"
	recordHandoffConversation(t, inst, tmux.ProgramClaude)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	restore := SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{
			Agents: []string{"claude", "codex", "gemini"},
			Entries: []daemon.AccountEntry{
				{Agent: "claude", Name: "work", LoggedIn: true},
				{Agent: "codex", Name: "spare", LoggedIn: true},
			},
		}, nil
	})
	defer restore()
	_, cmd := h.handleHandoff()
	h.Update(cmd())
	require.Equal(t, []string{"spare"}, h.handoffAccounts[:1],
		"fixture assumption: the codex→spare account row is the first row")
	require.Equal(t, tmux.ProgramCodex, h.handoffChoices[0],
		"fixture assumption: the first row targets codex, a cross-agent swap from claude")

	h.selectionOverlay.SetSelectedIndex(0)
	h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	rendered := flatten(h.confirmationOverlay.Render())
	require.Contains(t, rendered, "starts fresh with a summary of the work so far",
		"a cross-agent account swap keeps the fresh-start copy even with a recorded conversation")
	require.NotContains(t, rendered, "intended to continue",
		"the carry-intended copy must not appear on a cross-agent swap")
}

// TestHandoffConfirmDetailAgreesWithDaemonMissionNotice is the bug's core
// consistency check (#4504): the consent `detail` the confirming user reads and
// the mission notice the daemon delivers to the replacement must never assert
// opposite fates for the conversation. The confirm-time copy cannot know the
// carry outcome, so a carry-intended row HEDGES; this test asserts the hedge
// agrees with BOTH daemon outcomes (carry succeeds -> continue; carry fails ->
// fresh), and that a fresh-start row agrees with the cross-agent fresh notice
// and the same-agent no-carry fresh notice. The notice strings come straight
// from session.MissionBrief.Render — the same Render the daemon delivers as the
// replacement's first prompt (daemon/handoff_account_carry_test.go pins that
// delivery), so this is a non-interactive proof of the surfaces the bug left
// un-checked for consistency.
func TestHandoffConfirmDetailAgreesWithDaemonMissionNotice(t *testing.T) {
	claudeOutgoing := session.AgentConversationData{Agent: tmux.ProgramClaude, ID: handoffCarryConversationID}

	// A carry-intended same-agent claude swap: the TUI hedges.
	confirm := handoffConfirmDetail("personal", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, nil, claudeOutgoing)
	require.Contains(t, confirm, "intended to continue the previous conversation")
	require.Contains(t, confirm, "otherwise it starts fresh")

	// Daemon outcome 1 — carry SUCCEEDS: the replacement is told the
	// conversation continues. Agrees with the hedge's "intended to continue".
	carried := session.MissionBrief{From: tmux.ProgramClaude, To: tmux.ProgramClaude,
		Conversation: session.HandoffConversation{Carried: true}, Goal: "finish"}
	require.Contains(t, carried.Render(), "This conversation continues after an account handoff")
	require.NotContains(t, carried.Render(), "not available to you",
		"a carried conversation must not be described as lost — the consent copy promised a continue")

	// Daemon outcome 2 — carry FAILS: the replacement is told it is a fresh
	// conversation. Agrees with the hedge's "otherwise it starts fresh".
	failed := session.MissionBrief{From: tmux.ProgramClaude, To: tmux.ProgramClaude,
		Conversation: session.HandoffConversation{CarryFailure: "its transcript is missing from the previous account's home"}}
	require.Contains(t, failed.Render(), "This is a fresh conversation after an account handoff")
	require.Contains(t, failed.Render(), "af tried to carry the previous conversation over to the new account, but")
	require.Contains(t, failed.Render(), "so it is not available to you")

	// A cross-agent handoff: the TUI asserts fresh start. The daemon's
	// cross-agent notice agrees — the conversation is not available.
	crossConfirm := handoffConfirmDetail("", tmux.ProgramCodex, tmux.ProgramClaude, tmux.ProgramClaude, nil, claudeOutgoing)
	require.Contains(t, crossConfirm, "starts fresh with a summary of the work so far")
	cross := session.MissionBrief{From: tmux.ProgramClaude, To: tmux.ProgramCodex, CrossAgent: true, Goal: "finish"}
	require.Contains(t, cross.Render(), "Its conversation is not available to you")
}
