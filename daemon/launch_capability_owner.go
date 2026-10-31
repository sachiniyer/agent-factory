package daemon

import sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"

// claimLaunchCapabilityOwnership makes this process the one that owns session
// launch capabilities (#4731). The daemon outlives the sessions it launched and
// re-establishes each live session's default-command when it restores it, which is
// what a short-lived CLI process, or a daemon replaced by an upgrade, cannot do.
func claimLaunchCapabilityOwnership() {
	sessiontmux.SetLaunchCapabilityOwner()
}
