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

// A sandbox (remote-workspace) session answers not-carried until a respawn
// has reported back: the rollout or transcript that proves a resume lives in
// the sandbox's own store, which the daemon cannot read off-box, and which
// branch the respawn takes (in-place relaunch or re-provision) is decided
// inside RespawnWithLiveBoundary, not by a capability probe taken here.
// Under-reporting is the honest direction, so asked BEFORE a respawn — or
// after one that left no report — the CarryFailure text is the same
// whichever branch the respawn will take or took, and does not depend on
// CanRelaunchInPlace or --force-reap (the user-visible part of #6863). The
// carried answer a sandbox relaunch reports back is #6868, covered in
// sandbox_respawn_conversation_test.go.
func TestResumeConversationOutcomeSandboxAnswersUnprovenBeforeARespawn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const sandboxCarryFailure = "this session runs in a sandbox, and af cannot prove from the driving daemon that its respawn carried the conversation"

	// Control: a local session resumes its recorded conversation. The sandbox
	// refusal below is what changes the answer, not the recorded id.
	localInst := exactResumeSession(t, tmux.ProgramClaude)
	localInst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	require.Equal(t, HandoffConversation{Carried: true}, localInst.ResumeConversationOutcome(),
		"the control: a local session resumes its recorded conversation")

	// A relaunch-capable sandbox: the sandbox advertises the relaunch feature,
	// so an in-place relaunch is available. The verdict still does not answer
	// Carried — the daemon cannot prove the in-place relaunch resumes the
	// conversation off-box — and the text is the same regardless of the
	// recorded id, the program, or --force-reap.
	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": []string{"tab-api", RelaunchFeature}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	rc := srv.client(t, httpSrv.URL)

	relaunchCapable := func(t *testing.T, program, id string) *Instance {
		t.Helper()
		inst := exactResumeSession(t, program)
		inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: program, ID: id, CapturedAt: time.Now()}}}
		inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
		inst.mu.Lock()
		inst.remoteClient = rc
		inst.agentSrv = nil
		inst.mu.Unlock()
		return inst
	}

	// Relaunch-capable with a recorded id (Claude): would carry locally, but the
	// daemon cannot prove the sandbox's in-place relaunch resumes it.
	inst := relaunchCapable(t, tmux.ProgramClaude, "conv-1")
	require.True(t, inst.CanRelaunchInPlace(),
		"precondition: the sandbox advertises the relaunch feature")
	require.Equal(t, HandoffConversation{CarryFailure: sandboxCarryFailure}, inst.ResumeConversationOutcome(),
		"a relaunch-capable sandbox must not be reported Carried from the driving daemon")

	// Relaunch-capable with no recorded id (Codex, Amp): the in-place relaunch
	// starts fresh, but the verdict reports the same honest non-carry, not the
	// per-program fresh-start reason a local respawn would give.
	for _, program := range []string{tmux.ProgramCodex, tmux.ProgramAmp} {
		inst := relaunchCapable(t, program, "")
		require.True(t, inst.CanRelaunchInPlace(),
			"precondition: the sandbox advertises the relaunch feature")
		require.Equal(t, HandoffConversation{CarryFailure: sandboxCarryFailure},
			inst.ResumeConversationOutcome(), program)
	}

	// --force-reap re-provisions even a relaunch-capable sandbox: the
	// replacement sandbox starts fresh, and the verdict is the same honest
	// non-carry — it must not depend on the capability probe or the override.
	inst = relaunchCapable(t, tmux.ProgramClaude, "conv-1")
	inst.SetForceSandboxReplacement(true)
	require.True(t, inst.CanRelaunchInPlace(),
		"precondition: --force-reap overrides a relaunch-capable sandbox")
	require.Equal(t, HandoffConversation{CarryFailure: sandboxCarryFailure}, inst.ResumeConversationOutcome())

	// A sandbox with no live agent-server cannot relaunch in place, so the
	// respawn re-provisions: the replacement sandbox starts fresh, and the
	// verdict is the same honest non-carry.
	inst = exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	require.False(t, inst.CanRelaunchInPlace(),
		"precondition: a sandbox with no live agent-server cannot relaunch in place")
	require.Equal(t, HandoffConversation{CarryFailure: sandboxCarryFailure}, inst.ResumeConversationOutcome())
}
