package tmux

import (
	"errors"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// socketSweep is the answer to "who could own this absent socket path"
// (#7695): which processes are tied to the path by bound-socket evidence,
// and for whom the negative — "no same-uid process holds a socket bound to
// it" — cannot be proved. The evidence is the kernel's own socket tables on
// Linux (internal/proctree.UnixSocketOwnerSweep, socket_owner_linux.go),
// and on other platforms the #7689 candidacy reads of sweepCandidatePIDs
// (socket_owner_other.go). Either way a pid appears only through a positive
// tie to the path or as a read that failed: what it looks like — its task
// title, its executable — is never consulted, and a failed read is unknown,
// never an absence.
type socketSweep struct {
	// bound maps a pid to the bound socket of its that is certainly the
	// absent path — an owner, signallable once positively identified as a
	// tmux server.
	bound map[int]boundOwner
	// uncertain maps a pid to a bound spelling whose match to the absent
	// path stayed undecidable (a deleted component may have been an alias,
	// #7562; a bound spelling that will not canonicalize). A possible owner;
	// never signalled.
	uncertain map[int]string
	// unattributedBound counts certain bound rows whose holder no same-uid
	// pid could be named for: an other-uid, non-dumpable, out-of-pidns,
	// SCM_RIGHTS-buffered, or raced-away holder. A holder the kernel says
	// exists but af cannot name is a possible owner by itself.
	unattributedBound int
	// unattributedUncertain is unattributedBound for undecidable rows.
	unattributedUncertain int
	// uncovered lists the live pids the negative cannot be proved for —
	// their socket state could not be read completely — or, on the candidacy
	// fallback, candidates admitted that are not positively tmux servers or
	// whose binding stayed unresolved.
	uncovered []int
	// unprobed is true when the sweep itself could not run — the process
	// table or the socket table could not be read at all.
	unprobed bool
}

// boundOwner is one pid tied to the absent path by a bound socket: the
// kernel-kept spelling that matched and the evidence that tied it, named in
// the SIGUSR1 line (#7541).
type boundOwner struct {
	spelling string
	source   string
}

// socketOwnerSweep is the per-platform census the claim verdict consumes:
// the kernel-socket-table sweep on Linux, the #7689 candidacy fallback
// everywhere else. A package var so PinServerProbeForTest can pin the world
// a test asks about.
var socketOwnerSweep = probeSocketOwners

// tmuxServerProcessPIDs lists the candidacy fallback's candidates: the
// #7689 answer to "could a tmux server be alive for this uid?", by task
// name. It is the candidacy's own coarse prefilter — binding evidence still
// decides each one — kept for the platforms without a socket-table sweep
// and for the tests that pin it. A package var for the same reason it
// always was: the real probe reads the host's process table, so a developer
// box with tmux running would otherwise give a different result from a
// container that has none.
var tmuxServerProcessPIDs = liveTmuxServerPIDs

// tmuxSocketClaimedForCandidates is the #7689 claim verdict over an
// explicit candidate list: each pid is gated on a positive server identity,
// asked its binding, and only a server bound to the path is signalled. It
// is the fallback sweep's verdict path on platforms without a kernel-table
// census, and the decision seam the tests pin — the production verdict is
// socketClaimedByProbe, which on Linux starts from the kernel's tables
// instead of a candidate list.
func tmuxSocketClaimedForCandidates(socketPath string, pids []int) bool {
	return socketClaimedBySweep(socketPath, sweepCandidatePIDs(socketPath, pids))
}

// socketClaimedByProbe is the production claim verdict for an absent socket
// path: whichever census the platform offers — the kernel's socket tables
// on Linux, the candidacy read elsewhere — through the shared
// signal-and-watch tail.
func socketClaimedByProbe(socketPath string) bool {
	return socketClaimedBySweep(socketPath, socketOwnerSweep(socketPath))
}

// sweepCandidatePIDs is the #7689 ownership question asked of an explicit
// pid set — the fallback sweep for platforms without a socket-table census,
// and the world PinServerProbeForTest pins for a test. It reproduces the
// candidacy flow exactly: the 0 sentinel for an unreadable table, the
// identity gate before the binding read, a certain mismatch or a proved
// exit ruling a candidate out, and anything unresolved left as a possible
// owner.
func sweepCandidatePIDs(socketPath string, pids []int) socketSweep {
	s := socketSweep{bound: map[int]boundOwner{}, uncertain: map[int]string{}}
	for _, pid := range pids {
		if pid <= 0 {
			// The process table could not be read: an owner was not disproved.
			s.unprobed = true
			continue
		}
		if !tmuxServerIdentified(pid) {
			// The pid passed the comm prefilter but could not be positively
			// identified as a server (proctree.IsTmuxServer is strict: an
			// unreadable command line, a darwin foreground `tmux -D`, or a
			// same-uid client all fail). Never signalled — SIGUSR1's default
			// disposition is TERMINATE — and never disproved: it occupies a
			// slot the prefilter admitted, so socket absence is not evidence
			// against it.
			s.uncovered = append(s.uncovered, pid)
			continue
		}
		switch binding, boundTo := tmuxServerSocketBinding(pid, socketPath); binding {
		case socketBindingElsewhere:
			log.InfoLog.Printf("tmux socket %s is missing: tmux server pid %d is bound to %s instead, per its own "+
				"sockets read one by one, so it is ruled out and not signalled (asked by %s)", socketPath, pid, boundTo, socketProbeCaller())
		case socketBindingGone:
			log.InfoLog.Printf("tmux socket %s is missing: tmux server pid %d exited while its sockets were "+
				"read, so it is ruled out and not signalled (asked by %s)", socketPath, pid, socketProbeCaller())
		case socketBindingUnresolved:
			s.uncovered = append(s.uncovered, pid)
		case socketBindingBoundByTable:
			s.bound[pid] = boundOwner{spelling: boundTo,
				source: "the /proc/<pid>/net/unix table, since its own socket could not be read"}
		default: // socketBindingBound
			s.bound[pid] = boundOwner{spelling: boundTo,
				source: "its own socket, read by sock_diag/proc_pidfdinfo"}
		}
	}
	return s
}

// PinServerProbeForTest fixes the "is a tmux server alive for this uid?"
// answer and returns the restore. It exists for internal/testguard.IsolateTmux,
// which declares a private-socket world for a test; without it that isolation
// is incomplete, because the probe reads the HOST's kernel evidence and would
// see the developer's own tmux servers inside a world that is supposed to
// have none.
//
// The pinned pids mean what they always have: the live same-uid candidates
// the probe may consider. On every platform they go through the candidacy
// evidence — identity, then binding — so a real server pid is evaluated by
// its real binding (bound to the path, or ruled out on it), while a pid
// that is not positively a server, or a fabricated one, stays a possible
// owner. An empty pin is a world with no candidates: a determinate empty
// for a path nothing is bound to. A 0 pin is the unreadable-table sentinel.
//
// Test-only by contract, not by build tag: testguard lives in internal/ and
// is the only caller.
func PinServerProbeForTest(pids ...int) (restore func()) {
	prev := socketOwnerSweep
	socketOwnerSweep = func(socketPath string) socketSweep {
		return sweepCandidatePIDs(socketPath, pids)
	}
	return func() { socketOwnerSweep = prev }
}

// socketClaimedBySweep reports whether a live tmux server claims the socket
// at socketPath, given the sweep's census of who could own it. It is the
// active half of the socket-absent check: ENOENT alone cannot distinguish
// "no server ever created this socket" from "a server lost its socket to a
// /tmp cleaner and keeps running orphaned" (#2875).
//
// The kernel can tell them apart without asking what any process looks like
// (#7695). A socket bound to a filesystem path is a row in the socket table
// of the network namespace it was bound in — spelling and inode surviving
// the file's unlink — so the sweep finds the owners by who is BOUND to this
// path. Only an owner positively identified as a tmux server is sent
// SIGUSR1, tmux's documented recreate-socket signal, while the path is
// watched (#7533). A bound pid that is not positively a server is never
// signalled — SIGUSR1's default disposition is TERMINATE, which kills a
// tmux CLIENT before it installs its handler (#4678) and a server's child
// before it execs (#6581) — and it is a possible owner, as is every
// undecidable binding, every uncovered process, every unattributed bound
// holder, and every owner whose signal was refused.
//
// Each SIGUSR1 is logged at INFO with its target, the path the target is
// bound to, the evidence that tied it, and the code that asked, and the
// sweep itself is summarized once (#7541). An "unclaimed" answer resting on
// that evidence can then be traced the way a signal can.
//
// An owner that recreates the path claims it. Otherwise the answer is
// "unclaimed", a determinate empty, only when no possible owner remains:
// every holder was signalled and left the path absent (a server that cannot
// or does not recreate its socket is unreachable to every tmux client
// anyway), and nothing else tied to the path stayed unprobed. With nothing
// signalled the answer comes at once, with no watch.
func socketClaimedBySweep(socketPath string, s socketSweep) bool {
	logSocketSweep(socketPath, s)
	signaled := false
	unprobedOwnerPossible := s.unprobed || len(s.uncovered) > 0 || len(s.uncertain) > 0 ||
		s.unattributedBound > 0 || s.unattributedUncertain > 0
	for _, pid := range sortedSweepOwners(s.bound) {
		owner := s.bound[pid]
		if !tmuxServerIdentified(pid) {
			// Bound to the path but not positively a tmux server — a renamed
			// server whose identity will not resolve, or another daemon that
			// rebound the freed path. SIGUSR1 is never risked on it, and it
			// is never disproved.
			unprobedOwnerPossible = true
			continue
		}
		kerr := tmuxServerSignal(pid, syscall.SIGUSR1)
		outcome := "sent"
		if kerr != nil {
			outcome = "not delivered: " + kerr.Error()
		}
		log.InfoLog.Printf("tmux socket %s is missing: SIGUSR1 to tmux server pid %d, which is bound to %s per %s, "+
			"so it recreates the socket (%s; asked by %s)", socketPath, pid, owner.spelling, owner.source, outcome, socketProbeCaller())
		switch {
		case kerr == nil:
			signaled = true
		case errors.Is(kerr, syscall.ESRCH):
			// The pid vanished between the positive identity check and the
			// signal. Gone is gone: this owner IS disproved, so it is
			// neither counted as signalled nor remembered as a possible owner.
		default:
			// A positively-identified live owner we could not signal — e.g.
			// EPERM or EACCES from a sandbox/LSM signal policy. A live owner
			// that kill() refuses is NOT disproved, and a socket that stays
			// absent is NOT evidence against it: the safe direction is
			// "claimed" (unknown).
			unprobedOwnerPossible = true
		}
	}
	if !signaled {
		if unprobedOwnerPossible {
			return true
		}
		// Nothing remains that could own it. A server that bound the path
		// since the client's read would show as the file itself.
		_, err := os.Stat(socketPath)
		return err == nil
	}
	deadline := time.Now().Add(socketReclaimWindow)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			// A possible owner was never disproved — a bound pid that is
			// not positively a server, an undecidable binding, an uncovered
			// process, an unattributed holder. Socket absence after
			// signalling the *other* owners does not disprove it, so the
			// safe direction is "claimed" (unknown), refusing the
			// destructive cleanup a false "unclaimed" would license.
			return unprobedOwnerPossible
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// sortedSweepOwners orders a sweep bucket's pids so signals and logs are
// deterministic.
func sortedSweepOwners(m map[int]boundOwner) []int {
	pids := make([]int, 0, len(m))
	for pid := range m {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// logSocketSweep summarizes one sweep for the INFO log, so a determinate
// "unclaimed" and a refusal both carry the evidence they rested on — the
// same traceability #7541 demanded of the signal lines.
func logSocketSweep(socketPath string, s socketSweep) {
	log.InfoLog.Printf("tmux socket %s is missing: socket-owner sweep found bound=%v uncertain=%v uncovered=%v "+
		"unattributed=%d/%d unprobed=%v (asked by %s)", socketPath, sortedSweepOwners(s.bound), sortedKeys(s.uncertain),
		s.uncovered, s.unattributedBound, s.unattributedUncertain, s.unprobed, socketProbeCaller())
}

func sortedKeys(m map[int]string) []int {
	pids := make([]int, 0, len(m))
	for pid := range m {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// possibleSocketOwners is the socket-absent refusal's list of who could
// still own socketPath, read the way the claim verdict reads it but with
// nothing sent. servers are possible owners positively identified as tmux
// servers; unidentified are possible owners that are not — the 0 sentinel
// for a sweep that could not run among them; unattributed is the count of
// bound rows whose holder no process could be named for (#7695). A pid
// ruled out by evidence, or proved gone, is left out.
func possibleSocketOwners(socketPath string) (servers, unidentified []int, unattributed int) {
	return possibleSocketOwnersBySweep(socketOwnerSweep(socketPath))
}

// possibleSocketOwnersForCandidates is the candidacy version of
// possibleSocketOwners over an explicit pid set — the #7689 fallback's
// answer and the seam the decision-table tests pin.
func possibleSocketOwnersForCandidates(socketPath string, pids []int) (servers, unidentified []int) {
	servers, unidentified, _ = possibleSocketOwnersBySweep(sweepCandidatePIDs(socketPath, pids))
	return servers, unidentified
}

func possibleSocketOwnersBySweep(s socketSweep) (servers, unidentified []int, unattributed int) {
	if s.unprobed {
		unidentified = append(unidentified, 0)
	}
	candidates := append(append(sortedSweepOwners(s.bound), sortedKeys(s.uncertain)...), s.uncovered...)
	sort.Ints(candidates)
	for _, pid := range candidates {
		if !tmuxServerIdentified(pid) {
			unidentified = append(unidentified, pid)
			continue
		}
		servers = append(servers, pid)
	}
	return servers, unidentified, s.unattributedBound + s.unattributedUncertain
}

// liveTmuxServerPIDs lists the tmux servers owned by this uid: the
// candidacy fallback's candidates for owning an unlinked socket.
//
// It answers a deliberately BROAD question — "could any live server own an
// unlinked socket?" — by task name alone, and only on platforms without a
// kernel-socket-table sweep, where naming candidates is the best available
// start. Which candidate is bound to THIS socket is the binding question,
// answered from each server's own listening sockets (#7533). The asymmetry
// is chosen: a false "yes" here costs that per-server read; a false "no"
// lets reset delete worktrees out from under a live agent.
//
// A process table we cannot read reports TRUE, for the same reason: an
// unreadable table is not evidence of absence.
func liveTmuxServerPIDs() []int {
	snap, err := proctree.Snapshot()
	if err != nil {
		// Not evidence of absence. A sentinel PID would be a lie, so report an
		// unnamed one: callers only test emptiness, and the message degrades to
		// "a server is running" without claiming to know which.
		return []int{0}
	}
	uid := os.Getuid()
	var pids []int
	for pid, p := range snap {
		if !isTmuxServerComm(p.Comm) {
			continue
		}
		if owner, ok := proctree.UID(pid); ok && owner != uid {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}
