package daemon

import (
	"net/http"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// deliver_rules is the web's opt-in: the daemon owes the agent the proposal
// contract as a prompt, once per attach, delivered through the same guarded
// send path a proposal push uses (#6755). A request without it — every older
// client, the CLI included — must see no paste at all.

func lastPromptAttempt(t *testing.T, m *Manager, id string) time.Time {
	t.Helper()
	inst, _, _, _, _, err := m.resolveActionSession(id, "", "")
	require.NoError(t, err)
	require.NotNil(t, inst)
	return inst.ToInstanceData().LastPromptAttemptAt
}

func TestProposalRoutes_DeliverRulesBriefsTheAgentOnce(t *testing.T) {
	manager, _, data := createRealKillSession(t, "briefed")
	cs := &controlServer{manager: manager}
	inst, _, _, _, _, err := manager.resolveActionSession(data.ID, "", "")
	require.NoError(t, err)
	inst.SetStatusForTest(session.Ready)

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "auth", Docs: []string{"goals"}, DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	started := env["data"].(map[string]any)
	require.Contains(t, []string{"delivered", "unconfirmed"}, started["rules_delivery"],
		"deliver_rules must report what the send did, %v", started)
	attemptAt := lastPromptAttempt(t, manager, data.ID)
	require.False(t, attemptAt.IsZero(), "deliver_rules must send the contract as a prompt")

	code, env = postProposalRoute(t, cs, "AttachProposal", AttachProposalRequest{SessionID: data.ID, Slug: "auth", DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	require.False(t, lastPromptAttempt(t, manager, data.ID).After(attemptAt),
		"a repeated attach must not paste the contract a second time")
}

func TestProposalRoutes_NoDeliverRulesSendsNothing(t *testing.T) {
	manager, _, data := createRealKillSession(t, "cli-parity")
	cs := &controlServer{manager: manager}
	inst, _, _, _, _, err := manager.resolveActionSession(data.ID, "", "")
	require.NoError(t, err)
	inst.SetStatusForTest(session.Ready)

	// The request shape every older client sends: no flag, no paste.
	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "auth", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	require.Empty(t, env["data"].(map[string]any)["rules_delivery"])
	require.True(t, lastPromptAttempt(t, manager, data.ID).IsZero(),
		"an older client must not get a second copy of the rules")

	code, _ = postProposalRoute(t, cs, "AttachProposal", AttachProposalRequest{SessionID: data.ID, Slug: "auth"})
	require.Equal(t, http.StatusOK, code)
	require.True(t, lastPromptAttempt(t, manager, data.ID).IsZero())
}

func TestProposalRoutes_DeliverRulesHoldsForAPaneInUse(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "held-pane")
	cs := &controlServer{manager: manager}
	inst, _, _, _, _, err := manager.resolveActionSession(data.ID, "", "")
	require.NoError(t, err)
	inst.SetStatusForTest(session.Ready)
	manager.PauseStatusPollFor(repo.ID, data.Title, data.ID, "tui")

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "auth", Docs: []string{"goals"}, DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, "held", env["data"].(map[string]any)["rules_delivery"],
		"the web must be told the contract is owed, not that the agent was briefed")
	require.True(t, lastPromptAttempt(t, manager, data.ID).IsZero(),
		"the contract must not paste over the user's in-progress input")

	manager.ResumeStatusPollFor(repo.ID, data.Title, data.ID, "tui")
	manager.pushProposals()
	require.False(t, lastPromptAttempt(t, manager, data.ID).IsZero(),
		"the owed contract must land on the first clear tick")
}

func TestProposalRoutes_DeliverRulesDeadSessionSaysHeld(t *testing.T) {
	manager, _, data := createRealKillSession(t, "dead-pane")
	cs := &controlServer{manager: manager}
	inst, _, _, _, _, err := manager.resolveActionSession(data.ID, "", "")
	require.NoError(t, err)
	inst.SetStatusForTest(session.Dead)

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: data.ID, Slug: "auth", Docs: []string{"goals"}, DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	started := env["data"].(map[string]any)
	require.Equal(t, "held", started["rules_delivery"],
		"a session with no live agent must not claim a briefing")
	require.NotEmpty(t, started["rules_error"])
	require.True(t, lastPromptAttempt(t, manager, data.ID).IsZero())
}
