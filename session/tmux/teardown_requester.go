package tmux

import (
	"sync"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// The teardown-requester registry (#5182).
//
// `af sessions archive --self` — and the kill/tab-close/project-delete
// equivalents — run inside the very pane tree they ask the daemon to tear
// down, and stay blocked on the control-socket reply while teardown runs.
// Before this set existed the requester was just another captured pane
// process: the reaper waited the full reapGraceWait on a process that could
// not exit (it was waiting on the answer), then SIGTERMed it — and the
// committed archive's reply died with the client that asked for it, while the
// log called it a leaked process.
//
// Who the requester is comes from the KERNEL and nowhere else: the daemon
// reads the connection peer's pid at accept (SO_PEERCRED/LOCAL_PEERPID via
// internal/peercred), resolves it to a (pid, start-stamp) identity through
// proctree.Lookup, and registers it here for the duration of the handler. No
// request field exists that could claim this exemption, and a recycled pid
// fails the start-stamp match — a different process cannot fake being the
// requester (the issue's property (d)).
//
// The set is process-wide rather than keyed by teardown target on purpose.
// A registered process can only ever be exempted by a reap whose captured set
// actually contains it — i.e. a teardown of a pane it lives in — and for that
// teardown it is definitionally a caller whose reply is still outstanding,
// whichever session the request named. The exemption therefore never reaches
// outside the captured set, and it never needs the daemon to predict which
// tmux session a request resolves to.
//
// Tracking ends when the handler returns — the reply is then in flight and
// the requester is free to exit. A requester that lingers past that (wedged,
// or its pane already gone) is reaped by the next ordinary orphan sweep like
// any other leftover; the exemption only ever covers the window in which the
// process could not have exited because its answer had not been sent.
var teardownRequesters sync.Map // map[teardownRequesterID]struct{}

// teardownRequesterID is the (pid, start-stamp) identity of one registered
// requester — a process instance, not a pid slot (#2103's rule applies here
// exactly as it does at signal time).
type teardownRequesterID struct {
	pid     int
	startID uint64
}

// TrackTeardownRequester registers proc as a process blocked on a teardown
// reply for the duration of the caller's handler, returning the unregister.
// The daemon calls it with the kernel-verified socket peer resolved through
// proctree.Lookup; a zero or unresolvable identity registers nothing, so the
// caller degrades to the pre-#5182 behavior instead of exempting a guess.
func TrackTeardownRequester(proc proctree.Process) func() {
	if proc.PID <= 0 {
		return func() {}
	}
	id := teardownRequesterID{pid: proc.PID, startID: proc.StartID}
	teardownRequesters.Store(id, struct{}{})
	return func() { teardownRequesters.Delete(id) }
}

// isTeardownRequester reports whether p's identity is a process currently
// registered as blocked on a teardown reply.
func isTeardownRequester(p proctree.Process) bool {
	_, ok := teardownRequesters.Load(teardownRequesterID{pid: p.PID, startID: p.StartID})
	return ok
}

// dropTeardownRequesters removes registered requesters from a process set
// bound for the reaper or a teardown-stall wait. An exempted process is never
// waited on and never signalled — it exits when its reply arrives — and the
// caller sees a set that contains only the processes the teardown is actually
// responsible for.
func dropTeardownRequesters(procs []proctree.Process) []proctree.Process {
	var kept []proctree.Process
	var dropped bool
	for _, p := range procs {
		if isTeardownRequester(p) {
			dropped = true
			log.InfoLog.Printf("teardown requester pid %d (%s) is blocked on this teardown's reply; "+
				"excluding it from the process reap (#5182)", p.PID, p.Comm)
			continue
		}
		kept = append(kept, p)
	}
	if !dropped {
		return procs
	}
	return kept
}
