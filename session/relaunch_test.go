package session

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relaunch route rides the same envelope as every other control call, and
// CanRelaunch reads the /v1/health feature token — so an older sandbox build
// that lacks the token is never sent a route it does not have.
func TestRemoteAgentServer_RelaunchWire(t *testing.T) {
	srv := newTabWireServer(t).
		route("/v1/agent/relaunch", map[string]any{"ok": true}).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": []string{"tab-api", RelaunchFeature}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	as := &remoteAgentServer{rc: srv.client(t, httpSrv.URL)}

	can, err := as.CanRelaunch()
	require.NoError(t, err)
	assert.True(t, can)

	require.NoError(t, as.Relaunch())
	assert.Equal(t, "POST", srv.methods["/v1/agent/relaunch"])
	srv.assertAuth(t, "/v1/agent/relaunch")
}

func TestRemoteAgentServer_CanRelaunchFalseWithoutTheFeature(t *testing.T) {
	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "version": "old", "features": []string{"tab-api"}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	as := &remoteAgentServer{rc: srv.client(t, httpSrv.URL)}
	can, err := as.CanRelaunch()
	require.NoError(t, err)
	assert.False(t, can, "an agent-server that does not advertise relaunch must not be sent the route")
}

func TestRemoteAgentServer_CanRelaunchSurfacesUnreachable(t *testing.T) {
	srv := newTabWireServer(t)
	httpSrv := httptest.NewServer(srv.mux)
	url := httpSrv.URL
	httpSrv.Close() // nothing answers: unknown, not "no"
	as := &remoteAgentServer{rc: srv.client(t, url)}
	_, err := as.CanRelaunch()
	assert.Error(t, err, "an unanswered probe must stay distinguishable from 'cannot relaunch'")
}

func TestDeadRemoteAgentServerCannotRelaunch(t *testing.T) {
	as := &deadRemoteAgentServer{title: "t"}
	can, err := as.CanRelaunch()
	assert.False(t, can)
	assert.ErrorIs(t, err, ErrRemoteSandboxNotProvisioned)
	assert.ErrorIs(t, as.Relaunch(), ErrRemoteSandboxNotProvisioned)
}

// Re-provisioning after a failed relaunch is reserved for "cannot relaunch" and
// "no sandbox". A reachable sandbox that refused, or a transport failure, still
// holds the work and must never be replaced.
func TestRelaunchFallsBackToReprovisionOnlyWhenTheSandboxIsAbsentOrOld(t *testing.T) {
	assert.True(t, relaunchFallsBackToReprovision(ErrRelaunchUnsupported))
	assert.True(t, relaunchFallsBackToReprovision(fmt.Errorf("wrapped: %w", ErrRemoteSandboxNotProvisioned)))
	assert.False(t, relaunchFallsBackToReprovision(errors.New("connection refused")))
	assert.False(t, relaunchFallsBackToReprovision(errors.New("relaunch: cannot confirm the agent stopped")))
	assert.False(t, relaunchFallsBackToReprovision(nil))
}
