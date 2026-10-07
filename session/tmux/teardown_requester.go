package tmux

import (
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
	"github.com/sachiniyer/agent-factory/log"
)

// The teardown-requester registry itself lives in internal/teardownreq — see
// that package for the rationale (#5182). It cannot live in this package
// because session/git's worktree writer reaper must consult it too, and
// session/git cannot import session/tmux. These thin wrappers keep the tmux
// seam's naming local and add the tmux reap's log line where a requester is
// filtered out.

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

// dropTeardownRequesters removes registered requesters from a process set
// bound for the reaper or a teardown-stall wait, logging each exclusion so
// the absence of the usual SIGTERM line is explained rather than silent.
func dropTeardownRequesters(procs []proctree.Process) []proctree.Process {
	for _, p := range procs {
		if isTeardownRequester(p) {
			log.InfoLog.Printf("teardown requester pid %d (%s) is blocked on this teardown's reply; "+
				"excluding it from the process reap (#5182)", p.PID, p.Comm)
		}
	}
	return teardownreq.Drop(procs)
}
