package session

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

func swapServer(t *testing.T, features []string) (*remoteAgentServer, *tabWireServer) {
	t.Helper()
	srv := newTabWireServer(t).
		route("/v1/agent/swap-agent", map[string]any{"ok": true}).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": features})
	httpSrv := httptest.NewServer(srv.mux)
	t.Cleanup(httpSrv.Close)
	return &remoteAgentServer{rc: srv.client(t, httpSrv.URL), idPlaneKnown: true, features: features}, srv
}

func sandboxInstance(t *testing.T, as AgentServer) *Instance {
	t.Helper()
	var b Backend
	for _, backend := range sandboxBackends() {
		b = backend
		break
	}
	return &Instance{Title: "sb", backend: b, started: true, agentSrv: as, Program: tmux.ProgramClaude,
		Tabs: []*Tab{newRemoteAgentTab()}}
}

func TestRemoteAgentServer_SwapWire(t *testing.T) {
	as, srv := swapServer(t, []string{"tab-api", SwapAgentFeature})
	can, err := as.CanSwapAgent()
	require.NoError(t, err)
	assert.True(t, can)
	require.NoError(t, as.SwapAgentTo("gemini"))
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
	assert.ErrorIs(t, inst.swapAgentInPlace(tmux.ProgramGemini), ErrSwapNeedsAccountHandoff)
}
