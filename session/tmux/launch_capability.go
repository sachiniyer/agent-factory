package tmux

import (
	"sync/atomic"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// launchSpec is what a launch admits for one pane command (#4731); the unix
// file turns it into a sessionenv.Capability.
type launchSpec struct {
	agent           string
	account         string
	command         string
	extras          []string
	proof           sessionenv.AccountLaunchProof
	environmentOnly bool
	// perPane marks the session's default-command: tmux re-runs it for every new
	// window, so each pane claims it once and it lives until the session ends.
	perPane bool
}

// launchCapabilityOwner is true only in the long-lived daemon.
//
// A session's default-command names a capability served by the process that
// minted it, and tmux re-runs it for every new window for as long as the session
// lives. Whoever wrote it must therefore outlive the session. Any process that
// restores a session (af sessions preview and attach do, so does the TUI) runs
// the account-environment refresh, which used to rewrite default-command; with
// capabilities that would hand the session's windows to a process about to exit.
// So only the owner writes default-command. It re-establishes it for every live
// session it restores, which is what makes a daemon replaced by an upgrade pick
// its sessions back up (#4731).
var launchCapabilityOwner atomic.Bool

// SetLaunchCapabilityOwner marks this process as the owner. The daemon calls it
// once at startup; nothing else may.
func SetLaunchCapabilityOwner() { launchCapabilityOwner.Store(true) }

func ownsLaunchCapabilities() bool { return launchCapabilityOwner.Load() }

// nonOwnerDefaultCommand is what a process that does not own capabilities
// installs for a NEW session's default-command. It refuses with a message instead
// of leaving tmux's login shell, which would start an account session's new
// window outside the account boundary. The daemon replaces it on its next restore
// of the session.
const nonOwnerDefaultCommand = `/bin/sh -c 'echo "af: new windows in an account-scoped session are set up by the af daemon; run af daemon restart, then reopen the session" >&2; exit 127'`

// pendingCapability is a capability a launch minted that is not yet installed.
type pendingCapability struct {
	token   string
	perPane bool
}
