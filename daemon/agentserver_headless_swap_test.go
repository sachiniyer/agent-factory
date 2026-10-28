package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// swapFake is a headless agent-server that can swap its agent in place (W7.5).
type swapFake struct {
	*fakeHeadlessAgentServer
	targets []string
	err     error
}

func (s *swapFake) SwapAgentTo(target string) (string, error) {
	s.targets = append(s.targets, target)
	return "/host/bin/" + target, s.err
}
func (s *swapFake) CanSwapAgent() (bool, error) { return true, nil }

var _ session.AgentSwapper = (*swapFake)(nil)

func postSwap(hs *headlessServer, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	hs.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/agent/swap-agent", strings.NewReader(body)))
	return rec
}

func TestHeadlessSwapAgentRoute(t *testing.T) {
	fake := &swapFake{fakeHeadlessAgentServer: newFakeHeadlessAgentServer()}
	hs := &headlessServer{as: fake, title: "probe", events: newEventsHub()}

	rec := postSwap(hs, `{"target":"gemini"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var resp agentSwapResponse
	dataInto(t, env, &resp)
	assert.True(t, resp.OK)
	assert.Equal(t, "/host/bin/gemini", resp.Program, "the host-resolved command travels back for the daemon's evidence")
	assert.Equal(t, []string{"gemini"}, fake.targets)
	assert.False(t, fake.killed, "a swap must not kill the workspace")
}

func TestHeadlessSwapAgentRouteRefusesWhenUnsupported(t *testing.T) {
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), title: "probe", events: newEventsHub()}
	env := decodeEnvelope(t, postSwap(hs, `{"target":"gemini"}`))
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, session.ErrHandoffUnsupported.Error())
}

func TestHeadlessHealthAdvertisesAgentSwap(t *testing.T) {
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), events: newEventsHub()}
	rec := httptest.NewRecorder()
	hs.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	env := decodeEnvelope(t, rec)
	var health agentHealthResponse
	dataInto(t, env, &health)
	assert.Contains(t, health.Features, "agent-swap")
}
