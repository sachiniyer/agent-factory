package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #4400 review-round-5 carry helpers: ambientSafeCarriedTabs filters the
// roster a rejected pin must not restore, and reconcilePendingSwapConversation
// keeps the committed swap's recorded conversation equal to the one the create
// actually launches.

// TestAmbientSafeCarriedTabsKeepsAgentPlaceholder pins the two defects Codex
// found in the filter: it must not compact the parked roster's shared backing
// array in place (the next retry reads it), and it must keep index 0 as the
// placeholder restoreCarriedTabs and countNonAgentTabs skip unconditionally —
// dropping it silently consumes the first surviving web/editor tab.
func TestAmbientSafeCarriedTabsKeepsAgentPlaceholder(t *testing.T) {
	parked := []session.TabData{
		{ID: "tab-agent", Kind: session.TabKindAgent, TmuxName: "af_x"},
		{ID: "tab-shell", Kind: session.TabKindShell, TmuxName: "af_x_shell"},
		{ID: "tab-web", Kind: session.TabKindWeb, URL: "http://localhost:9/"},
		{ID: "tab-logs", Kind: session.TabKindProcess, Command: "tail -f x", TmuxName: "af_x_logs"},
		{ID: "tab-vs", Kind: session.TabKindVSCode},
	}
	snapshot := append([]session.TabData(nil), parked...)

	kept := ambientSafeCarriedTabs(parked)

	var ids []string
	for _, td := range kept {
		ids = append(ids, td.ID)
	}
	require.Equal(t, []string{"tab-agent", "tab-web", "tab-vs"}, ids,
		"index 0 stays the placeholder; every tmux-backed row after it is dropped")
	require.Equal(t, snapshot, parked,
		"filtering must not mutate the roster the parked carry still owns")
	require.Empty(t, ambientSafeCarriedTabs(nil))
}

// TestReconcilePendingSwapConversationFollowsLaunchedConversation pins the
// create-side half of the finding: the attached swap's ConversationID tracks
// the conversation this attempt launches — substituted, fresh, or
// non-resumable — and the parked carry's shared pointer is never written
// through.
func TestReconcilePendingSwapConversationFollowsLaunchedConversation(t *testing.T) {
	parked := &session.AccountSwapData{To: "work", ConversationID: "old-conv"}
	req := CreateSessionRequest{pendingAccountSwap: parked}

	// A substituted conversation becomes the swap's recorded id.
	req.resumeConversation = session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "new-conv"}
	reconcilePendingSwapConversation(&req)
	require.Equal(t, "new-conv", req.pendingAccountSwap.ConversationID)
	require.Equal(t, "old-conv", parked.ConversationID,
		"the parked carry's pointer is shared — reconcile must clone, not mutate")

	// A fresh start clears it: the pane has no resumable id to stamp.
	req.resumeConversation = session.AgentConversationData{}
	reconcilePendingSwapConversation(&req)
	require.Empty(t, req.pendingAccountSwap.ConversationID)

	// A swap that never recorded a conversation stays untouched.
	plain := &session.AccountSwapData{To: "work"}
	req.pendingAccountSwap = plain
	req.resumeConversation = session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "x"}
	reconcilePendingSwapConversation(&req)
	require.Same(t, plain, req.pendingAccountSwap)
	require.Empty(t, plain.ConversationID)

	// No swap at all is a no-op.
	req.pendingAccountSwap = nil
	reconcilePendingSwapConversation(&req)
	require.Nil(t, req.pendingAccountSwap)
}
