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

// agentTabSandbox is a sandbox that advertises "agent-tab-id", records the id it
// is told, and reports an agent tab under whatever id it currently holds.
type agentTabSandbox struct {
	mu       sync.Mutex
	held     string // the id its agent tab carries
	told     []string
	failTell bool
}

func newAgentTabSandbox(t *testing.T, held string) (*agentTabSandbox, *remoteAgentClient) {
	t.Helper()
	sb := &agentTabSandbox{held: held}
	ws := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "features": []string{"tab-api", tabIDAddressingFeature, agentTabIDFeature}}).
		route("/v1/agent/preview", map[string]any{"content": "screen"})
	ws.mux.HandleFunc("/v1/agent/agent-tab-id", func(w http.ResponseWriter, r *http.Request) {
		var req agentTabIDReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		sb.mu.Lock()
		defer sb.mu.Unlock()
		if sb.failTell {
			ws.encode(w, map[string]any{"data": nil, "error": map[string]any{"message": "no"}})
			return
		}
		sb.held = req.ID
		sb.told = append(sb.told, req.ID)
		ws.encode(w, map[string]any{"data": map[string]any{}, "error": nil})
	})
	ws.mux.HandleFunc("/v1/agent/tabs", func(w http.ResponseWriter, r *http.Request) {
		sb.mu.Lock()
		rows := []TabData{{ID: sb.held, Kind: TabKindAgent, Name: agentTabName}}
		sb.mu.Unlock()
		ws.encode(w, map[string]any{"data": map[string]any{"tabs": rows}, "error": nil})
	})
	srv := httptest.NewServer(ws.mux)
	t.Cleanup(srv.Close)
	rc := ws.client(t, srv.URL)
	return sb, rc
}

func (sb *agentTabSandbox) tellings() []string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return append([]string(nil), sb.told...)
}

// The agent tab is addressed by id like every other tab once the sandbox holds
// the daemon's id (#5997).
func TestAgentTabRidesTheIDPlaneOnceTheSandboxIsTold(t *testing.T) {
	sb, rc := newAgentTabSandbox(t, "sandbox-minted")
	agent := newRemoteAgentTab()
	inst := &Instance{Title: "sb", Tabs: []*Tab{agent}}
	s := &remoteAgentServer{rc: rc, inst: inst}

	require.True(t, s.nativeFor(agent.ID))
	assert.Equal(t, []string{agent.ID}, sb.tellings(), "the sandbox is told the daemon's id")
	require.True(t, s.nativeFor(agent.ID))
	assert.Len(t, sb.tellings(), 1, "told once per connection")
}

func TestAgentTabStaysOnTheBridgeWhenTheSandboxCannotBeTold(t *testing.T) {
	sb, rc := newAgentTabSandbox(t, "sandbox-minted")
	sb.failTell = true
	agent := newRemoteAgentTab()
	s := &remoteAgentServer{rc: rc, inst: &Instance{Title: "sb", Tabs: []*Tab{agent}}}

	assert.False(t, s.nativeFor(agent.ID), "an id the sandbox does not hold must not be sent to it")
	sb.mu.Lock()
	sb.failTell = false
	sb.mu.Unlock()
	assert.True(t, s.nativeFor(agent.ID), "the next operation tells it and goes native")
}

// A sandbox that restarted mints a fresh agent-tab id; the sweep notices the
// roster no longer holds the daemon's and tells it again.
func TestReconcileAgentTabIDRepairsARestartedSandbox(t *testing.T) {
	sb, rc := newAgentTabSandbox(t, "sandbox-minted")
	inst := newOffBoxInstance(rc)
	agentID := inst.Tabs[0].ID

	inst.ReconcileAgentTabID()
	assert.Equal(t, []string{agentID}, sb.tellings())

	inst.ReconcileAgentTabID()
	assert.Len(t, sb.tellings(), 1, "a roster that holds the id needs no call")

	sb.mu.Lock()
	sb.held = "fresh-after-restart"
	sb.mu.Unlock()
	inst.ReconcileAgentTabID()
	assert.Equal(t, []string{agentID, agentID}, sb.tellings())
}

func TestAdoptAgentTabID(t *testing.T) {
	inst := &Instance{Title: "sb", Tabs: []*Tab{newAgentTab(nil), {ID: "other", Kind: TabKindShell, Name: "shell"}}}
	require.NoError(t, inst.AdoptAgentTabID("daemon-id"))
	assert.Equal(t, "daemon-id", inst.Tabs[0].ID)
	require.NoError(t, inst.AdoptAgentTabID("daemon-id"), "idempotent")
	assert.Error(t, inst.AdoptAgentTabID("other"), "an id another tab holds is refused")
	assert.Error(t, inst.AdoptAgentTabID(""))
	assert.Equal(t, "other", inst.Tabs[1].ID)
}
