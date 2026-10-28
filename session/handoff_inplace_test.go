package session

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

func swapServer(t *testing.T, features []string) (*remoteAgentServer, *tabWireServer) {
	t.Helper()
	srv := newTabWireServer(t).
		route("/v1/agent/swap-agent", map[string]any{"ok": true, "program": "/home/dev/.local/bin/gemini"}).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": features})
	httpSrv := httptest.NewServer(srv.mux)
	t.Cleanup(httpSrv.Close)
	return &remoteAgentServer{rc: srv.client(t, httpSrv.URL), idPlaneKnown: true, features: features}, srv
}

func sandboxInstance(t *testing.T, as AgentServer) *Instance {
	t.Helper()
	return sandboxInstanceOn(t, "ssh", as)
}

func sandboxInstanceOn(t *testing.T, backendType string, as AgentServer) *Instance {
	t.Helper()
	b := newInertSandboxBackend(backendType)
	return &Instance{Title: "sb", backend: b, started: true, agentSrv: as, Program: tmux.ProgramClaude,
		Tabs: []*Tab{newRemoteAgentTab()}}
}

func TestRemoteAgentServer_SwapWire(t *testing.T) {
	as, srv := swapServer(t, []string{"tab-api", SwapAgentFeature})
	can, err := as.CanSwapAgent()
	require.NoError(t, err)
	assert.True(t, can)
	program, err := as.SwapAgentTo("gemini")
	require.NoError(t, err)
	assert.Equal(t, "/home/dev/.local/bin/gemini", program, "the host-resolved command comes back")
	assert.Equal(t, "gemini", srv.bodies["/v1/agent/swap-agent"]["target"])
	assert.Equal(t, "POST", srv.methods["/v1/agent/swap-agent"])
	srv.assertAuth(t, "/v1/agent/swap-agent")

	old, _ := swapServer(t, []string{"tab-api"})
	can, err = old.CanSwapAgent()
	require.NoError(t, err)
	assert.False(t, can, "a sandbox that does not advertise the route is never sent it")
}

// can_handoff is true exactly where the sandbox's agent-server supports the swap.
func TestSandboxHandoffCapabilityFollowsTheAdvertisement(t *testing.T) {
	supports, _ := swapServer(t, []string{SwapAgentFeature})
	assert.True(t, sandboxInstance(t, supports).Capabilities().Handoff)

	old, _ := swapServer(t, []string{"tab-api"})
	assert.False(t, sandboxInstance(t, old).Capabilities().Handoff,
		"an older agent-server must not be offered a handoff it cannot perform")

	assert.False(t, sandboxInstance(t, &deadRemoteAgentServer{title: "sb"}).Capabilities().Handoff)

	// docker embeds the same remote backend but is not driven by this slice.
	assert.False(t, sandboxInstanceOn(t, "docker", supports).Capabilities().Handoff)
}

func TestSandboxHandoffPlanAndSwapGoThroughTheAgentServer(t *testing.T) {
	as, srv := swapServer(t, []string{SwapAgentFeature})
	inst := sandboxInstance(t, as)
	b := inst.backend

	plan, err := b.PrepareAgentSwap(inst, tmux.ProgramGemini)
	require.NoError(t, err)
	assert.Equal(t, tmux.ProgramGemini, plan.target)
	_, hit := srv.bodies["/v1/agent/swap-agent"]
	assert.False(t, hit, "preparing must not touch the running agent")

	require.NoError(t, b.SwapAgent(inst, plan))
	assert.Equal(t, "gemini", srv.bodies["/v1/agent/swap-agent"]["target"])

	_, err = b.PrepareAgentSwap(inst, "not-an-agent")
	require.Error(t, err)
}

func TestSandboxHandoffRefusedAgainstAnOldOrAbsentSandbox(t *testing.T) {
	old, srv := swapServer(t, []string{"tab-api"})
	inst := sandboxInstance(t, old)
	_, err := inst.backend.PrepareAgentSwap(inst, tmux.ProgramGemini)
	assert.ErrorIs(t, err, ErrHandoffUnsupported)
	_, hit := srv.bodies["/v1/agent/swap-agent"]
	assert.False(t, hit)

	dead := sandboxInstance(t, &deadRemoteAgentServer{title: "sb"})
	_, err = dead.backend.PrepareAgentSwap(dead, tmux.ProgramGemini)
	assert.ErrorIs(t, err, ErrHandoffUnsupported)
}

// Account handoff is W7.6: a scoped session is refused before anything is
// stopped, never swapped with its account carried or dropped silently.
func TestSwapInPlaceRefusesAnAccountScopedSession(t *testing.T) {
	inst := &Instance{Title: "t", Account: "work"}
	_, err := inst.swapAgentInPlace(tmux.ProgramGemini)
	assert.ErrorIs(t, err, ErrSwapNeedsAccountHandoff)
}

// A remote swap leaves the row as a local handoff would: the OUTGOING runtime's
// command evidence is replaced by the host-resolved command of the new one, so
// the session reports (and the readiness check uses) the incoming agent.
func TestSandboxSwapReplacesRuntimeCommandEvidence(t *testing.T) {
	as, _ := swapServer(t, []string{SwapAgentFeature})
	inst := sandboxInstance(t, as)
	inst.setRuntimeProgramLocked("claude --session-id abc")
	require.Equal(t, tmux.ProgramClaude, inst.CurrentAgentName(), "premise: the outgoing evidence names claude")

	plan, err := inst.backend.PrepareAgentSwap(inst, tmux.ProgramGemini)
	require.NoError(t, err)
	require.NoError(t, inst.backend.SwapAgent(inst, plan))

	inst.mu.RLock()
	got := inst.runtimeProgram
	inst.mu.RUnlock()
	assert.Equal(t, "/home/dev/.local/bin/gemini", got)
	assert.Equal(t, tmux.ProgramGemini, inst.CurrentAgentName(), "a handoff back to claude must not be refused as 'already claude'")
}

// The mission brief is assembled on the remote path too — before the swap, so a
// failure leaves the outgoing agent untouched.
func TestSandboxSwapCapturesTheMissionBriefBeforeSwapping(t *testing.T) {
	as, srv := swapServer(t, []string{SwapAgentFeature})
	inst := sandboxInstance(t, as)
	plan, err := inst.backend.PrepareAgentSwap(inst, tmux.ProgramGemini)
	require.NoError(t, err)

	captured := 0
	plan = plan.WithPostStopCapture(func() error { captured++; return nil })
	require.NoError(t, inst.backend.SwapAgent(inst, plan))
	assert.Equal(t, 1, captured, "the brief must be captured on the remote swap path")

	boom := errors.New("no handoff to capture")
	failing := plan.WithPostStopCapture(func() error { return boom })
	delete(srv.bodies, "/v1/agent/swap-agent")
	require.ErrorIs(t, inst.backend.SwapAgent(inst, failing), boom)
	_, swapped := srv.bodies["/v1/agent/swap-agent"]
	assert.False(t, swapped, "a failed capture must abort before the sandbox's agent is touched")
}

// An account-scoped sandbox session is refused up front — in daemon-side
// preparation, with the outgoing credentials still in place — with a message
// that says what to do instead.
func TestSandboxHandoffRefusesAnAccountScopedSessionUpFront(t *testing.T) {
	as, srv := swapServer(t, []string{SwapAgentFeature})
	inst := sandboxInstance(t, as)
	inst.Account = "work"

	_, err := inst.backend.PrepareAgentSwap(inst, tmux.ProgramGemini)
	require.ErrorIs(t, err, ErrSwapNeedsAccountHandoff)
	assert.Contains(t, err.Error(), `"work"`)
	assert.Contains(t, err.Error(), "without --account")
	_, hit := srv.bodies["/v1/agent/swap-agent"]
	assert.False(t, hit)

	// The --account handoff entry point is refused for any sandbox session.
	plain := sandboxInstance(t, as)
	require.ErrorIs(t, plain.RefuseOffBoxAccountHandoff(), ErrSwapNeedsAccountHandoff)
	assert.NoError(t, (&Instance{Title: "local"}).RefuseOffBoxAccountHandoff())
}
