package doctor

import "github.com/sachiniyer/agent-factory/internal/proctree"

// The ancestor-walk half of the orphan check's ownership proof, split out of
// checks_processes.go for the file-length lint (#1145) — the unit
// orphan_ancestor_proof_test.go names.
//
// markedProcessOwnership (in checks_processes.go) proves a marked process's
// home from the process's OWN environment, then calls ancestorWalk for the
// second layer: what the tree the process hangs from can prove. The walk can
// return a marked process as foreign even when its own environment is silent
// — a verified ancestor that DECLARES another install's AGENT_FACTORY_HOME —
// or unproven, when the chain cannot be fully verified. Only ancestryClean
// leaves the arm to the environment layers' verdict.

// ancestryVerdict is the outcome of walking a marked process's ancestor
// chain: clean (every ancestor up to init was verified and none declares
// another install's home), foreign (a verified ancestor declares another
// install's home), or unproven (the chain could not be fully verified).
type ancestryVerdict int

const (
	// ancestryUnknown is the zero value: a verdict nobody produced. It must
	// never read as clean — only an explicitly produced ancestryClean may
	// leave ownership to the environment layers (#6716).
	ancestryUnknown ancestryVerdict = iota
	ancestryClean
	ancestryForeign
	ancestryUnproven
)

// ancestryResult is the walk's verdict plus what a report row needs to
// describe it: the foreign home an ancestor declares, the pid the verdict
// names, and — for an unproven verdict — the specific way the chain failed,
// so the row can state the true cause instead of listing every possible one.
type ancestryResult struct {
	verdict ancestryVerdict
	home    string
	pid     int
	cause   ancestorUnproven
}

// ancestorUnproven is the specific way an ancestor chain failed verification.
type ancestorUnproven int

const (
	ancestorGapNone ancestorUnproven = iota
	// ancestorGapMissing: the recorded ancestor is absent from the snapshot.
	ancestorGapMissing
	// ancestorGapIdentity: the pid no longer names the process the snapshot
	// recorded (reaped and recycled, or unverifiable).
	ancestorGapIdentity
	// ancestorGapEnv: the ancestor's environment could not be read or a
	// declared home could not be resolved in its frame.
	ancestorGapEnv
	// ancestorGapCycle: the chain loops back onto an already-seen pid — it
	// never reaches init, so the proof is truncated, not complete.
	ancestorGapCycle
	// ancestorGapDepth: the hop bound ran out with a recorded parent still
	// unexamined.
	ancestorGapDepth
)

func (c ancestorUnproven) describe() string {
	switch c {
	case ancestorGapMissing:
		return "it is missing from the process snapshot"
	case ancestorGapIdentity:
		return "it no longer names the process the snapshot recorded"
	case ancestorGapEnv:
		return "its environment could not be read"
	case ancestorGapCycle:
		return "the ancestor chain loops back to it"
	case ancestorGapDepth:
		return "the ancestor chain exceeds the verification bound"
	}
	return "it could not be verified"
}

// ancestorHomeProof walks p's ancestor chain in the run's snapshot and proves
// what it can about the tree the process lives in: ancestryForeign plus the
// first home an ancestor DECLARES through its own AGENT_FACTORY_HOME that is
// not this install's — proof the process runs under another install's tmux
// server, daemon or helper, even when its own environment resolves to this
// home or cannot be attributed at all.
//
// The walk fails CLOSED (#6703): ancestryUnproven when a recorded ancestor is
// absent from the snapshot (it exited mid-scan, or under a restricted process
// table was never listed), when the pid no longer names the process the
// snapshot recorded (reaped and recycled — SameIdentity, the same check the
// death proof applies to stamped server pids), or when the ancestor's
// environment cannot be read — a denied or withheld environ is not "declares
// nothing". On a path that may arm a kill, an ancestor that cannot be
// interrogated is an ancestor whose home cannot be ruled out.
//
// Only an explicit declaration counts; the HOME-derived default that
// daemonProcessHome applies to the candidate is deliberately not applied to
// ancestors. A process that declares no agent-factory home is just some
// process: applying the default up the tree would let a HOME=/root login
// shell — or pid 1 in a container — read as "another install" and disarm the
// arm for every descendant, the mirror image of the kill this walk exists to
// prevent. An ancestor that is READ and declares no home is clean, not
// unproven — only gaps count as gaps.
func (ctx *scanContext) ancestorHomeProof(p proctree.Process) ancestryResult {
	seen := map[int]bool{p.PID: true}
	pid := p.PPID
	for hops := 0; pid > 1 && hops < ancestorHopLimit; hops++ {
		if seen[pid] {
			// A cycle back onto an already-seen pid means the chain never
			// reaches init — the proof is truncated, not complete, so the
			// verdict is unproven, not clean (#6704).
			return ancestryResult{verdict: ancestryUnproven, pid: pid, cause: ancestorGapCycle}
		}
		seen[pid] = true
		a, ok := ctx.snap[pid]
		if !ok {
			// The recorded parent is absent from the snapshot: it exited
			// between the stat pass and now, or the table never showed
			// it. Either way it cannot be interrogated.
			return ancestryResult{verdict: ancestryUnproven, pid: pid, cause: ancestorGapMissing}
		}
		if same, err := ancestorIdentity(a); err != nil || !same {
			// The pid was recycled since the snapshot — or cannot be
			// revalidated — so the record describes a different process
			// than the one running now.
			return ancestryResult{verdict: ancestryUnproven, pid: pid, cause: ancestorGapIdentity}
		}
		home, declared, readable := declaredAgentFactoryHome(pid)
		if !readable {
			return ancestryResult{verdict: ancestryUnproven, pid: pid, cause: ancestorGapEnv}
		}
		// Revalidate AFTER the environment read, not only before it
		// (#6704): the pid may have been recycled while the environ was
		// being read, and believing the answer then is trusting whatever
		// the pid names NOW — which need not be the ancestor the snapshot
		// recorded.
		if same, err := ancestorIdentity(a); err != nil || !same {
			return ancestryResult{verdict: ancestryUnproven, pid: pid, cause: ancestorGapIdentity}
		}
		if declared && !sameHome(home, ctx.opts.ConfigDir) {
			return ancestryResult{verdict: ancestryForeign, home: home, pid: pid}
		}
		pid = a.PPID
	}
	if pid > 1 {
		// The hop bound ran out mid-chain — the unexamined tail is
		// unverified. The cause is the bound itself, not a missing or
		// unreadable ancestor.
		return ancestryResult{verdict: ancestryUnproven, pid: pid, cause: ancestorGapDepth}
	}
	return ancestryResult{verdict: ancestryClean}
}

// declaredAgentFactoryHome resolves the home a process DECLARES through its
// own AGENT_FACTORY_HOME, in its own frame — the explicit half of
// daemonProcessHome's rule, without the HOME-derived default (see
// ancestorHomeProof for why ancestors get no default). The third return is
// false when the environment could not be read or a declared home could not
// be resolved in the ancestor's frame — "no answer" is never "no home"
// (#6703).
func declaredAgentFactoryHome(pid int) (string, bool, bool) {
	raw, found, err := daemonProcessEnvLookup(pid, "AGENT_FACTORY_HOME")
	if err != nil {
		return "", false, false
	}
	if !found || raw == "" {
		return "", false, true
	}
	homeDir, _, err := daemonProcessEnvLookup(pid, "HOME")
	if err != nil {
		return "", false, false
	}
	cwd, _ := daemonProcessCwd(pid)
	home, resolved := resolveHomeIn(raw, homeDir, cwd)
	if !resolved {
		return "", false, false
	}
	return home, true, true
}

// ancestorIdentity is the live-process revalidation the ancestor walk applies
// to each recorded ancestor — a var so a test can script an identity that
// changes mid-scan rather than race a real exec (#6704).
var ancestorIdentity = proctree.SameIdentity

// ancestorWalk is the classifier's call into the ancestor proof — a var so a
// test can hand the classifier a verdict nobody produced (#6716).
var ancestorWalk = func(ctx *scanContext, p proctree.Process) ancestryResult {
	return ctx.ancestorHomeProof(p)
}

// ancestorHopLimit bounds the ancestor chain — a var so a test can shrink it
// rather than spawn a chain past the production bound (#6704).
var ancestorHopLimit = 128
