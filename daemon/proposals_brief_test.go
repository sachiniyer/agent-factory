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

// briefedManager seeds one instance whose prompt delivery observes status.
func briefedManager(t *testing.T, title string, status session.Status, delivery session.PromptDeliveryStatus) (*Manager, string, *session.Instance) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := observedPromptBackend{
		readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()},
		status:           delivery,
	}
	inst := registerStarted(t, manager, repoID, repoPath, title, backend, true, status)
	return manager, repoID, inst
}

func lastPromptAttempt(t *testing.T, m *Manager, id string) time.Time {
	t.Helper()
	inst, _, _, _, _, err := m.resolveActionSession(id, "", "")
	require.NoError(t, err)
	require.NotNil(t, inst)
	return inst.ToInstanceData().LastPromptAttemptAt
}

func TestProposalRoutes_DeliverRulesBriefsTheAgentOnce(t *testing.T) {
	manager, _, inst := briefedManager(t, "briefed", session.Ready, session.PromptDelivered)
	cs := &controlServer{manager: manager}

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: inst.ID, Slug: "auth", Docs: []string{"goals"}, DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	started := env["data"].(map[string]any)
	require.Equal(t, "delivered", started["rules_delivery"],
		"a confirmed send must report what it did, %v", started)
	attemptAt := lastPromptAttempt(t, manager, inst.ID)
	require.False(t, attemptAt.IsZero(), "deliver_rules must send the contract as a prompt")

	code, env = postProposalRoute(t, cs, "AttachProposal", AttachProposalRequest{SessionID: inst.ID, Slug: "auth", DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	require.False(t, lastPromptAttempt(t, manager, inst.ID).After(attemptAt),
		"a repeated attach must not paste the contract a second time")
}

func TestProposalRoutes_NoDeliverRulesSendsNothing(t *testing.T) {
	manager, _, inst := briefedManager(t, "cli-parity", session.Ready, session.PromptDelivered)
	cs := &controlServer{manager: manager}

	// The request shape every older client sends: no flag, no paste.
	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: inst.ID, Slug: "auth", Docs: []string{"goals"}})
	require.Equal(t, http.StatusOK, code, env)
	require.Empty(t, env["data"].(map[string]any)["rules_delivery"])
	require.True(t, lastPromptAttempt(t, manager, inst.ID).IsZero(),
		"an older client must not get a second copy of the rules")

	code, _ = postProposalRoute(t, cs, "AttachProposal", AttachProposalRequest{SessionID: inst.ID, Slug: "auth"})
	require.Equal(t, http.StatusOK, code)
	require.True(t, lastPromptAttempt(t, manager, inst.ID).IsZero())
}

func TestProposalRoutes_DeliverRulesHoldsForAPaneInUse(t *testing.T) {
	manager, repoID, inst := briefedManager(t, "held-pane", session.Ready, session.PromptDelivered)
	cs := &controlServer{manager: manager}
	manager.PauseStatusPollFor(repoID, inst.Title, inst.ID, "tui")

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: inst.ID, Slug: "auth", Docs: []string{"goals"}, DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, "held", env["data"].(map[string]any)["rules_delivery"],
		"the web must be told the contract is owed, not that the agent was briefed")
	require.True(t, lastPromptAttempt(t, manager, inst.ID).IsZero(),
		"the contract must not paste over the user's in-progress input")

	manager.ResumeStatusPollFor(repoID, inst.Title, inst.ID, "tui")
	manager.pushProposals()
	require.Eventually(t, func() bool {
		return !lastPromptAttempt(t, manager, inst.ID).IsZero()
	}, 5*time.Second, 10*time.Millisecond,
		"the owed contract must land on the first clear tick")
}

func TestProposalRoutes_DeliverRulesDeadSessionSaysHeld(t *testing.T) {
	manager, _, inst := briefedManager(t, "dead-pane", session.Dead, session.PromptDelivered)
	cs := &controlServer{manager: manager}

	code, env := postProposalRoute(t, cs, "StartProposal", StartProposalRequest{SessionID: inst.ID, Slug: "auth", Docs: []string{"goals"}, DeliverRules: true})
	require.Equal(t, http.StatusOK, code, env)
	started := env["data"].(map[string]any)
	require.Equal(t, "held", started["rules_delivery"],
		"a session with no live agent must not claim a briefing")
	require.NotEmpty(t, started["rules_error"])
	require.True(t, lastPromptAttempt(t, manager, inst.ID).IsZero())
}
