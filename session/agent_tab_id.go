package session

import (
	"fmt"
)

// The agent tab's stable id across the daemon and a sandbox (#5997).
//
// Every tab but the agent tab is created by the daemon under its own id, which
// the sandbox's agent-server uses verbatim, so both rosters agree on them. The
// agent tab is seeded independently on each side, so the two minted different
// ids and an agent-tab id sent to the sandbox was tab_gone there. The daemon is
// the id authority: it tells the sandbox which id its agent tab carries, once per
// connection, and from then on the agent tab is addressed like any other.
//
// A sandbox that predates the "agent-tab-id" feature cannot be told, so the
// agent tab alone keeps riding the legacy ordinal bridge there (exact only
// because the agent tab is pinned to slot 0).

// agentTabIDFeature is the /v1/health token for POST /v1/agent/agent-tab-id.
// daemon.agentServerWireFeatures advertises the same token.
const agentTabIDFeature = "agent-tab-id"

// AgentTabIDFeature is exported for the agent-server's /v1/health advertisement.
const AgentTabIDFeature = agentTabIDFeature

// AdoptAgentTabID gives the agent tab (slot 0) the daemon's stable id. It is
// idempotent, and refuses an id another tab already holds or an instance with no
// agent tab, so a stale or hostile id cannot make the roster ambiguous.
func (i *Instance) AdoptAgentTabID(id string) error {
	if id == "" {
		return fmt.Errorf("an agent tab id is required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.Tabs) == 0 || i.Tabs[0].Kind != TabKindAgent {
		return fmt.Errorf("session %q has no agent tab to name", i.Title)
	}
	if i.Tabs[0].ID == id {
		return nil
	}
	if idx, taken := i.tabIndexByIDLocked(id); taken && idx != 0 {
		return fmt.Errorf("session %q: id %q already names another tab", i.Title, id)
	}
	i.replaceTabFieldLocked(0, func(c *Tab) { c.ID = id })
	return nil
}

// agentTabIDLocked returns the agent tab's id, or "". Caller holds i.mu.
func (i *Instance) agentTabIDLocked() string {
	if len(i.Tabs) > 0 && i.Tabs[0].Kind == TabKindAgent {
		return i.Tabs[0].ID
	}
	return ""
}

// agentTabNative reports whether the agent tab id tabID can ride the id-native
// plane: the sandbox must hold the daemon's id for it. When the sandbox
// advertises the feature but has not been told yet, tell it now.
func (s *remoteAgentServer) agentTabNative(tabID string) bool {
	s.mu.Lock()
	synced := s.agentTabSynced == tabID
	s.mu.Unlock()
	if synced {
		return true
	}
	if !s.hasFeatureCached(agentTabIDFeature) {
		return false
	}
	return s.syncAgentTabID(tabID) == nil
}

func (s *remoteAgentServer) syncAgentTabID(tabID string) error {
	if err := s.rc.setAgentTabID(tabID); err != nil {
		return err
	}
	s.mu.Lock()
	s.agentTabSynced = tabID
	s.mu.Unlock()
	return nil
}

// ReconcileAgentTabID keeps the sandbox's agent tab on the daemon's id: it
// tells a freshly attached (or restarted) sandbox once, and re-tells one whose
// roster no longer holds the id. A no-op for a local instance, a sandbox without
// the feature, or one that cannot be reached; the next sweep tries again.
func (i *Instance) ReconcileAgentTabID() {
	i.mu.RLock()
	rs := i.remoteTabServerCachedLocked()
	hasClient := i.remoteClient != nil
	remote := i.capabilitiesLocked().Workspace == WorkspaceRemote
	id := i.agentTabIDLocked()
	i.mu.RUnlock()
	if !remote || id == "" {
		return
	}
	if rs == nil && hasClient {
		rs, _ = i.AgentServer().(*remoteAgentServer)
	}
	if rs == nil {
		return
	}
	rs.idNative()
	if !rs.hasFeatureCached(agentTabIDFeature) {
		return
	}
	roster, err := rs.rc.tabs()
	if err != nil {
		return
	}
	for _, td := range roster {
		if td.ID == id && td.Kind == TabKindAgent {
			rs.mu.Lock()
			rs.agentTabSynced = id
			rs.mu.Unlock()
			return
		}
	}
	// The sandbox's agent tab carries another id (a new connection, or a sandbox
	// that restarted and minted a fresh one).
	rs.mu.Lock()
	rs.agentTabSynced = ""
	rs.mu.Unlock()
	_ = rs.syncAgentTabID(id)
}

// setAgentTabID tells the sandbox which stable id its agent tab carries (#5997).
func (c *remoteAgentClient) setAgentTabID(id string) error {
	return c.call("/v1/agent/agent-tab-id", agentTabIDReq{ID: id}, nil)
}

// agentTabIDReq names the id the daemon assigns the sandbox's agent tab.
type agentTabIDReq struct {
	ID string `json:"id"`
}
