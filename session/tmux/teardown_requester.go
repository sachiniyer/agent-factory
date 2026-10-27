package tmux

import (
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
)

// The teardown-requester registry itself lives in internal/teardownreq — see
// that package for the rationale (#5182). It cannot live in this package
// because session/git's worktree writer reaper must consult it too, and
// session/git cannot import session/tmux. These thin wrappers keep the tmux
// seam's naming local.

// TrackTeardownRequester registers proc as a process blocked on a teardown
// reply for the duration of the caller's handler, returning the unregister.
func TrackTeardownRequester(proc proctree.Process) func() {
	return teardownreq.Track(proc)
}

// isTeardownRequester reports whether p's identity is a process currently
// registered as blocked on a teardown reply.
func isTeardownRequester(p proctree.Process) bool {
	return teardownreq.Is(p)
}
