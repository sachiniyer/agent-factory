// Package teardownreq holds the teardown-requester registry (#5182).
//
// `af sessions archive --self` — and the kill/tab-close/project-delete
// equivalents — run inside the very pane tree they ask the daemon to tear
// down, and stay blocked on the control-socket reply while teardown runs.
// Before this set existed the requester was just another captured pane
// process: the reaper waited the full grace period on a process that could
// not exit (it was waiting on the answer), then SIGTERMed it — and the
// committed archive's reply died with the client that asked for it, while
// the log called it a leaked process.
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
// A registered process can only ever be exempted by a reap whose captured
// set actually contains it — i.e. a teardown of a pane it lives in — and for
// that teardown it is definitionally a caller whose reply is still
// outstanding, whichever session the request named. The exemption therefore
// never reaches outside the captured set, and it never needs the daemon to
// predict which tmux session a request resolves to.
//
// Tracking ends when the LAST handler from a requester returns — the reply
// is then in flight and the requester is free to exit. Registrations are
// counted rather than boolean because one process can have two teardowns in
// flight at once (net/rpc serves a connection's calls concurrently, and a
// second connection carries the same kernel-verified peer): the first
// unregister must not drop the identity the second handler still needs. A
// requester that lingers past that (wedged, or its pane already gone) is
// reaped by the next ordinary sweep like any other leftover; the exemption
// only ever covers the window in which the process could not have exited
// because its answer had not been sent.
//
// The registry lives here rather than in session/tmux because teardown has
// more than one kill channel: the pane-tree reaper in session/tmux and the
// worktree writer reaper in session/git both must consult it, and
// session/git cannot import session/tmux.
package teardownreq

import (
	"sync"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

var (
	trackedMu sync.Mutex
	// tracked counts live registrations per requester identity: two
	// overlapping handlers from the same process share one key, and only the
	// last unregister removes it.
	tracked = make(map[requesterID]int)
)

// requesterID is the (pid, start-stamp) identity of one registered requester
// — a process instance, not a pid slot (#2103's rule applies here exactly as
// it does at signal time).
type requesterID struct {
	pid     int
	startID uint64
}

// Track registers proc as a process blocked on a teardown reply for the
// duration of the caller's handler, returning the unregister. The daemon
// calls it with the kernel-verified socket peer resolved through
// proctree.Lookup; a zero or unresolvable identity registers nothing, so the
// caller degrades to the pre-#5182 behavior instead of exempting a guess.
func Track(proc proctree.Process) func() {
	if proc.PID <= 0 {
		return func() {}
	}
	id := requesterID{pid: proc.PID, startID: proc.StartID}
	trackedMu.Lock()
	tracked[id]++
	trackedMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			trackedMu.Lock()
			defer trackedMu.Unlock()
			if tracked[id] > 1 {
				tracked[id]--
			} else {
				delete(tracked, id)
			}
		})
	}
}

// Is reports whether p's identity is a process currently registered as
// blocked on a teardown reply.
func Is(p proctree.Process) bool {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	return tracked[requesterID{pid: p.PID, startID: p.StartID}] > 0
}

// SignalUnlessTracked sends sig to p only if p is not a tracked requester —
// the check and the signal happen under the registry lock, so a Track that
// lands between a reaper's exemption check and its kill can no longer slip a
// now-registered process into the signal set (Codex on #5186). Returns
// (attempted, err): attempted false means p was exempt under the lock.
func SignalUnlessTracked(p proctree.Process, sig syscall.Signal) (bool, error) {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	if tracked[requesterID{pid: p.PID, startID: p.StartID}] > 0 {
		return false, nil
	}
	return true, proctree.Signal(p, sig)
}

// Drop returns procs without registered requesters. An exempted process is
// never waited on and never signalled — it exits when its reply arrives —
// and the caller sees a set containing only the processes the teardown is
// actually responsible for.
func Drop(procs []proctree.Process) []proctree.Process {
	var kept []proctree.Process
	var dropped bool
	for _, p := range procs {
		if Is(p) {
			dropped = true
			continue
		}
		kept = append(kept, p)
	}
	if !dropped {
		return procs
	}
	return kept
}
