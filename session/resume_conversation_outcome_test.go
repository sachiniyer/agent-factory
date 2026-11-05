package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #6768: a retry-limit response owes the caller the answer a re-spawn produces
// — the conversation carried, or it starts fresh and why. The predicate is the
// same one the respawn applies: a recorded id the command can name, or the
// worktree-scoped resume form the exact-only rewrite keeps.
func TestResumeConversationOutcomeCarriesAProvableResume(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// A recorded conversation id the launch command can name carries.
	inst := exactResumeSession(t, tmux.ProgramCodex)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramCodex, ID: "019f386f-7206-7fc2-803b-f7045e07a242", CapturedAt: time.Now()}}}
	require.Equal(t, HandoffConversation{Carried: true}, inst.ResumeConversationOutcome())

	// So do the worktree-scoped resume forms that need no recorded id: the
	// rewrite restores the newest conversation in this session's own worktree.
	for _, program := range []string{tmux.ProgramClaude, tmux.ProgramAider, tmux.ProgramGemini, tmux.ProgramOpencode, tmux.ProgramDevin} {
		inst := exactResumeSession(t, program)
		require.Equal(t, HandoffConversation{Carried: true}, inst.ResumeConversationOutcome(), program)
	}
}

// The shared-home "latest" a respawn could previously only log is stated, not
// claimed: codex and amp with no recorded id cannot be proven to resume the
// session's own conversation, so the outcome names the fresh start.
func TestResumeConversationOutcomeNamesTheFreshStart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	for _, program := range []string{tmux.ProgramCodex, tmux.ProgramAmp} {
		inst := exactResumeSession(t, program)
		require.Equal(t,
			HandoffConversation{CarryFailure: "af recorded no conversation id for this " + program + " session"},
			inst.ResumeConversationOutcome(), program)
	}

	// A codex selector af cannot scope to the worktree is not a provable
	// resume either; the stated reason is the same no-recorded-id one.
	inst := exactResumeSession(t, "codex resume --all")
	require.Equal(t,
		HandoffConversation{CarryFailure: "af recorded no conversation id for this codex session"},
		inst.ResumeConversationOutcome())

	// An unrecognized command cannot be proven to resume anything.
	inst = exactResumeSession(t, "bash")
	require.Equal(t,
		HandoffConversation{CarryFailure: "af does not know which agent this session runs"},
		inst.ResumeConversationOutcome())
}

// A sandbox session's conversation lives in the previous host's store; the
// re-provisioned sandbox never holds it (the same reason ExactResume gives).
func TestResumeConversationOutcomeSandboxNeverCarries(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	inst := exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	require.Equal(t, HandoffConversation{Carried: true}, inst.ResumeConversationOutcome(),
		"the control: a local session resumes its recorded conversation")

	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	require.Equal(t,
		HandoffConversation{CarryFailure: "this session runs in a sandbox, and the sandbox af restores it into does not keep its agent's conversation"},
		inst.ResumeConversationOutcome())
}
