package app

import "github.com/sachiniyer/agent-factory/session"

// firstAutoOpenCandidate picks the pane to auto-open on a cold start when no row
// is selected. It returns the daemon-managed root session (the instance whose
// title session.IsReservedTitle matches) wherever it sits in the list, so launch
// lands on root. When no root is present — a repo without [root_agent] — it
// returns the first instance. Returns nil for an empty list.
func firstAutoOpenCandidate(instances []*session.Instance) *session.Instance {
	if len(instances) == 0 {
		return nil
	}
	for _, inst := range instances {
		if session.IsReservedTitle(inst.Title) {
			return inst
		}
	}
	return instances[0]
}
