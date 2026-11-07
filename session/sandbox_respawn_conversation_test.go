package session

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #6868: a sandbox session's carried/not-carried answer is what the sandbox's
// relaunch reported about the respawn it ACTUALLY performed — the branch
// picked inside the respawn, never a check made before it. The relaunch route
// reports that verdict back over the wire; every other shape — a re-provision,
// a fresh start, an older build that sends no report, a report for another
// session or conversation — answers not-carried with the reason that is true
// for what ran.

// sandboxRelaunchSession binds a sandbox-shaped instance to a wire server and
// returns the instance with the remote client the daemon would have.
func sandboxRelaunchSession(t *testing.T, srv *tabWireServer, url, program, convID string) *Instance {
	t.Helper()
	inst := exactResumeSession(t, program)
	if convID != "" {
		inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: program, ID: convID, CapturedAt: time.Now()}}}
	}
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	rc := srv.client(t, url)
	inst.mu.Lock()
	inst.remoteClient = rc
	inst.agentSrv = nil
	inst.mu.Unlock()
	return inst
}

// relaunchWireServer answers /v1/health with the relaunch feature and
// /v1/agent/relaunch with the supplied response payload.
func relaunchWireServer(t *testing.T, relaunchResp map[string]any) (*tabWireServer, *httptest.Server) {
	t.Helper()
	srv := newTabWireServer(t).
		route("/v1/agent/relaunch", relaunchResp).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": []string{"tab-api", RelaunchFeature}})
	httpSrv := httptest.NewServer(srv.mux)
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv
}

// carriedReport is the wire payload a sandbox's relaunch returns when its
// respawn resumed this session's own conversation by its recorded id.
func carriedReport(session, agent, convID string) map[string]any {
	return map[string]any{
		"ok": true,
		"conversation": map[string]any{
			"session":      session,
			"carried":      true,
			"conversation": map[string]any{"agent": agent, "id": convID},
		},
	}
}

const sandboxReprovisionedReason = "the respawn re-provisioned this session's sandbox — the in-place relaunch that could resume its conversation never ran"

// An in-place relaunch that reports it resumed this session's own recorded
// conversation is the one case a sandbox session answers carried:true — the
// report comes from the sandbox that ran the relaunch, not from anything the
// driving daemon could see itself.
func TestSandboxRelaunchReportCarriesAProvableResume(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	srv, httpSrv := relaunchWireServer(t, carriedReport("B-7 item", tmux.ProgramClaude, "conv-1"))
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "conv-1")

	require.NoError(t, inst.RelaunchInPlace())
	assert.Equal(t, "POST", srv.methods["/v1/agent/relaunch"])
	require.Equal(t, HandoffConversation{Carried: true}, inst.ResumeConversationOutcome())
}

// A worktree-scoped resume names no conversation id before the launch picks
// one, but it is still the sandbox's own report that this session's
// conversation resumed.
func TestSandboxRelaunchReportCarriesAWorktreeScopedResume(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	srv, httpSrv := relaunchWireServer(t, map[string]any{
		"ok":           true,
		"conversation": map[string]any{"session": "B-7 item", "carried": true},
	})
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "")

	require.NoError(t, inst.RelaunchInPlace())
	require.Equal(t, HandoffConversation{Carried: true}, inst.ResumeConversationOutcome())
}

// A relaunch that ran and reported a fresh start relays the sandbox's own
// reason for it.
func TestSandboxRelaunchReportRelaysAFreshStart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const freshReason = "af recorded no conversation id for this codex session"
	srv, httpSrv := relaunchWireServer(t, map[string]any{
		"ok":           true,
		"conversation": map[string]any{"session": "B-7 item", "carried": false, "carry_failure": freshReason},
	})
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramCodex, "")

	require.NoError(t, inst.RelaunchInPlace())
	require.Equal(t, HandoffConversation{CarryFailure: freshReason}, inst.ResumeConversationOutcome())
}

// A report for a different session cannot answer for this one.
func TestSandboxRelaunchReportForAnotherSessionDoesNotCarry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	srv, httpSrv := relaunchWireServer(t, carriedReport("some other session", tmux.ProgramClaude, "conv-1"))
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "conv-1")

	require.NoError(t, inst.RelaunchInPlace())
	outcome := inst.ResumeConversationOutcome()
	require.False(t, outcome.Carried)
	require.Equal(t, "the sandbox's relaunch report describes a different session", outcome.CarryFailure)
}

// A report that names no session at all cannot be bound to this one.
func TestSandboxRelaunchReportWithoutASessionDoesNotCarry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	srv, httpSrv := relaunchWireServer(t, map[string]any{
		"ok":           true,
		"conversation": map[string]any{"carried": true},
	})
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "conv-1")

	require.NoError(t, inst.RelaunchInPlace())
	outcome := inst.ResumeConversationOutcome()
	require.False(t, outcome.Carried)
	require.Equal(t, "the sandbox's relaunch report did not name the session it describes", outcome.CarryFailure)
}

// A carried report for a different conversation than this session's recorded
// one cannot answer for this session's conversation.
func TestSandboxRelaunchReportForAnotherConversationDoesNotCarry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	srv, httpSrv := relaunchWireServer(t, carriedReport("B-7 item", tmux.ProgramClaude, "conv-2"))
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "conv-1")

	require.NoError(t, inst.RelaunchInPlace())
	outcome := inst.ResumeConversationOutcome()
	require.False(t, outcome.Carried)
	require.Equal(t, "the sandbox's relaunch resumed a different conversation than this session's recorded one", outcome.CarryFailure)
}

// An agent-server that predates the report answers the relaunch without one:
// the response has no conversation field, and the driving daemon must produce
// exactly the answer it gives today.
func TestSandboxRelaunchFromAnOlderBuildKeepsTodaysAnswer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const sandboxCarryFailure = "this session runs in a sandbox, and af cannot prove from the driving daemon that its respawn carried the conversation"
	srv, httpSrv := relaunchWireServer(t, map[string]any{"ok": true})
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "conv-1")

	require.NoError(t, inst.RelaunchInPlace())
	require.Equal(t, HandoffConversation{CarryFailure: sandboxCarryFailure}, inst.ResumeConversationOutcome(),
		"an older sandbox build sends no report, so the answer is exactly today's")
}

// A relaunch verdict reported by a retired runtime is not this runtime's
// report: the record binds to the remote client that produced it.
func TestSandboxRelaunchReportBindsToTheRuntimeThatProducedIt(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const sandboxCarryFailure = "this session runs in a sandbox, and af cannot prove from the driving daemon that its respawn carried the conversation"
	srv, httpSrv := relaunchWireServer(t, carriedReport("B-7 item", tmux.ProgramClaude, "conv-1"))
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "conv-1")

	// A second client takes over the binding before the retired one's
	// relaunch call resolves — the late answer is dropped, not recorded.
	inst.mu.RLock()
	oldRC := inst.remoteClient
	inst.mu.RUnlock()
	newRC := srv.client(t, httpSrv.URL)
	inst.mu.Lock()
	inst.remoteClient = newRC
	inst.agentSrv = nil
	inst.mu.Unlock()
	retired := &remoteAgentServer{rc: oldRC, inst: inst}

	require.NoError(t, retired.Relaunch())
	require.Equal(t, HandoffConversation{CarryFailure: sandboxCarryFailure}, inst.ResumeConversationOutcome(),
		"a retired runtime's report must not be attributed to its replacement")
}

// A re-provision — the relaunch declined because the sandbox cannot — is the
// branch that ran, and its answer is a truthful not-carried.
func TestSandboxReprovisionReportsNotCarried(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	inst := exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})

	replaced := 0
	require.NoError(t, decideRecovery(inst,
		func(*Instance) error { return ErrRelaunchUnsupported },
		func(*Instance) error { replaced++; return nil }))
	require.Equal(t, 1, replaced, "precondition: the re-provision branch ran")
	require.Equal(t, HandoffConversation{CarryFailure: sandboxReprovisionedReason}, inst.ResumeConversationOutcome())
}

// --force-reap skips the relaunch outright; the re-provision's answer is the
// same truthful not-carried.
func TestSandboxForceReapReportsNotCarried(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	inst := exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	inst.SetForceSandboxReplacement(true)

	relaunched := 0
	require.NoError(t, decideRecovery(inst,
		func(*Instance) error { relaunched++; return nil },
		func(*Instance) error { return nil }))
	require.Zero(t, relaunched, "precondition: force-reap bypasses the relaunch")
	require.Equal(t, HandoffConversation{CarryFailure: sandboxReprovisionedReason}, inst.ResumeConversationOutcome())
}

// The answer belongs to the branch that actually ran: a relaunch that carried
// leaves a carried verdict, but the re-provision a LATER respawn takes
// replaces it — the carried report must never describe a respawn that did
// not run it.
func TestSandboxRespawnOutcomeComesFromTheBranchThatRan(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	srv, httpSrv := relaunchWireServer(t, carriedReport("B-7 item", tmux.ProgramClaude, "conv-1"))
	inst := sandboxRelaunchSession(t, srv, httpSrv.URL, tmux.ProgramClaude, "conv-1")

	require.NoError(t, inst.RelaunchInPlace())
	require.True(t, inst.ResumeConversationOutcome().Carried,
		"precondition: the first respawn's relaunch carried")

	inst.SetForceSandboxReplacement(true)
	relaunched, replaced := 0, 0
	require.NoError(t, decideRecovery(inst,
		func(*Instance) error { relaunched++; return nil },
		func(*Instance) error { replaced++; return nil }))
	require.Zero(t, relaunched, "force-reap must not relaunch")
	require.Equal(t, 1, replaced)
	require.Equal(t, HandoffConversation{CarryFailure: sandboxReprovisionedReason}, inst.ResumeConversationOutcome(),
		"the earlier relaunch's carried verdict must not describe this respawn's re-provision")
}

// A relaunch that refused to fall back (a reachable sandbox that answered an
// error) leaves the respawn's outcome unproven — nothing recorded a verdict.
func TestSandboxFailedRelaunchLeavesTheOutcomeUnproven(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const sandboxCarryFailure = "this session runs in a sandbox, and af cannot prove from the driving daemon that its respawn carried the conversation"
	inst := exactResumeSession(t, tmux.ProgramClaude)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: "conv-1", CapturedAt: time.Now()}}}
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})

	replaced := 0
	err := decideRecovery(inst,
		func(*Instance) error { return errors.New("connection refused") },
		func(*Instance) error { replaced++; return nil })
	require.Error(t, err)
	require.Zero(t, replaced, "a reachable-but-refused relaunch never falls back to re-provision")
	require.Equal(t, HandoffConversation{CarryFailure: sandboxCarryFailure}, inst.ResumeConversationOutcome())
}

// --- the reporting half, inside the fix ---

// RelaunchReporting is the wire call that fetches the verdict: the response's
// conversation field rides back to the caller, and a server that answers
// without one — an older build — yields nil.
func TestRemoteAgentServerRelaunchReportingReadsTheWireVerdict(t *testing.T) {
	report := map[string]any{
		"session":      "probe",
		"carried":      true,
		"conversation": map[string]any{"agent": "claude", "id": "conv-1"},
	}
	srv := newTabWireServer(t).route("/v1/agent/relaunch", map[string]any{"ok": true, "conversation": report})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	as := &remoteAgentServer{rc: srv.client(t, httpSrv.URL)}

	got, err := as.RelaunchReporting()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "probe", got.Session)
	assert.True(t, got.Carried)
	assert.Equal(t, AgentConversationData{Agent: "claude", ID: "conv-1"}, got.Conversation)
}

func TestRemoteAgentServerRelaunchReportingIsNilForAnOlderBuild(t *testing.T) {
	srv := newTabWireServer(t).route("/v1/agent/relaunch", map[string]any{"ok": true})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	as := &remoteAgentServer{rc: srv.client(t, httpSrv.URL)}

	got, err := as.RelaunchReporting()
	require.NoError(t, err)
	assert.Nil(t, got, "a server that predates the field answers without it")
}

// A request that joins a running relaunch shares its verdict — the attempt's
// report joins its result, so every request the attempt served answers alike.
func TestRelaunchJoinerSharesTheAttemptReport(t *testing.T) {
	srv := &localAgentServer{}
	release, started := make(chan struct{}), make(chan struct{})
	report := &RelaunchConversationReport{Session: "t", Carried: true}
	leader := make(chan *RelaunchConversationReport, 1)
	go func() {
		got, _ := srv.relaunchReporting(func() (*RelaunchConversationReport, error) {
			close(started)
			<-release
			return report, nil
		})
		leader <- got
	}()
	<-started
	joined := make(chan *RelaunchConversationReport, 1)
	go func() {
		got, _ := srv.relaunchReporting(func() (*RelaunchConversationReport, error) {
			return nil, errors.New("a second relaunch must not run")
		})
		joined <- got
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	require.Same(t, report, <-leader)
	require.Same(t, report, <-joined, "the joiner gets the attempt's own report")
}

// respawnConversationPlan is the verdict the in-sandbox relaunch computes on
// its own store: an exact resume names the recorded conversation it resumes,
// a worktree-scoped resume carries without naming one, and everything else is
// a fresh start with its reason.
func TestRespawnConversationPlanNamesTheResumedConversation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	const convID = "019f386f-7206-7fc2-803b-f7045e07a242"
	inst := exactResumeSession(t, tmux.ProgramCodex)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramCodex, ID: convID, CapturedAt: time.Now()}}}
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-17-35-"+convID+".jsonl", inst.GetWorktreePath())

	outcome, resumed := inst.respawnConversationPlan()
	require.True(t, outcome.Carried)
	require.Equal(t, convID, resumed.ID, "an exact resume names the conversation it resumes")
	require.Equal(t, tmux.ProgramCodex, resumed.Agent)

	// The worktree-scoped forms carry too, but name no conversation.
	inst = exactResumeSession(t, tmux.ProgramClaude)
	outcome, resumed = inst.respawnConversationPlan()
	require.True(t, outcome.Carried)
	require.True(t, resumed.Empty())

	// A session with nothing resumable reports the fresh-start reason.
	inst = exactResumeSession(t, tmux.ProgramAmp)
	outcome, resumed = inst.respawnConversationPlan()
	require.Equal(t, "af recorded no conversation id for this amp session", outcome.CarryFailure)
	require.True(t, resumed.Empty())
}
