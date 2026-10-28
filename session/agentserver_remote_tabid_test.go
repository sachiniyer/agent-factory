package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W4.4 (#5571): the remote agent-server speaks the id-native plane when the
// sandbox advertises tab-id-addressing, and keeps the per-operation ordinal
// bridge only for an older one.

func TestRemoteAgentServer_IDPlaneFollowsHealthAdvertisement(t *testing.T) {
	modern := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "version": "v9", "features": []string{"tab-api", tabIDAddressingFeature}}).
		route("/v1/agent/preview", map[string]any{"content": "hi"})
	srv := httptest.NewServer(modern.mux)
	defer srv.Close()
	s := &remoteAgentServer{rc: modern.client(t, srv.URL)}

	require.True(t, s.idNative())
	// PreviewByID reaches the sandbox by id with no daemon-side roster at all.
	snap, err := s.PreviewByID("tab-7", false)
	require.NoError(t, err)
	assert.Equal(t, "hi", snap.Content)
	modern.mu.Lock()
	assert.Equal(t, "tab-7", modern.bodies["/v1/agent/preview"]["tab_id"])
	modern.mu.Unlock()
}

func TestRemoteAgentServer_LegacySandboxBridgesThroughRoster(t *testing.T) {
	legacy := newTabWireServer(t).route("/v1/health", map[string]any{"ok": true})
	srv := httptest.NewServer(legacy.mux)
	defer srv.Close()
	s := &remoteAgentServer{rc: legacy.client(t, srv.URL)}

	require.False(t, s.idNative())
	// No instance to resolve the id against: refused, never pinned to ordinal 0.
	_, err := s.SubscribeTab("tab-7", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no stable-id roster")
	assert.Error(t, s.InputTab("tab-7", []byte("x")))
	assert.Error(t, s.ResizeTab("tab-7", 24, 80))
}

func TestRemoteAgentServer_StaleIDDialIsErrTabGone(t *testing.T) {
	ws := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "features": []string{tabIDAddressingFeature}})
	ws.mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(ws.mux)
	defer srv.Close()
	s := &remoteAgentServer{rc: ws.client(t, srv.URL)}

	_, err := s.SubscribeTab("gone", 0)
	require.ErrorIs(t, err, ErrTabGone)
	s.mu.Lock()
	assert.Empty(t, s.idBrokers, "a gone id must not retain its broker")
	s.mu.Unlock()

	_, err = s.rc.dialStreamByID(context.Background(), "gone")
	require.ErrorIs(t, err, ErrTabGone)
}

func TestRemoteAgentServer_CloseTabStreamDropsOnlyThatBroker(t *testing.T) {
	s := &remoteAgentServer{}
	s.rc = &remoteAgentClient{title: "t"}
	a, err := s.ensureBrokerByID("a")
	require.NoError(t, err)
	_, err = s.ensureBrokerByID("b")
	require.NoError(t, err)
	s.closeTabStream("a")
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.NotContains(t, s.idBrokers, "a")
	assert.Contains(t, s.idBrokers, "b")
	_ = a
}

// The agent tab's id is minted independently on each side, so it must stay on
// the ordinal bridge even against a sandbox that advertises tab-id-addressing
// (Codex P1 on #5575); a tab the daemon created rides the native plane.
func TestRemoteAgentServer_AgentTabStaysOnOrdinalBridge(t *testing.T) {
	modern := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "features": []string{tabIDAddressingFeature}}).
		route("/v1/agent/preview", map[string]any{"content": "screen"})
	srv := httptest.NewServer(modern.mux)
	defer srv.Close()
	agent := newRemoteAgentTab()
	shell := &Tab{ID: "daemon-shell", Name: "shell", Kind: TabKindShell}
	inst := &Instance{Title: "sb", Tabs: []*Tab{agent, shell}}
	s := &remoteAgentServer{rc: modern.client(t, srv.URL), inst: inst}

	assert.False(t, s.nativeFor(agent.ID), "agent tab id is not known to the sandbox")
	assert.True(t, s.nativeFor(shell.ID))

	_, err := s.PreviewByID(agent.ID, false)
	require.NoError(t, err)
	modern.mu.Lock()
	body := modern.bodies["/v1/agent/preview"]
	modern.mu.Unlock()
	assert.Nil(t, body["tab_id"], "the agent tab preview must not send the daemon-minted id")
}
