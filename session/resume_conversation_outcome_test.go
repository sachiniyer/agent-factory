package session

import (
	"net/http/httptest"
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

	// A recorded conversation id the launch command can name carries — when
	// the store still holds its rollout, the evidence exact resume checks.
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	inst := exactResumeSession(t, tmux.ProgramCodex)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramCodex, ID: "019f386f-7206-7fc2-803b-f7045e07a242", CapturedAt: time.Now()}}}
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", inst.GetWorktreePath())
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

// A sandbox session whose respawn re-provisions (an older sandbox build, or a
// sandbox with no live agent-server) starts a fresh conversation: the
// replacement sandbox's store does not hold the old transcript (the same
// reason ExactResume gives). CanRelaunchInPlace is false here — no remoteClient
// is bound, so AgentServer() is the deadRemoteAgentServer sentinel.
func TestResumeConversationOutcomeSandboxRefusesWhenReprovisioning(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	inst := exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	require.Equal(t, HandoffConversation{Carried: true}, inst.ResumeConversationOutcome(),
		"the control: a local session resumes its recorded conversation")

	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	require.False(t, inst.CanRelaunchInPlace(),
		"precondition: a sandbox with no live agent-server cannot relaunch in place")
	require.Equal(t,
		HandoffConversation{CarryFailure: "this session runs in a sandbox, and the sandbox af restores it into does not keep its agent's conversation"},
		inst.ResumeConversationOutcome())
}

// A sandbox session whose sandbox can relaunch its agent in place (W7.1,
// #5571) carries: the respawn dispatches to the in-place relaunch, which runs
// the same resume-capable local respawn inside the existing sandbox — whose
// store still holds the agent's transcript — rather than re-provisioning. The
// verdict must answer carried, not the re-provision refusal the blanket
// sandbox branch previously asserted (#6768).
func TestResumeConversationOutcomeSandboxCarriesWhenRelaunchCapable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": []string{"tab-api", RelaunchFeature}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	rc := srv.client(t, httpSrv.URL)

	inst := exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	inst.mu.Lock()
	inst.remoteClient = rc
	inst.agentSrv = nil
	inst.mu.Unlock()

	require.True(t, inst.CanRelaunchInPlace(),
		"precondition: the sandbox advertises the relaunch feature")
	require.Equal(t, HandoffConversation{Carried: true}, inst.ResumeConversationOutcome(),
		"a relaunch-capable sandbox resumes the agent in place, keeping its conversation")
}

// Capability is not proof of carry: a relaunch-capable sandbox running an agent
// whose respawn uses exact-resume-only mode (Codex, Amp) with no recorded
// conversation id still starts fresh — the in-place relaunch runs the same
// local respawn, which refuses an account-wide latest — so the verdict must
// name the fresh start, not answer carried. CanRelaunchInPlace proves only that
// the relaunch endpoint exists, not that the configured program can resume this
// session's conversation.
func TestResumeConversationOutcomeSandboxNamesFreshStartWhenRelaunchCannotResume(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": []string{"tab-api", RelaunchFeature}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	rc := srv.client(t, httpSrv.URL)

	for _, program := range []string{tmux.ProgramCodex, tmux.ProgramAmp} {
		inst := exactResumeSession(t, program)
		// No recorded conversation id: the respawn's exact-resume-only mode
		// cannot name this session's own conversation, so it starts fresh.
		inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: program, CapturedAt: time.Now()}}}
		inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
		inst.mu.Lock()
		inst.remoteClient = rc
		inst.agentSrv = nil
		inst.mu.Unlock()

		require.True(t, inst.CanRelaunchInPlace(),
			"precondition: the sandbox advertises the relaunch feature")
		require.Equal(t,
			HandoffConversation{CarryFailure: "af recorded no conversation id for this " + program + " session"},
			inst.ResumeConversationOutcome(), program)
	}
}

// --force-reap asks for the sandbox to be replaced even when it could relaunch
// in place: the replacement sandbox starts a fresh conversation, so the
// verdict must answer the re-provision refusal regardless of CanRelaunchInPlace
// (the same gate decideRecovery applies).
func TestResumeConversationOutcomeSandboxRefusesWhenForceReapOverridesRelaunch(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": []string{"tab-api", RelaunchFeature}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	rc := srv.client(t, httpSrv.URL)

	inst := exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	inst.mu.Lock()
	inst.remoteClient = rc
	inst.agentSrv = nil
	inst.mu.Unlock()
	inst.SetForceSandboxReplacement(true)

	require.True(t, inst.CanRelaunchInPlace(),
		"precondition: the sandbox advertises the relaunch feature, but --force-reap overrides it")
	require.Equal(t,
		HandoffConversation{CarryFailure: "this session runs in a sandbox, and the sandbox af restores it into does not keep its agent's conversation"},
		inst.ResumeConversationOutcome())
}
