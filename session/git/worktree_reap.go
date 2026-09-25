// The worktree writer-reaper layer: the best-effort pass that kills every live
// process still working inside a worktree before the tree is deleted, plus the
// shared snapshot-selection machinery the pathname matcher and the repo-gone
// cleanup's descriptor-identity matcher both apply. Nothing here knows about
// GitWorktree — only paths, PIDs, and the protected-infrastructure exclusions.
package git

import (
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
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
	procs := worktreeWriterProcessesMatching(snap, os.Getpid(), matches, proctree.IsTmuxProcess)
	if len(procs) == 0 {
		return
	}
	proctree.KillEscalating(procs, worktreeReapGrace, worktreeReapTermWait, func(_ proctree.ReapOutcome, format string, args ...any) {
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

// worktreeWriterProcesses selects the identity-verified snapshot entries the
// worktree reaper may signal. The process fact functions are explicit so tests
// can cover dangerous ancestry shapes without scanning or signalling real
// processes on the host.
func worktreeWriterProcesses(
	root string,
	snap map[int]proctree.Process,
	selfPID int,
	workingDir func(int) (string, bool),
	isTmuxProcess func(int) bool,
) []proctree.Process {
	matches := func(pid int) bool {
		cwd, ok := workingDir(pid)
		return ok && pathutil.IsAtOrInside(filepath.Clean(cwd), root)
	}
	return worktreeWriterProcessesMatching(snap, selfPID, matches, isTmuxProcess)
}

// worktreeWriterProcessesMatching applies the shared-infrastructure exclusions
// to both pathname-based cleanup and the repo-gone cleanup's descriptor-identity
// matcher. The latter must not lose the protections merely because it cannot
// safely re-resolve the pathname it is about to delete.
func worktreeWriterProcessesMatching(
	snap map[int]proctree.Process,
	selfPID int,
	matches func(int) bool,
	isTmuxProcess func(int) bool,
) []proctree.Process {
	// The daemon can inherit a cwd inside a worktree when an af invocation
	// auto-starts it there, and the shared tmux server inherits its cwd from the
	// client that first started it. Neither may be a selected root or be reached
	// through another matching ancestor. Prune each protected subtree during that
	// walk; descendants remain eligible when their own cwd independently matches.
	//
	// Every tmux process is protected, clients included — proctree.IsTmuxProcess,
	// not IsTmuxServer (#4678). A client writes nothing into the worktree, and
	// af's own short-lived clients inherit the self-matching daemon's cwd, so
	// selecting them would kill the daemon's in-flight tmux commands for
	// unrelated sessions.
	protectedInfrastructure := func(pid int) bool {
		return pid == selfPID || isTmuxProcess(pid)
	}
	seen := make(map[int]bool)
	var procs []proctree.Process
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
			// subtree rule protects a self-matching daemon and its unrelated sessions.
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
			add(p)
		}
	}
	return procs
}
