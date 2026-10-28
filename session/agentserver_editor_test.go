package session

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// W5.4 (#5571): EnsureEditor asks the host's agent-server for the workspace
// editor, and says so plainly when that host's af predates the route.
func TestRemoteAgentServer_EnsureEditorReportsTheHostsState(t *testing.T) {
	for _, state := range []string{agentproto.EditorStateReady, agentproto.EditorStateStarting, agentproto.EditorStateMissing} {
		ws := newTabWireServer(t).
			route("/v1/health", map[string]any{"ok": true, "features": []string{agentproto.EditorFeature}}).
			route(agentproto.EditorPath, map[string]any{"state": state, "message": "install code-server"})
		srv := httptest.NewServer(ws.mux)
		s := &remoteAgentServer{rc: ws.client(t, srv.URL)}
		got, err := s.EnsureEditor(t.Context())
		srv.Close()
		require.NoError(t, err)
		assert.Equal(t, state, got.State)
		assert.Equal(t, "install code-server", got.Message)
	}
}

func TestRemoteAgentServer_EnsureEditorRefusesAHostWithoutTheFeature(t *testing.T) {
	ws := newTabWireServer(t).route("/v1/health", map[string]any{"ok": true, "features": []string{"tab-api"}})
	srv := httptest.NewServer(ws.mux)
	defer srv.Close()
	s := &remoteAgentServer{rc: ws.client(t, srv.URL)}
	_, err := s.EnsureEditor(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update af there")
}

func TestRemoteAgentServer_DialEditorRefusesAfterKill(t *testing.T) {
	s := &remoteAgentServer{closed: true}
	_, err := s.DialEditor(t.Context())
	require.Error(t, err, "an editor relay must not outlive the session")
}
