package tmux

import "github.com/sachiniyer/agent-factory/internal/sessionenv"

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
