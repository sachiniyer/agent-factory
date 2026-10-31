package daemon

import (
	"fmt"
)

// agentTabIDRequest is the daemon telling this agent-server which stable id its
// agent tab carries (#5997). The daemon is the id authority for every tab; the
// agent tab was the one seeded independently on each side.
type agentTabIDRequest struct {
	ID string `json:"id"`
}

// agentTabIDAdopter is the Instance method the route binds to, kept off the
// tabKeeper interface so existing roster fakes need not grow it.
type agentTabIDAdopter interface {
	AdoptAgentTabID(id string) error
}

// AgentTabID makes the workspace's agent tab carry the daemon's id. Idempotent.
func (hs *headlessServer) AgentTabID(req agentTabIDRequest, resp *agentOKResponse) error {
	if req.ID == "" {
		return fmt.Errorf("an agent tab id is required")
	}
	tabs, err := hs.tabStore()
	if err != nil {
		return err
	}
	a, ok := tabs.(agentTabIDAdopter)
	if !ok {
		return fmt.Errorf("this agent-server cannot rename its agent tab")
	}
	if err := a.AdoptAgentTabID(req.ID); err != nil {
		return err
	}
	resp.OK = true
	return nil
}
