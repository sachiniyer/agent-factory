package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #6768: whoever asks for a handoff must be told whether the conversation
// carried — in the response, and afterwards on the ledger entry that outlives
// the pending swap record. These pin both halves against the same fixture the
// carry behavior itself is tested on.

func TestHandoffSessionReportsTheCarriedConversation(t *testing.T) {
	m, repo, inst, _, source, rel := carryHandoffFixture(t)
	const transcript = "{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(source, rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, rel), []byte(transcript), 0o600))

	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, session.HandoffConversation{Carried: true}, resp.Conversation,
		"the response must say the transcript carried — ok:true alone used to hide the answer")

	require.Len(t, inst.Tabs[0].Handoffs, 1)
	require.Equal(t, session.HandoffConversation{Carried: true}, inst.Tabs[0].Handoffs[0].Conversation,
		"the ledger entry keeps the answer after the pending swap record is gone")
}

func TestHandoffSessionReportsWhyItCouldNotCarry(t *testing.T) {
	m, repo, inst, _, _, _ := carryHandoffFixture(t) // no transcript in the old home

	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.NoError(t, err)
	require.True(t, resp.OK, "the handoff still succeeds — only its conversation answer changes")
	require.False(t, resp.Conversation.Carried)
	require.Contains(t, resp.Conversation.CarryFailure, "transcript",
		"a fresh start must name its reason, not just omit carried")

	require.Len(t, inst.Tabs[0].Handoffs, 1)
	require.Equal(t, resp.Conversation, inst.Tabs[0].Handoffs[0].Conversation,
		"the afterwards answer is the same one the response gave")
}

// A retry whose re-spawn resumes the session's conversation says carried; one
// that cannot prove a resume names the fresh start — the fact the respawn
// could previously only write to the daemon log.
func TestResumeFromLimitReportsTheRespawnConversation(t *testing.T) {
	t.Run("worktree-scoped resume carries", func(t *testing.T) {
		m, repo, inst, _ := newAutoResumeManager(t, "", false, "continue", time.Now().Add(time.Hour))

		outcome, conversation, err := m.resumeFromLimitOutcomeConversation(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
		require.NoError(t, err)
		require.Equal(t, resumePerformed, outcome)
		require.Equal(t, session.HandoffConversation{Carried: true}, conversation)
	})

	t.Run("no recorded id says the fresh start", func(t *testing.T) {
		m, repo, inst, _ := newAutoResumeManager(t, "", false, "continue", time.Now().Add(time.Hour))
		inst.Program = tmux.ProgramCodex
		inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))

		outcome, conversation, err := m.resumeFromLimitOutcomeConversation(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
		require.NoError(t, err)
		require.Equal(t, resumePerformed, outcome)
		require.Equal(t,
			session.HandoffConversation{CarryFailure: "af recorded no conversation id for this codex session"},
			conversation)
	})
}

// A committed carry whose replacement died before proving itself is retried
// as a fresh conversation; the response must name that demotion the notice
// already states to the agent.
func TestResumeFromLimitReportsTheAbandonedCarry(t *testing.T) {
	m, repo, inst, _ := newAutoResumeManager(t, "", false, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "work")
	inst.ReconcileAccountHandoffSnapshot("work", "claude", true, &session.AccountSwapData{
		To:                    "work",
		CarriedConversationID: carryHandoffConversationID,
		CarriedLaunchStarted:  true,
	})

	outcome, conversation, err := m.resumeFromLimitOutcomeConversation(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err)
	require.Equal(t, resumePerformed, outcome)
	require.False(t, conversation.Carried)
	require.Contains(t, conversation.CarryFailure,
		"the replacement stopped before it was confirmed working on the carried conversation")
}

// The operator attestation retires the pending record that held the verdict,
// so its response answers the same question the retry's does.
func TestConfirmHandoffDeliveryReportsTheSwapConversation(t *testing.T) {
	t.Run("manual swap reports its carry", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, manager, repoID, repoPath, "swapped", backend)
		inst.ReconcileAccountHandoffSnapshot("work", "claude", false, &session.AccountSwapData{
			Manual: true, From: "personal", To: "work",
			ReplacementPanesStarted: true,
			MissionDeliveryStatus:   session.PromptDelivered,
			CarriedConversationID:   carryHandoffConversationID,
		})

		performed, conversation, err := manager.confirmHandoffDeliveryConversation(ConfirmHandoffDeliveryRequest{
			ID: inst.ID, Title: inst.Title, RepoID: repoID,
		})
		require.NoError(t, err)
		require.True(t, performed)
		require.Equal(t, session.HandoffConversation{Carried: true}, conversation)
	})

	t.Run("manual swap reports its fallback", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, manager, repoID, repoPath, "swapped", backend)
		inst.ReconcileAccountHandoffSnapshot("work", "claude", false, &session.AccountSwapData{
			Manual: true, From: "personal", To: "work",
			ReplacementPanesStarted: true,
			MissionDeliveryStatus:   session.PromptDelivered,
			CarryFallback:           "the outgoing account's transcript could not be copied",
		})

		performed, conversation, err := manager.confirmHandoffDeliveryConversation(ConfirmHandoffDeliveryRequest{
			ID: inst.ID, Title: inst.Title, RepoID: repoID,
		})
		require.NoError(t, err)
		require.True(t, performed)
		require.Equal(t,
			session.HandoffConversation{CarryFailure: "the outgoing account's transcript could not be copied"},
			conversation)
	})

	t.Run("agent handoff reports the fresh start", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, manager, repoID, repoPath, "wedged-handoff", backend)
		stageHandoffWedge(t, inst, "continue the inherited work", session.PromptSentUnverified)

		performed, conversation, err := manager.confirmHandoffDeliveryConversation(ConfirmHandoffDeliveryRequest{
			ID: inst.ID, Title: inst.Title, RepoID: repoID,
		})
		require.NoError(t, err)
		require.True(t, performed)
		require.Equal(t,
			session.HandoffConversation{CarryFailure: crossAgentConversationReason},
			conversation)
	})
}
