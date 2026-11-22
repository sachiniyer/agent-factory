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
// A registration carries the teardown it is waiting on — the set of tmux
// session names and worktree paths that teardown will own (#7754). The
// exemption is scoped to THAT teardown: a client blocked on archiving
// session B while it sits in session A's pane is a caller of B's teardown,
// not A's, and A's reaper must be free to count it among A's survivors
// rather than excusing a process that owes it nothing. Reapers ask with the
// name or path they are tearing down, so the match is exact and needs no
// daemon prediction about which session a request resolves to.
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
	// tracked counts live registrations per requester identity and target:
	// two overlapping handlers from the same process share one identity key,
	// and each registration's own target set keeps its own count — a handler
	// unregistering must not drop a target another handler still needs.
	tracked = make(map[requesterID]map[Target]int)
)

// requesterID is the (pid, start-stamp) identity of one registered requester
// — a process instance, not a pid slot (#2103's rule applies here exactly as
// it does at signal time).
type requesterID struct {
	pid     int
	startID uint64
}

// Target names ONE teardown a requester may be excused from (#7754). The
// reaper answers "is this process the caller waiting on MY reply", so the
// key is what the reaper already knows about the teardown it is running: a
// tmux session name for the pane-tree reaper, a worktree path for the
// writer reaper. The kind tag in the key keeps a tmux name that happens to
// spell a path from matching a worktree target.
type Target struct {
	key string
}

// TmuxTarget is the teardown of one tmux session — the sanitized name every
// pane-tree reaper and orphan sweep already addresses it by.
func TmuxTarget(sanitizedName string) Target {
	return Target{key: "tmux\x00" + sanitizedName}
}

// WorktreeTarget is the teardown of one worktree — the path the writer
// reaper is asked to empty, exactly as the session record carries it (the
// reaper's own normalization is a cwd-matching detail, not part of the key).
func WorktreeTarget(worktreePath string) Target {
	return Target{key: "worktree\x00" + worktreePath}
}

// Track registers proc as a process blocked on the reply of EACH of targets'
// teardowns for the duration of the caller's handler, returning the
// unregister. A session teardown owns several targets at once — its tmux
// sessions AND its worktree — so the handler registers them all in one
// registration; a reaper consulting any one of them spares the caller.
//
// The daemon calls it with the kernel-verified socket peer resolved through
// proctree.Lookup; a zero or unresolvable identity registers nothing, as
// does an empty target set, so the caller degrades to the pre-#5182 behavior
// instead of exempting a guess.
func Track(proc proctree.Process, targets ...Target) func() {
	if proc.PID <= 0 || len(targets) == 0 {
		return func() {}
	}
	id := requesterID{pid: proc.PID, startID: proc.StartID}
	trackedMu.Lock()
	set := tracked[id]
	if set == nil {
		set = make(map[Target]int, len(targets))
		tracked[id] = set
	}
	for _, target := range targets {
		set[target]++
	}
	trackedMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			trackedMu.Lock()
			defer trackedMu.Unlock()
			set := tracked[id]
			for _, target := range targets {
				if set[target] > 1 {
					set[target]--
				} else {
					delete(set, target)
				}
			}
			if len(set) == 0 {
				delete(tracked, id)
			}
		})
	}
}

// Is reports whether p's identity is a process currently registered as
// blocked on target's teardown reply.
func Is(p proctree.Process, target Target) bool {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	return tracked[requesterID{pid: p.PID, startID: p.StartID}][target] > 0
}

// SignalUnlessTracked sends sig to p only if p is not a requester tracked
// for target's teardown — the check and the signal happen under the registry
// lock, so a Track that lands between a reaper's exemption check and its
// kill can no longer slip a now-registered process into the signal set
// (Codex on #5186). Returns (attempted, err): attempted false means p was
// exempt under the lock.
func SignalUnlessTracked(p proctree.Process, target Target, sig syscall.Signal) (bool, error) {
	return SignalUnlessTrackedOrSpared(p, target, sig, nil)
}

// SignalUnlessTrackedOrSpared is SignalUnlessTracked with one more check
// inside the same critical section: spare, when it is not nil, is evaluated
// after the registry check and immediately before the signal, and a true
// answer spares p (attempted false). Nothing else runs between spare and the
// signal except proctree.Signal's own identity check, so a caller whose
// reprieve depends on what p is right now (the worktree reaper re-reading
// tmux identity, #7244) has no lock wait between that read and the kill.
// spare is not consulted for a tracked requester.
//
// spare runs while the registry lock is held, so it must be short and must
// not call back into this package.
func SignalUnlessTrackedOrSpared(p proctree.Process, target Target, sig syscall.Signal, spare func(proctree.Process) bool) (bool, error) {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	if tracked[requesterID{pid: p.PID, startID: p.StartID}][target] > 0 {
		return false, nil
	}
	if spare != nil && spare(p) {
		return false, nil
	}
	return true, proctree.Signal(p, sig)
}
