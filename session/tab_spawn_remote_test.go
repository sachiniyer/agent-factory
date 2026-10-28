package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W4.5 (#5571): a sandbox advertising tab-api serves shell/process tabs
// off-box under the daemon's stable id.

type fakeTabSandbox struct {
	mu      sync.Mutex
	creates []agentTabCreateReq
	closes  []string
}

func newFakeTabSandbox(t *testing.T, features []string) (*remoteAgentClient, *fakeTabSandbox) {
	t.Helper()
	f := &fakeTabSandbox{}
	ws := newTabWireServer(t).route("/v1/health", map[string]any{"ok": true, "features": features})
	ws.mux.HandleFunc("/v1/agent/tab-create", func(w http.ResponseWriter, r *http.Request) {
		var req agentTabCreateReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.creates = append(f.creates, req)
		f.mu.Unlock()
		kind, _ := ParseTabKindName(req.Kind)
		ws.encode(w, map[string]any{"data": map[string]any{"tab": map[string]any{
			"id": req.ID, "name": req.Name, "kind": kind, "command": req.Command}}, "error": nil})
	})
	ws.mux.HandleFunc("/v1/agent/tab-close", func(w http.ResponseWriter, r *http.Request) {
		var req agentTabCloseReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.closes = append(f.closes, req.ID)
		f.mu.Unlock()
		ws.encode(w, map[string]any{"data": map[string]any{}, "error": nil})
	})
	srv := httptest.NewServer(ws.mux)
	t.Cleanup(srv.Close)
	return ws.client(t, srv.URL), f
}

func newOffBoxInstance(rc *remoteAgentClient) *Instance {
	return &Instance{
		Title:        "sb",
		backend:      &HookBackend{},
		started:      true,
		remoteClient: rc,
		Tabs:         []*Tab{newRemoteAgentTab()},
	}
}

func TestOffBoxTabs_CapabilityFollowsAdvertisement(t *testing.T) {
	rc, _ := newFakeTabSandbox(t, []string{"tab-api", tabIDAddressingFeature})
	i := newOffBoxInstance(rc)
	assert.False(t, i.Capabilities().TabManagement, "unknown until the sandbox has answered health")
	require.True(t, i.AgentServer().(*remoteAgentServer).idNative())
	assert.True(t, i.Capabilities().TabManagement)
	require.NoError(t, i.Capabilities().RefuseTabKind(TabKindShell, ""))

	rcOld, _ := newFakeTabSandbox(t, nil)
	old := newOffBoxInstance(rcOld)
	old.AgentServer().(*remoteAgentServer).idNative()
	assert.False(t, old.Capabilities().TabManagement, "an older sandbox keeps today's refusal")
}

func TestOffBoxTabs_CreateIsIdempotentAndCloseGoesByID(t *testing.T) {
	rc, sb := newFakeTabSandbox(t, []string{"tab-api", tabIDAddressingFeature})
	i := newOffBoxInstance(rc)
	i.AgentServer().(*remoteAgentServer).idNative()

	shell, err := i.AddShellTabWithID("shell-id")
	require.NoError(t, err)
	assert.Equal(t, "shell-id", shell.ID)
	assert.Nil(t, shell.tmux)

	proc, err := i.AddProcessTabWithID("proc-id", "btop", "")
	require.NoError(t, err)
	assert.Equal(t, TabKindProcess, proc.Kind)
	assert.Equal(t, "btop", proc.Command)

	// A retry of the same create answers the existing tab and spawns nothing.
	again, err := i.AddProcessTabWithID("proc-id", "btop", "")
	require.NoError(t, err)
	assert.Same(t, proc, again)
	sb.mu.Lock()
	assert.Len(t, sb.creates, 2, "the retry must not reach the sandbox")
	sb.mu.Unlock()
	assert.Len(t, i.Tabs, 3)

	require.NoError(t, i.CloseTabByID("proc-id"))
	sb.mu.Lock()
	assert.Equal(t, []string{"proc-id"}, sb.closes)
	sb.mu.Unlock()
	assert.Len(t, i.Tabs, 2)
}

func TestOffBoxTabs_DistinctCreatesGetDistinctNames(t *testing.T) {
	rc, _ := newFakeTabSandbox(t, []string{"tab-api", tabIDAddressingFeature})
	i := newOffBoxInstance(rc)
	i.AgentServer().(*remoteAgentServer).idNative()
	a, err := i.AddShellTab()
	require.NoError(t, err)
	b, err := i.AddShellTab()
	require.NoError(t, err)
	assert.NotEqual(t, a.Name, b.Name)
	assert.NotEqual(t, a.ID, b.ID)
}
