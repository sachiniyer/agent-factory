package app

import "github.com/sachiniyer/agent-factory/session"

// firstAutoOpenCandidate picks the pane to auto-open on a cold start when no row
// is selected. It prefers the first NON-reserved instance so a relaunch (e.g.
// right after an auto-update) doesn't greet the user with the daemon-managed
// root agent front-and-center — the "kill right after a reload lands on root"
// trap (#1238). Root pins first in the store order (ui/store/order.go), so
// without this the auto-opened pane is always root. Falls back to the first
// instance (root) only when root is the sole session. Returns nil for an empty
// list.
func firstAutoOpenCandidate(instances []*session.Instance) *session.Instance {
	if len(instances) == 0 {
		return nil
	}
	for _, inst := range instances {
		if !session.IsReservedTitle(inst.Title) {
			return inst
		}
	}
	return instances[0]
}
