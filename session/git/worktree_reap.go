// The worktree writer-reaper layer: the best-effort pass that kills every live
// process still working inside a worktree before the tree is deleted, plus the
// shared snapshot-selection machinery the pathname matcher and the repo-gone
// cleanup's descriptor-identity matcher both apply. Nothing here knows about
// GitWorktree — only paths, PIDs, and the protected-infrastructure exclusions.
package git

import (
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
	"github.com/sachiniyer/agent-factory/log"
)

var (
	// worktreeReapGrace is how long a writer discovered inside the worktree gets
	// to exit on its own before it is SIGTERMed. Zero: by the time a worktree is
	// being deleted the pane teardown has already SIGHUP'd the pane's process
	// group and waited for the pane to exit (#802), so a process still writing
	// here has already had its grace and there is no reason to wait again — go
	// straight to the escalation. var, not const, so a test can tune the pacing.
	worktreeReapGrace = 0 * time.Second
	// worktreeReapTermWait is how long a SIGTERMed writer gets before SIGKILL.
	// WaitForExits returns as soon as everything is gone, so a well-behaved writer
	// costs a poll interval, not the whole wait.
	worktreeReapTermWait = 2 * time.Second
)

// reapWorktreeWriters kills every live process still working inside worktreePath
// BEFORE the tree is deleted (#2025): any process whose current working directory
// is at or under the tree, plus that process's whole descendant subtree.
//
// The leak this closes: `git worktree remove -f` and the os.RemoveAll fallback
// both delete recursively and do NOT fail on a merely-non-empty directory — they
// fail "directory not empty" only when files are being CREATED into the tree
// faster than they can be unlinked, i.e. a live process is still writing to it.
// A session whose agent backgrounded a long-lived writer (an installer, a dev
// server, a package manager) can leave that writer alive after the kill tore down
// the agent/tmux — the tmux reaper (#1104) escalates asynchronously and does not
// block this removal — and it then races, and beats, the delete, orphaning the
// worktree (the worktree_ops.go / teardown.go "directory not empty" pair).
// Killing the writers first removes the racer so the delete can finish.
//
// WHICH processes are killed is deliberately narrow (the #1104 "only our own
// descendants" discipline, and the "which children are garbage" hazard): an AF
// worktree directory is a session-private path, so a process cwd'd inside it is
// unambiguously this session's and no unrelated process is ever signalled. The
// kill itself routes through the existing #1104 reaper (proctree.KillEscalating):
// every signal is identity-verified against (pid, start-time) so a recycled PID
// is never hit, and the SIGTERM→SIGKILL escalation is shared, not re-implemented.
//
// Best-effort, like every reaper on this path: an unreadable process table (no
// /proc, an unsupported platform) degrades to a no-op — nothing is reaped, and
// the existing WARNING plus doctor's stale-worktree path own whatever survives.
// It never errors and never loops: a writer that ignores SIGKILL (a D-state,
// uninterruptible I/O, a mount) is left for the removal to fail loudly on, not
// spun on forever.
//
// Linux and darwin both back proctree.WorkingDir (/proc/<pid>/cwd and
// proc_info(PROC_PIDVNODEPATHINFO) respectively), so the reap is live on both
// as of #2050. Elsewhere, and for any process whose cwd the kernel will not
// disclose, WorkingDir reports the honest unknown and that process is simply not
// matched — the safe degradation, since it can only fail to signal the right
// process, never signal the wrong one.
//
// The leave-alone identity degrades the same way (#7238). A process whose
// command line cannot be read (one mid-execve or mid-exit, or one darwin
// refuses) might be the shared tmux server, so it is excluded like tmux, never
// read as "not tmux". The reaper signals only what it has fully classified,
// and that holds when it signals, not only when it selects: the identity is
// read again just before every signal tier (#7244, tierIdentity).
func reapWorktreeWriters(worktreePath string) {
	// The path exists (callers reap only after an os.Stat succeeds), so resolving
	// symlinks here matches /proc/<pid>/cwd, which the kernel already resolves.
	root := normalizeWorktreePath(worktreePath)
	reapWorktreeWritersMatching(worktreePath, func(pid int) bool {
		cwd, ok := proctree.WorkingDir(pid)
		return ok && pathutil.IsAtOrInside(filepath.Clean(cwd), root)
	})
}

func reapWorktreeWritersMatching(worktreePath string, matches func(int) bool) {
	snap, err := proctree.Snapshot()
	if err != nil {
		// Could not READ the process table — never the same fact as "no writers"
		// (proctree's whole design). Skip the reap and let the removal proceed; a
		// writer that really is alive surfaces as the existing "directory not empty"
		// WARNING for doctor to reconcile, never a silently-swept process table.
		return
	}
	procs, unclassified := worktreeWriterProcessesMatching(snap, os.Getpid(), matches, proctree.TmuxIdentityOf)
	// The identity read above is re-established before every signal (#7244).
	tiers := newTierIdentity(proctree.TmuxIdentityOf, proctree.AliveSame)
	// Runs after the kill below, or now if there is nothing to kill. It reads
	// tiers only when it runs, so it also covers what a signal tier left alone.
	defer func() {
		warnUnclassifiedSurvivors(worktreePath, append(sparedAtSelection(unclassified), tiers.unclassified...), proctree.TmuxIdentityOf)
	}()
	if len(procs) == 0 {
		return
	}
	// A teardown requester can register AFTER the set above was built — a
	// teardown still in its grace wait when the requester's own destructive
	// RPC reaches its handler. Re-check the registry before each signal tier
	// so a late-tracked requester is spared too (Codex on #5186). Requesters
	// already tracked at selection were logged by the selector; seed them here
	// so the mid-reap notice fires only for genuine late registrations.
	exemptedMidReap := make(map[int]bool)
	for _, p := range procs {
		if teardownreq.Is(p) {
			exemptedMidReap[p.PID] = true
		}
	}
	proctree.KillEscalatingExcept(procs, func(p proctree.Process) bool {
		if tiers.leftAlone(p) {
			return true
		}
		if !teardownreq.Is(p) {
			return false
		}
		if !exemptedMidReap[p.PID] {
			exemptedMidReap[p.PID] = true
			log.InfoLog.Printf("teardown requester pid %d (%s) registered while this reap was already waiting; "+
				"excluding it from the worktree writer reap (#5182)", p.PID, p.Comm)
		}
		return true
	}, tiers.signalIf(teardownreq.SignalUnlessTrackedOrSpared), worktreeReapGrace, worktreeReapTermWait, func(_ proctree.ReapOutcome, format string, args ...any) {
		// Every tier stays a WARNING here, and the outcome is deliberately unused
		// (#2765). This reaper does not run on the requested-teardown side of that
		// split: it fires only when a process is STILL WRITING into a worktree
		// after the agent and its tmux session were already torn down, which is the
		// definition of having escaped its pane tree. There is no routine case.
		//
		// worktreePath is a runtime value that may legally contain `%`, so it MUST
		// be a `%s` ARGUMENT, never spliced into the format string (the #1211 rule
		// the tmux reaper follows). `format` is KillEscalating's own constant literal.
		log.WarningLog.Printf("worktree %s: leaked past its session: "+format, append([]any{worktreePath}, args...)...)
	})
}

// warnUnclassifiedSurvivors reports, at WARNING, each process the reap left
// alone only because its tmux identity could not be established, if it is still
// alive once the reap is done (#7238, #7242 review R6). Leaving it alone is
// right, but it is not loud on its own. A removal fails only if the process
// out-creates the unlink. On relocate (archive) the rename succeeds and the
// process keeps writing into the moved tree. So this is where an operator hears
// about it.
//
// It first gives those processes the same wait a SIGTERMed writer gets. An
// unknown identity is usually a process starting or exiting (see
// proctree.ErrNoArgv), and an ordinary exit is not worth a warning. Then it reads
// each survivor's identity again, and says what that read found (#7245 review
// R3):
//
//   - tmux: it was caught mid-exec into tmux, and leaving it alone was right, so
//     nothing is reported.
//   - not tmux: it was mid-transition when the reap read it, and it is an
//     ordinary process now. The warning says so, and that a retried removal, or
//     deleting an archived session, reaps it.
//   - still unknown: the warning says its identity could not be established.
//
// Nothing is signalled here.
func warnUnclassifiedSurvivors(worktreePath string, spared []sparedProcess, identity func(int) proctree.TmuxIdentity) {
	if len(spared) == 0 {
		return
	}
	procs := make([]proctree.Process, 0, len(spared))
	byPID := make(map[int]sparedProcess, len(spared))
	for _, s := range spared {
		procs = append(procs, s.Process)
		byPID[s.PID] = s
	}
	for _, p := range proctree.WaitForExits(procs, worktreeReapTermWait) {
		s := byPID[p.PID]
		// worktreePath is a runtime value that may contain `%`, so it is an
		// argument, never part of either format (#1211).
		switch identity(p.PID) {
		case proctree.Tmux:
			continue
		case proctree.NotTmux:
			log.WarningLog.Printf("worktree %s: pid %d (%s) is still working inside it and %s. When the reap read it "+
				"%s, whether it is tmux could not be read, which happens while a process starts, execs or exits, "+
				"and it reads as not tmux now. If the removal failed with \"directory not empty\", retrying it will "+
				"reap this process; after an archive, deleting the archived session does. Inspect it with "+
				"`ps -o pid,stat,lstart,args -p %d`",
				worktreePath, p.PID, p.Comm, s.signalled(), s.readWhen(), p.PID)
		default:
			log.WarningLog.Printf("worktree %s: pid %d (%s) is still working inside it and %s, "+
				"because whether it is the shared tmux server could not be established (#7238). It and its "+
				"subtree may keep writing: a removal can fail with \"directory not empty\", and an archive leaves "+
				"it writing into the moved tree. Inspect it with `ps -o pid,stat,lstart,args -p %d`",
				worktreePath, p.PID, p.Comm, s.signalled(), p.PID)
		}
	}
}

// sparedProcess is a process the reap left alone because its tmux identity was
// unknown, for warnUnclassifiedSurvivors.
type sparedProcess struct {
	proctree.Process
	// afterSIGTERM: the reap had already sent it SIGTERM, and it was left out
	// of the SIGKILL tier only.
	afterSIGTERM bool
	// readAt is the signal whose identity read spared it, or zero when
	// selection did.
	readAt syscall.Signal
}

// signalled says what the reap had sent the process.
func (s sparedProcess) signalled() string {
	if s.afterSIGTERM {
		return "was sent SIGTERM but not SIGKILL"
	}
	return "was not signalled"
}

// readWhen names the read that spared the process.
func (s sparedProcess) readWhen() string {
	if s.readAt == 0 {
		return "at selection"
	}
	return "just before its " + signalName(s.readAt)
}

// sparedAtSelection wraps the processes selection left out for an unknown
// identity. Selection settles each pid's identity once per pass, so none of
// them is also in the kill set, and none was signalled.
func sparedAtSelection(procs []proctree.Process) []sparedProcess {
	spared := make([]sparedProcess, 0, len(procs))
	for _, p := range procs {
		spared = append(spared, sparedProcess{Process: p})
	}
	return spared
}

// tierIdentity re-establishes the leave-alone identity immediately before each
// signal the reap sends (#7244).
//
// Selection reads proctree.TmuxIdentityOf once, and the signal tiers come
// later: SIGTERM after the grace, then SIGKILL after worktreeReapTermWait.
// Neither the kill set nor AliveSame notices an exec in between, because exec
// keeps the pid and the start time. That is #4678's shape: a daemon child
// selected in its pre-exec window, still carrying af's argv, execs tmux and
// becomes a client, or the server's fork parent, and the next tier signals it.
//
// So "signal only what has been fully classified" holds when the reap signals,
// not just when it selects. Before each signal the identity is read again, and
// only a positive NotTmux lets the signal through. A process that reads as
// tmux, or as unknown, is left out of that tier and every later one. The
// decision sticks: a process caught mid-exec, which reads as unknown, is still
// not signalled if a later read finds something else. As at selection, that
// costs at most one missed writer, and an unknown one that survives the reap is
// reported (warnUnclassifiedSurvivors).
//
// No liveness read overrules that (#7245 review R1). On Linux, a multi-threaded
// process that execs from a thread other than its leader reads as a zombie for
// about 100 µs while de_thread hands the pid over, and then carries on under the
// same pid and start time. So "gone" is not proof, and a process that read as
// tmux or unknown is never handed to the signal because a liveness read said it
// had exited. Liveness only picks the log line. One that really has exited
// costs that INFO line, and the survivor warning's own wait drops it.
//
// The read is the spare predicate of teardownreq.SignalUnlessTrackedOrSpared,
// so it runs inside the requester lock, after the registry check, with only
// proctree.Signal's identity check between it and kill(2) (#7245 review R2).
// What remains is that one stat read: an exec that starts after the identity
// read can still be signalled, and nothing in Linux or darwin refuses a signal
// across an exec. The lock is held over the read. On darwin that read can wait
// out its target's own exec, the same blocking class as the stat read
// proctree.Signal already makes under that lock.
//
// The read sits in the check-and-signal rather than in the exemption, because
// KillEscalatingExcept consults the exemption for its wait sets and its final
// leftovers too. A process exiting after its SIGKILL can read as unknown there
// (see proctree.ErrNoArgv), and it would be reported as spared when it was
// killed. The exemption only consults the decision the check-and-signal
// recorded.
//
// The subtree needs no fresh walk. The kill set is a snapshot, so a process
// forked after selection is never in it, and that includes the panes a tmux
// forks after its exec. Each selected descendant is re-read before its own
// signal. A child that existed before its parent became tmux belongs to the
// parent's old image, so it was admitted by the same descent rule as before and
// is still a writer.
type tierIdentity struct {
	identity func(int) proctree.TmuxIdentity
	alive    func(proctree.Process) bool
	// left holds each pid a tier left alone; it is never signalled again.
	left map[int]bool
	// termed holds each pid that was sent SIGTERM.
	termed map[int]bool
	// unclassified is each process left alone because it read as unknown.
	unclassified []sparedProcess
}

func newTierIdentity(identity func(int) proctree.TmuxIdentity, alive func(proctree.Process) bool) *tierIdentity {
	return &tierIdentity{identity: identity, alive: alive, left: make(map[int]bool), termed: make(map[int]bool)}
}

// leftAlone reports whether a signal tier has already left p alone. It reads
// nothing about the process.
func (t *tierIdentity) leftAlone(p proctree.Process) bool {
	return t.left[p.PID]
}

// signalIf adapts next, a check-and-signal that takes a spare predicate
// (teardownreq.SignalUnlessTrackedOrSpared), to the signalIf KillEscalatingExcept
// takes. next evaluates the identity read as that predicate, immediately before
// the signal. signalIf reports attempted false for a process left alone, which
// KillEscalatingExcept treats like any other reprieve.
func (t *tierIdentity) signalIf(next func(proctree.Process, syscall.Signal, func(proctree.Process) bool) (bool, error)) func(proctree.Process, syscall.Signal) (bool, error) {
	return func(p proctree.Process, sig syscall.Signal) (bool, error) {
		if t.left[p.PID] {
			return false, nil
		}
		identity := proctree.NotTmux
		attempted, err := next(p, sig, func(p proctree.Process) bool {
			identity = t.identity(p.PID)
			return identity != proctree.NotTmux
		})
		if identity != proctree.NotTmux {
			// Recorded and logged here, once next has released its lock.
			t.leave(p, sig, identity)
			return false, nil
		}
		if attempted && err == nil && sig == syscall.SIGTERM {
			t.termed[p.PID] = true
		}
		return attempted, err
	}
}

// leave records that the read before sig left p alone, for this tier and every
// later one.
func (t *tierIdentity) leave(p proctree.Process, sig syscall.Signal, identity proctree.TmuxIdentity) {
	t.left[p.PID] = true
	if identity == proctree.TmuxUnknown {
		t.unclassified = append(t.unclassified, sparedProcess{Process: p, afterSIGTERM: t.termed[p.PID], readAt: sig})
	}
	// The liveness read comes after the identity read, so a live answer means
	// the identity read was of p itself: its pid cannot have been reused while it
	// is alive. A "gone" answer proves nothing (see tierIdentity), so it changes
	// only what is said.
	if t.alive(p) {
		log.InfoLog.Printf("pid %d (%s) was selected as not tmux, but just before its %s it reads as %s; "+
			"leaving it out of this signal tier and every later one of the worktree writer reap (#7244)",
			p.PID, p.Comm, signalName(sig), identity)
		return
	}
	log.InfoLog.Printf("pid %d (%s) reads as %s just before its %s, and as exited. A process mid-exec can read "+
		"that way too, so it is not signalled at this tier or any later one of the worktree writer reap (#7244)",
		p.PID, p.Comm, identity, signalName(sig))
}

// signalName names the two signals the reap sends as an operator writes them.
func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	default:
		return sig.String()
	}
}

// worktreeWriterProcesses selects the identity-verified snapshot entries the
// worktree reaper may signal. The process fact functions are explicit so tests
// can cover dangerous ancestry shapes without scanning or signalling real
// processes on the host.
func worktreeWriterProcesses(
	root string,
	snap map[int]proctree.Process,
	selfPID int,
	workingDir func(int) (string, bool),
	tmuxIdentity func(int) proctree.TmuxIdentity,
) []proctree.Process {
	matches := func(pid int) bool {
		cwd, ok := workingDir(pid)
		return ok && pathutil.IsAtOrInside(filepath.Clean(cwd), root)
	}
	procs, _ := worktreeWriterProcessesMatching(snap, selfPID, matches, tmuxIdentity)
	return procs
}

// worktreeWriterProcessesMatching applies the shared-infrastructure exclusions
// to both pathname-based cleanup and the repo-gone cleanup's descriptor-identity
// matcher. The latter must not lose the protections merely because it cannot
// safely re-resolve the pathname it is about to delete.
//
// It returns the kill set, and separately each process left out ONLY because its
// tmux identity was unknown, for warnUnclassifiedSurvivors.
func worktreeWriterProcessesMatching(
	snap map[int]proctree.Process,
	selfPID int,
	matches func(int) bool,
	tmuxIdentity func(int) proctree.TmuxIdentity,
) (procs, unclassified []proctree.Process) {
	// The daemon can inherit a cwd inside a worktree when an af invocation
	// auto-starts it there, and the shared tmux server inherits its cwd from the
	// client that first started it. Neither may be a selected root or be reached
	// through another matching ancestor. Prune each protected subtree during that
	// walk; descendants remain eligible when their own cwd independently matches.
	//
	// Every tmux process is protected, clients included — proctree.TmuxIdentityOf,
	// not IsTmuxServer (#4678). A client writes nothing into the worktree, and
	// af's own short-lived clients inherit the self-matching daemon's cwd, so
	// selecting them would kill the daemon's in-flight tmux commands for
	// unrelated sessions.
	//
	// So is every process whose tmux identity is UNKNOWN (#7238). Only a
	// positive NotTmux makes a process eligible. An unreadable command line
	// (a process mid-execve or mid-exit, or one darwin refuses) could belong to
	// the shared server, and signalling the server ends every session on it. That
	// costs at most one missed writer, and it is not loud on its own: a removal
	// fails only if the writer out-creates the unlink, and a relocate (archive)
	// succeeds with the writer still writing into the moved tree. So each such
	// process is returned as unclassified, and one still alive after the reap is
	// reported at WARNING (warnUnclassifiedSurvivors).
	//
	// The first identity read of a pid settles it for the whole pass (#7245
	// review R3). A pid read as unknown as a root and as not tmux inside another
	// root's walk used to be both unclassified and admitted, so the survivor
	// warning could call a signalled process unsignalled. Keeping the first
	// answer is the safe direction, and it is the rule the signal tiers follow.
	// A tracked teardown requester (#5182) — the process that asked for this
	// teardown, identified by kernel peer credentials and still blocked on its
	// reply — is alive inside the worktree for the best possible reason:
	// killing it kills the caller before the answer it asked for can arrive.
	// Unlike the infrastructure exclusions it exempts ONLY its own identity —
	// a requester that spawned a writer which later chdir'd out of the
	// worktree is still the ancestor keeping that writer reachable, and that
	// child is this session's to reap. So the requester stays in the candidate
	// set but is spared only while its registration holds — the per-signal
	// recheck in KillEscalatingExcept — and traversal continues INTO its
	// subtree exactly as the original matcher intends (Codex on #5186).
	requestersSeen := make(map[int]bool)
	isRequester := func(pid int) bool {
		p, ok := snap[pid]
		if !ok || !teardownreq.Is(p) {
			return false
		}
		if !requestersSeen[pid] {
			// Logged once per requester per pass — the same pid can be
			// evaluated twice (once as a root, once inside another matcher's
			// subtree).
			requestersSeen[pid] = true
			log.InfoLog.Printf("teardown requester pid %d (%s) is blocked on this teardown's reply; "+
				"sparing it while its registration holds (#5182)", p.PID, p.Comm)
		}
		return true
	}
	settled := make(map[int]proctree.TmuxIdentity)
	protectedInfrastructure := func(pid int) bool {
		if pid == selfPID {
			return true
		}
		identity, ok := settled[pid]
		if !ok {
			// Read once per pid per pass: a pid can be evaluated as a root and
			// again inside another root's subtree.
			identity = tmuxIdentity(pid)
			settled[pid] = identity
			if identity == proctree.TmuxUnknown {
				unclassified = append(unclassified, snap[pid])
				log.InfoLog.Printf("pid %d (%s) could not be classified as tmux or not: its command line or task "+
					"name could not be read; leaving it and its subtree out of the worktree writer reap (#7238)",
					pid, snap[pid].Comm)
			}
		}
		return identity != proctree.NotTmux
	}
	seen := make(map[int]bool)
	add := func(p proctree.Process) {
		if !seen[p.PID] {
			seen[p.PID] = true
			procs = append(procs, p)
		}
	}
	for pid := range snap {
		if !matches(pid) {
			continue
		}
		if protectedInfrastructure(pid) {
			// A tmux server is shared infrastructure whose cwd comes from the client
			// that first started it. It is not owned by this worktree, and selecting
			// its tree could terminate every tmux session on the server. The same
			// subtree rule protects a self-matching daemon and its unrelated sessions,
			// and a process that cannot be told apart from a tmux server.
			continue
		}
		if seen[pid] {
			continue
		}
		// Take the whole subtree of the matching process: a child of a
		// worktree-cwd'd writer is this session's too even if it chdir'd elsewhere,
		// and it may be the actual file-creator holding the directory non-empty.
		pruned := make(map[int]bool)
		for _, p := range proctree.TreeOf(snap, pid) {
			if pruned[p.PPID] {
				pruned[p.PID] = true
				continue
			}
			if protectedInfrastructure(p.PID) {
				pruned[p.PID] = true
				continue
			}
			if isRequester(p.PID) {
				// The requester stays IN the kill set — a registration that
				// lapses mid-reap must let the next signal tier reconsider it
				// rather than escaping the teardown permanently (Codex on
				// #5186) — while KillEscalatingExcept's per-signal recheck
				// spares it only while tracked. Its children are walked
				// regardless: the writer it spawned stays a writer — not
				// marking pruned keeps the walk reaching them.
			}
			add(p)
		}
	}
	return procs, unclassified
}
