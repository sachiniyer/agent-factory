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
	protectReasonSelfSubtree = "a descendant of the reaper outside a tmux server boundary"
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
//   - a captured process whose parent walk REACHES the reaper — the reaper's
//     own descendant — unless a positively-identified tmux process lies
//     between them (#7433 R2, #7435): the reaper's own children are excluded
//     like any af daemon's subtree, because descent is not ownership and an
//     inherited-marker child of the reaper is not this session's to kill;
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
//   - the reaper ends the walk protected: the captured process is the
//     reaper's own descendant, excluded like an af daemon's subtree (#7435).
//     A POSITIVELY identified tmux process is checked FIRST, so a pane
//     process under a server that itself descends from the reaper still ends
//     eligible at the server — the boundary verdict only fails to protect
//     the reaper's own children when no server lies between them and it;
//   - a positively-identified agent-factory daemon ends the walk protected;
//   - a pid whose argv cannot be read ends the walk protected (unproven);
//   - the top of the chain ends it eligible: an orphan whose parent died
//     reaches that end like any other chain.
func classifyCaptureAncestry(snap map[int]proctree.Process, pid, self int) string {
	start := pid
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
			return protectReasonSelfSubtree
		case proctree.TmuxIdentityOf(pid) == proctree.Tmux:
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

// reportableSurvivors filters a survivor set the way signalling filtered its
// kill set (#7435 R3): a process the protected set spares by PROOF — the
// reaper's own chain, a positively-identified af-daemon subtree — is not a
// leak and must not read as one forever. A process spared only because its
// daemon ancestry could not be proven is different: alive, unattributable,
// and never "nothing found" — it stays reported so a cleanup-authorizing
// caller still refuses. snap is the SAME table the survivor set was built
// from, so both verdicts classify against identical ancestry.
func reportableSurvivors(snap map[int]proctree.Process, survivors []proctree.Process) []proctree.Process {
	if len(survivors) == 0 {
		return survivors
	}
	protected := reapProtectedSet(snap, survivors)
	var reportable []proctree.Process
	for _, p := range survivors {
		if reason := protected[p.PID]; reason != "" && reason != protectReasonUnproven {
			continue
		}
		reportable = append(reportable, p)
	}
	return reportable
}

type afDaemonVerdict int

const (
	afDaemonNot afDaemonVerdict = iota
	afDaemonIs
	afDaemonUncertain
)

// afDaemonClass positively identifies an agent-factory daemon from the
// snapshot entry: the same af-binary-plus---daemon argv classification the
// daemon's own PID validation uses (proctree.ArgvIsAFDaemon). A kernel thread
// can never be a daemon.
//
// When argv cannot be read the kernel task name answers instead. An unreadable
// argv is ROUTINE on darwin — the kernel declines KERN_PROCARGS2 for a
// foreign-uid target, so every orphan reparented to launchd would report
// "uncertain" and be protected, and nothing on darwin could ever be reaped
// (#7435 CI on macOS). p.Comm needs no permission and names the same
// executable argv[0] names, so a comm that cannot be an af binary rules the
// process OUT. Only an af-named comm with unreadable argv stays uncertain —
// the still-fail-closed direction for a real foreign-uid daemon.
func afDaemonClass(p proctree.Process) afDaemonVerdict {
	if p.KernelThread {
		return afDaemonNot
	}
	if argv := proctree.Argv(p.PID); len(argv) > 0 {
		if proctree.ArgvIsAFDaemon(argv) {
			return afDaemonIs
		}
		return afDaemonNot
	}
	if commNamesAFDaemonBinary(p.Comm) {
		return afDaemonUncertain
	}
	return afDaemonNot
}

// commNamesAFDaemonBinary reports whether a kernel task name is one argv[0]
// could carry for an agent-factory daemon: the same {af, agent-factory} set
// proctree.ArgvIsAFBinary checks. Both names fit inside the task-name field
// (MAXCOMLEN) untruncated.
func commNamesAFDaemonBinary(comm string) bool {
	return comm == "af" || comm == "agent-factory"
}
