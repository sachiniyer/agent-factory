package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// relaunchFake is a headless agent-server that can relaunch (W7.1).
type relaunchFake struct {
	*fakeHeadlessAgentServer
	relaunched int
	err        error
}

func (r *relaunchFake) Relaunch() error            { r.relaunched++; return r.err }
func (r *relaunchFake) CanRelaunch() (bool, error) { return true, nil }

var _ session.AgentRelauncher = (*relaunchFake)(nil)

func postRelaunch(hs *headlessServer) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	hs.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/agent/relaunch", nil))
	return rec
}

// The route stops and restarts the agent in this workspace and answers ok; it
// touches nothing else (no archive, no kill).
func TestHeadlessRelaunchRoute(t *testing.T) {
	fake := &relaunchFake{fakeHeadlessAgentServer: newFakeHeadlessAgentServer()}
	hs := &headlessServer{as: fake, title: "probe", events: newEventsHub()}

	rec := postRelaunch(hs)
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var resp agentOKResponse
	dataInto(t, env, &resp)
	assert.True(t, resp.OK)
	assert.Equal(t, 1, fake.relaunched)
	assert.False(t, fake.killed, "a relaunch must not kill the workspace")
}

func TestHeadlessRelaunchRouteSurfacesFailure(t *testing.T) {
	fake := &relaunchFake{fakeHeadlessAgentServer: newFakeHeadlessAgentServer(), err: errors.New("cannot confirm the agent stopped")}
	hs := &headlessServer{as: fake, title: "probe", events: newEventsHub()}
	rec := postRelaunch(hs)
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, "cannot confirm the agent stopped")
}

// A server whose runtime cannot relaunch refuses with the typed error rather
// than succeeding without having done anything.
func TestHeadlessRelaunchRouteRefusesWhenUnsupported(t *testing.T) {
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), title: "probe", events: newEventsHub()}
	env := decodeEnvelope(t, postRelaunch(hs))
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, session.ErrRelaunchUnsupported.Error())
}
