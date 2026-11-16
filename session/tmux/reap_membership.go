package tmux

import (
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// protectedSet is the pid set a captured-tree reap must never signal, each
// mapped to the reason it is excluded, so the log can say WHY a member of a
// session's own captured tree was spared (#7435).
type protectedSet map[int]string

func (s protectedSet) protects(pid int) bool {
	_, ok := s[pid]
	return ok
}

// Exclusion reasons. Each completes the log line "process pid %d (%s) is %s;
// excluding it from signalling".
const (
	protectReasonSelf        = "the reaper's own process"
	protectReasonAncestor    = "an ancestor of the reaper"
	protectReasonSelfSubtree = "a descendant of the reaper that reached it below this session's tmux server"
	protectReasonAFDaemon    = "an agent-factory daemon"
	protectReasonUnproven    = "a process whose daemon ancestry cannot be proven"
)

func protectReasonDaemonDescendant(daemonPID int) string {
	return fmt.Sprintf("a descendant of agent-factory daemon pid %d", daemonPID)
}

// reapProtectedSet computes, from ONE pre-kill process-table snapshot — the
// same one the captured process list was built from (#7435) — the pids a reap
// of that captured tree must never signal, and why:
//
//   - the reaper's own process and its ancestors, unconditionally;
//   - a captured process whose parent walk meets the reaper strictly below a
//     positively-identified tmux process — descendant → reaper → tmux (#7433
//     R2);
//   - a captured process that IS an agent-factory daemon, of ANY home, or that
//     descends from one (#7435): a daemon EnsureDaemon forked from a CLI
//     running inside an orphaned pane lands in that pane's captured tree with
//     everything it has spawned, and descent is not ownership.
//
// Descent still decides captured-tree membership — detached children that left
// the pane's kernel session (Node spawn detached:true, Playwright browsers,
// dev servers, language servers) are reaped by descent today and must stay
// reaped — but membership no longer decides signalability on its own: within
// the captured set, daemon subtrees and the reaper's own chain are positively
// identified out of the same snapshot and excluded.
//
// The error direction is asymmetric, so the walk errs toward exclusion: a
// false exclusion leaks a process (safe), while a false inclusion kills a
// daemon (not). A process that cannot be positively classified — its own argv
// unreadable, or an ancestor's — is excluded rather than risked. An ancestor
// absent from the snapshot is different: it is dead or dying, and no live
// daemon can hide in a dead link — the walk simply ends there, which is what
// keeps genuine orphans (whose parents die by definition) reapable.
//
// Protection is decided only for captured processes, so in the common case —
// no captured process sits in the reaper's ancestry or subtree — no identity
// walk runs at all.
func reapProtectedSet(snap map[int]proctree.Process, captured []proctree.Process) protectedSet {
	self := os.Getpid()
	protected := protectedSet{self: protectReasonSelf}
	if snap == nil {
		log.WarningLog.Printf("the process-table snapshot is missing; only the reaper's own pid is excluded from signalling")
		return protected
	}
	for pid := range selfAndAncestorProcesses(snap) {
		if pid != self {
			protected[pid] = protectReasonAncestor
		}
	}
	for _, p := range captured {
		if protected.protects(p.PID) {
			continue
		}
		if reason := classifyCaptureAncestry(snap, p.PID, self); reason != "" {
			protected[p.PID] = reason
		}
	}
	return protected
}

// classifyCaptureAncestry walks pid's parent chain inside the capture's own
// snapshot and returns the reason the process is protected, or "" when it
// stays reapable. The first decisive verdict wins:
//
//   - the reaper itself is marked reached; a POSITIVELY identified tmux
//     process afterwards ends the walk — the R2 own-chain descendant
//     (reaper → tmux above it) is protected, while every pane process reaches
//     its server first and stays eligible;
//   - a positively-identified agent-factory daemon ends the walk protected;
//   - a pid whose argv cannot be read ends the walk protected (unproven);
//   - the top of the chain ends it eligible: a reaper with no tmux or daemon
//     above it exempts nothing, and an orphan whose parent died reaches that
//     end like any other chain.
func classifyCaptureAncestry(snap map[int]proctree.Process, pid, self int) string {
	start := pid
	reachedSelf := false
	for range 128 {
		if pid <= 0 {
			return ""
		}
		p, ok := snap[pid]
		if !ok {
			return ""
		}
		switch {
		case pid == self:
			reachedSelf = true
		case proctree.TmuxIdentityOf(pid) == proctree.Tmux:
			if reachedSelf {
				return protectReasonSelfSubtree
			}
			return ""
		default:
			switch afDaemonClass(p) {
			case afDaemonIs:
				if pid == start {
					return protectReasonAFDaemon
				}
				return protectReasonDaemonDescendant(pid)
			case afDaemonUncertain:
				return protectReasonUnproven
			}
		}
		pid = p.PPID
	}
	return ""
}

type afDaemonVerdict int

const (
	afDaemonNot afDaemonVerdict = iota
	afDaemonIs
	afDaemonUncertain
)

// afDaemonClass positively identifies an agent-factory daemon from the
// snapshot entry: the same af-binary-plus---daemon argv classification the
// daemon's own PID validation uses (proctree.ArgvIsAFDaemon). A process whose
// argv cannot be read is NOT ruled out — it reports uncertain so the caller
// errs toward exclusion. A kernel thread can never be a daemon.
func afDaemonClass(p proctree.Process) afDaemonVerdict {
	if p.KernelThread {
		return afDaemonNot
	}
	argv := proctree.Argv(p.PID)
	if len(argv) == 0 {
		return afDaemonUncertain
	}
	if proctree.ArgvIsAFDaemon(argv) {
		return afDaemonIs
	}
	return afDaemonNot
}
