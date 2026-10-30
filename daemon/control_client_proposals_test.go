package daemon

import (
	"strings"
	"testing"
)

// Every proposal route the daemon serves must be callable through the local
// client dispatcher; a route missing from proposalMethods is refused by
// CallProposal before it reaches the daemon (#6287).
func TestProposalMethodsCoverEveryProposalRoute(t *testing.T) {
	for _, route := range httpRoutes {
		name := strings.TrimPrefix(route.Path, "/v1/")
		if !strings.Contains(name, "Proposal") {
			continue
		}
		if !proposalMethods[name] {
			t.Errorf("route %s is not in proposalMethods, so the local client refuses it", route.Path)
		}
	}
	if err := CallProposal("NoSuchProposalMethod", nil, nil); err == nil {
		t.Error("CallProposal accepted an unknown method")
	}
}
