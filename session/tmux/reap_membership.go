package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
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
		if reason, _ := classifyCaptureAncestry(snap, p.PID, self); reason != "" {
			protected[p.PID] = reason
		}
	}
	return protected
}

// classifyCaptureAncestry walks pid's parent chain inside the capture's own
// snapshot and returns the reason the process is protected — or "" when it
// stays reapable — plus whether the walk met a positively-identified tmux
// process before it ended. The first decisive verdict wins:
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
//
// The boundary flag is reported but does not itself protect: on a capture
// path the server a pane still sits under is our own mid-kill and its
// members stay reapable. The VANISHED-session cohort reads it the other
// way — a marker carrier still behind a server is a foreign pane (#6846,
// #7435 R6), not a member that escaped.
func classifyCaptureAncestry(snap map[int]proctree.Process, pid, self int) (reason string, tmuxBoundary bool) {
	start := pid
	for range 128 {
		if pid <= 0 {
			return "", false
		}
		p, ok := snap[pid]
		if !ok {
			return "", false
		}
		switch {
		case pid == self:
			return protectReasonSelfSubtree, false
		case proctree.TmuxIdentityOf(pid) == proctree.Tmux:
			return "", true
		default:
			switch afDaemonClass(p) {
			case afDaemonIs:
				if pid == start {
					return protectReasonAFDaemon, false
				}
				return protectReasonDaemonDescendant(pid), false
			case afDaemonUncertain:
				return protectReasonUnproven, false
			}
		}
		pid = p.PPID
	}
	return "", false
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

// cohortMembership is the kernel evidence a vanished-session cohort can still
// demand after the tmux record is gone (#7435 item 5):
//
//   - Proven holds (pid → StartID) pairs the caller itself captured inside the
//     proven live tree before the session vanished — descent capture IS
//     membership proof (item 1): a process observed inside the verified pane
//     tree is the session's own even if it setsid'd out of its kernel session
//     (the escapee shape, #1104). Processes discovered by descent from a proven
//     member during the sweep are admitted the same way.
//   - PaneSIDs holds the kernel session ids the capture's verified pane roots
//     led — a marker-matched process still carrying one is a pane member.
//   - Recorded holds (pid → StartID) pairs a caller recorded when the pane was
//     spawned (#5066).
//   - Root is the recorded pane root — the launch's own (pid, StartID) — when
//     the caller carries one. It anchors the generation-identity tier.
//   - AgentExe names the session's resolved program binary (its basename —
//     "claude", "sleep"): the only executable identity tier 2 will admit by
//     (#7435 R9). Empty means the caller does not know it, and tier 2 cannot
//     run at all.
//
// What is NOT evidence is the point: a marker-matched process with none of
// these is exactly the inherited-env case — an af daemon's or helper's child
// that setsid'd out of the pane, carrying markers it inherited — and it is
// REPORTED, never silently dropped: "nothing found" on this path authorizes
// worktree cleanup, so an unattributable survivor must block it. With NO
// evidence at all the gate refuses every candidate rather than signalling on
// env alone (#7435 R2): a blind sweep that captured nothing to place against
// keeps its refusals — it just never opens the kill set.
type cohortMembership struct {
	Proven   map[int]uint64
	PaneSIDs map[int]bool
	Recorded map[int]uint64
	// Root is the recorded pane root — the launch's own (pid, StartID) — when
	// the caller carries one. It anchors the generation-identity tier: a
	// marker carrier that began no earlier than the root could only have been
	// spawned under this session's life (#7435 R6). A zero Process means no
	// recorded root, and that tier cannot run.
	Root proctree.Process
	// AgentExe is the basename of the session's resolved program binary —
	// "claude" for an agent tab, "sleep" for a process tab, "af" for an
	// internal pane. Tier 2 admits only a candidate whose own executable
	// identity matches it (plus that candidate's marked subtree and its
	// direct marked host); every other marker carrier without kernel evidence
	// is refused and reported, never signalled (#7435 R9). Empty means the
	// caller could not name the program, and tier 2 admits nothing.
	AgentExe string
}

func (m cohortMembership) admits(snap map[int]proctree.Process, p proctree.Process) bool {
	if len(m.Proven) == 0 && len(m.PaneSIDs) == 0 && len(m.Recorded) == 0 {
		// No kernel evidence at all: nothing is admitted — refuse, never
		// signal (#7435 R2). A marker-only scan cannot prove membership, so
		// the caller that arrives without captured, SID, or recorded
		// evidence gets refusals for every candidate it asks about.
		return false
	}
	if m.PaneSIDs[p.SID] {
		// A SID vouches only while the leader the evidence names still leads
		// it — or is simply gone, leaving the session to its members (#7435
		// R8): a pid recycled into a NEW session head carrying our markers
		// must not vouch for a cohort it never led. The leader's expected
		// StartID is the recorded pair when one exists, else the proven
		// capture's own identity for that pid.
		leaderStartID, bound := m.Recorded[p.SID]
		if !bound {
			leaderStartID, bound = m.Proven[p.SID]
		}
		if leader, ok := snap[p.SID]; !ok || (bound && leader.StartID == leaderStartID) {
			return true
		}
	}
	if startID, ok := m.Recorded[p.PID]; ok && startID == p.StartID {
		return true
	}
	if startID, ok := m.Proven[p.PID]; ok && startID == p.StartID {
		return true
	}
	// Descent from a proven member: a child a captured process forked after
	// the session died was never in the capture, but its ancestry places it
	// inside the proven tree all the same. The StartID recheck keeps a reused
	// pid from minting membership it does not hold.
	for ancestor, i := p.PPID, 0; ancestor > 1 && i < 128; i++ {
		q, ok := snap[ancestor]
		if !ok {
			break
		}
		if startID, ok := m.Proven[ancestor]; ok && startID == q.StartID {
			return true
		}
		ancestor = q.PPID
	}
	return false
}

// markedOrphanProcesses filters a previously captured pane process tree down to
// processes whose immutable launch environment proves they belong to this exact
// tmux generation and AF home. The capture happens while the listed session
// still exists; if ownership lookup then loses the session, AF_SESSION,
// AF_SESSION_GEN, and AF_HOME provide the authority that the vanished tmux
// environment no longer can.
//
// The candidate set is load-bearing. Scanning every same-user process would
// turn an unrelated unreadable /proc environment into a possible helper and
// make successful absence impossible on hardened hosts. Within the captured
// tree, unreadable or mismatched provenance is genuinely UNKNOWN and blocks
// worktree deletion rather than being collapsed into "not ours".
//
// An EMPTY cohort — the blind vanished-session sweep, which captured no
// predecessor to place anyone against — still refuses, but as ONE message rather
// than a line per pid: there the pid/generation pairing is the operator's whole
// decision procedure, and blindGenerationRefusal is what carries it (#3706).
// The returned snapshot is the ONE process-table read this filter made: it
// decided identity, ancestry and membership for every verdict, so a caller
// that must apply the same exclusions the signalling reap did — the final
// survivor check (#7435 R3) — classifies against this same table instead of
// a second read that could disagree with it.
func markedOrphanProcesses(candidates []proctree.Process, sanitizedName, ownHome string, generations orphanGenerationSet, membership cohortMembership, reported map[int]bool) ([]proctree.Process, map[int]proctree.Process, error) {
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	if reported == nil {
		reported = make(map[int]bool)
	}
	var inspectErrs []error
	report := func(pid int, err error) {
		// A survivor the sweep refuses on is named once across every pass
		// that sees it — the refusal reason may differ pass to pass, but a
		// pid repeated per pass reads as a bigger leak than exists (#7435 R8).
		if !reported[pid] {
			reported[pid] = true
			inspectErrs = append(inspectErrs, err)
		}
	}
	snap, err := proctree.Snapshot()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot inspect captured processes after tmux session vanished: %w", err)
	}
	self := os.Getpid()
	selfUID, selfUIDKnown := proctree.UID(self)
	if !selfUIDKnown {
		return nil, nil, fmt.Errorf("cannot determine reset process ownership while checking vanished tmux session %s", sanitizedName)
	}
	selfChain := selfAndAncestorProcesses(snap)
	var unplaceable []markedGeneration
	var eligible []proctree.Process
	eligibleClean := make(map[int]bool)
	for _, process := range candidates {
		same, identityErr := proctree.SameIdentity(process)
		if identityErr != nil {
			report(process.PID, fmt.Errorf("cannot determine whether captured pid %d is still the same process: %w",
				process.PID, identityErr))
			continue
		}
		if !same {
			continue
		}
		uid, uidKnown := proctree.UID(process.PID)
		if !uidKnown {
			sameAfter, recheckErr := proctree.SameIdentity(process)
			switch {
			case recheckErr != nil:
				report(process.PID, fmt.Errorf("cannot recheck captured pid %d after owner lookup failed: %w",
					process.PID, recheckErr))
			case sameAfter:
				report(process.PID, fmt.Errorf("cannot determine owner of captured live pid %d", process.PID))
			}
			continue
		}
		if uid != selfUID {
			report(process.PID, fmt.Errorf("captured live pid %d belongs to uid %d, not reset uid %d",
				process.PID, uid, selfUID))
			continue
		}

		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			sameAfter, recheckErr := proctree.SameIdentity(process)
			switch {
			case recheckErr != nil:
				report(process.PID, fmt.Errorf("cannot recheck captured pid %d after environment lookup failed: %w",
					process.PID, errors.Join(envErr, recheckErr)))
			case sameAfter:
				report(process.PID, fmt.Errorf("cannot determine whether captured live pid %d belongs to session %s: %w",
					process.PID, sanitizedName, envErr))
			}
			continue
		}
		processSession, hasSession := processEnvValue(environ, EnvMarkerSession)
		if !hasSession {
			report(process.PID, fmt.Errorf("captured live pid %d has no %s marker for vanished session %s",
				process.PID, EnvMarkerSession, sanitizedName))
			continue
		}
		if processSession != sanitizedName {
			report(process.PID, fmt.Errorf("captured live pid %d marks session %s instead of vanished session %s",
				process.PID, processSession, sanitizedName))
			continue
		}
		processHome, hasHome := processEnvValue(environ, EnvMarkerHome)
		if !hasHome {
			report(process.PID, fmt.Errorf("captured live pid %d marks session %s but has no %s ownership marker",
				process.PID, sanitizedName, EnvMarkerHome))
			continue
		}
		switch classifyHomeDir(processHome, ownHome) {
		case homeForeign:
			continue
		case homeUnproven:
			// Unproven is never "not ours, carry on" (#7435 Codex): a relative
			// or unstat-able marker could name this home or another's, so the
			// process is UNATTRIBUTABLE — refusing is the only answer that
			// keeps a survivor check from reporting an empty set while the
			// process is still alive (the close path would authorize deleting
			// the worktree under it).
			switch {
			case !filepath.IsAbs(ownHome):
				// The recorded home itself is relative or missing — af was
				// launched with a relative AF_HOME or lost it before this
				// record was written. No marker can be proven against it, so
				// name that root cause instead of letting it read as one
				// process's problem (#7435 R8).
				report(process.PID, fmt.Errorf(
					"captured live pid %d marks session %s but the recorded home (%s) is relative or missing, so no %s marker can be proven against it",
					process.PID, sanitizedName, ownHome, EnvMarkerHome))
			case !filepath.IsAbs(processHome):
				report(process.PID, fmt.Errorf(
					"captured live pid %d marks session %s but carries a relative %s marker (%s) that cannot be proven to name this home (%s)",
					process.PID, sanitizedName, EnvMarkerHome, processHome, ownHome))
			default:
				report(process.PID, fmt.Errorf(
					"captured live pid %d marks session %s but its %s marker (%s) cannot be proven to name this home (%s) — one of the directories does not resolve",
					process.PID, sanitizedName, EnvMarkerHome, processHome, ownHome))
			}
			continue
		}
		// The exclusion set is the same one signalling applies (#7435 R3): the
		// marker checks below decide MEMBERSHIP only for a process whose
		// ancestry does not already place it. The reaper and its ancestors keep
		// their refusal — reaching here carrying this session's markers means
		// the reaper is inside the cohort it is judging, so it refuses. A
		// process POSITIVELY placed in another protected subtree — the reaper's
		// own descendant outside a tmux boundary, or a proven af-daemon subtree
		// — is proven NOT ours: skipped silently like signalling spares it, and
		// never reported as a leak (Codex 4593411284). Only protectReasonUnproven
		// falls through: it stays unattributable and must surface below.
		if selfChain[process.PID] {
			report(process.PID, fmt.Errorf("refusing to reap reset process or ancestor pid %d for vanished session %s",
				process.PID, sanitizedName))
			continue
		}
		reason, boundary := classifyCaptureAncestry(snap, process.PID, self)
		switch {
		case boundary:
			// A positively-identified tmux process in the chain means this
			// carrier lives under SOME server. Once the session has vanished
			// a true member's chain ends at init or at its own non-tmux
			// ancestors — never at a server — so a carrier still behind one
			// is a foreign pane (#6846's global-env copy, a second server
			// stamped with our markers): proven NOT ours, excluded silently
			// the way a placed subtree is (#7435 R6). It is never reported:
			// "leak" would claim a member this sweep owes a kill to.
			log.InfoLog.Printf("tmux session %s: marked pid %d (%s) sits behind a tmux server — a foreign pane; excluding it",
				sanitizedName, process.PID, process.Comm)
			continue
		case reason != "" && reason != protectReasonUnproven:
			log.InfoLog.Printf("tmux session %s: marked pid %d (%s) is %s; excluding it the way signalling does",
				sanitizedName, process.PID, process.Comm, reason)
			continue
		}
		processGeneration, hasGeneration := processEnvValue(environ, EnvMarkerGeneration)
		switch {
		case hasGeneration && generations.empty():
			// An EMPTY cohort is the blind vanished-session sweep (cleanup.go): it
			// captured no predecessor, so it has nothing to place this generation
			// against — a different statement from "outside a cohort I know", which
			// is what the case below reports. Collected instead of refused one pid
			// at a time so the refusal can be ONE message naming every unplaceable
			// pid and the generation it carries: on this path that comparison is
			// the operator's entire decision procedure, and it does not fit in a
			// per-pid line (#3706). See blindGenerationRefusal.
			//
			// The reaper's own chain never reaches this bucket — it is refused
			// above, before ancestry is consulted — so collecting here cannot
			// print "safe to kill" at the running af, the footgun this message
			// exists to avoid. A blind scan really can reach it:
			// refreshOrphanCandidates walks the whole table for AF_SESSION
			// matches with no self-exclusion, so an af invoked inside a pane of
			// the session that then vanished carries that session's markers.
			unplaceable = append(unplaceable, markedGeneration{pid: process.PID, generation: processGeneration})
			continue
		case hasGeneration && !generations.values[processGeneration]:
			// Marker scans are filtered before they enter this bounded set. A
			// different generation already present here may be a descendant that
			// changed its environment after capture, so absence is not proved. It
			// must remain a refusal rather than being silently reclassified as a
			// replacement (#3309 review).
			report(process.PID, fmt.Errorf("captured live pid %d marks generation %s outside the vanished session %s generation cohort",
				process.PID, processGeneration, sanitizedName))
			continue
		case !hasGeneration && !generations.legacy:
			report(process.PID, fmt.Errorf("captured live pid %d marks session %s but has no %s generation marker",
				process.PID, sanitizedName, EnvMarkerGeneration))
			continue
		}
		// Everything below the generation gate is a MEMBERSHIP verdict, so
		// gate-passed candidates are collected first and admitted second: the
		// tier-2 allowlist needs the whole eligible cohort before it can tell
		// an agent program's host apart from a look-alike (#7435 R9).
		eligible = append(eligible, process)
		eligibleClean[process.PID] = reason == ""
	}
	// Environment is proof of CARRYING the session's launch markers, never
	// of MEMBERSHIP: a daemon or helper that setsid'd out of the pane keeps
	// inherited markers while joining a session this sweep has no claim on
	// (#7435 item 5 — tmux's global environment can expose them to unrelated
	// later panes too, #6846). Membership is decided in two tiers. Tier 1
	// is kernel evidence: a SID one of the proven pane roots led, a
	// (pid, StartID) pair recorded at spawn, capture inside the proven
	// live tree, or descent from one of those proven members. Tier 2
	// (#7435 R6/R9) is the re-hosted shape kernel membership cannot see —
	// but as an ALLOWLIST on executable identity, not the open "clean
	// ancestry plus StartID" rule it replaced, which admitted anything
	// pty-host-shaped: a screen server and its window, an af daemon's
	// double-forked helper, the reaper's own escaped helper. A carrier is
	// admitted only when its OWN executable identity — comm or argv[0], the
	// same sources the af-daemon classifier uses — is the session's resolved
	// program binary, or it descends from such a carrier through marked
	// eligible ancestors, or it is the direct marked parent of one. The
	// #3699 chain (claude daemon run → bg-pty-host → claude) is the agent
	// binary re-hosting itself under a shim exactly one hop up, so it still
	// converges; a screen/zellij/dtach server or any other pty host fails
	// the identity match and is REFUSED and reported, never signalled — the
	// safe direction for a process af cannot prove it owns, with its pids
	// named for the operator. Tier 2 still requires the R6 guards: a
	// recorded root, an eligible-clean ancestry verdict above (every
	// positive outside — tmux boundary, af-daemon subtree, reaper chain —
	// already excluded, and an unproven ancestor stays excluded), and
	// StartID at or after the root's. An unadmitted carrier is REPORTED,
	// never silently dropped: "nothing found" on this path authorizes
	// worktree cleanup, so an unattributable survivor must block it
	// exactly like an unproven home (#7435 R3, Codex 4593576722).
	agentExe := membership.AgentExe
	if agentExe == "" {
		// The recorded root's own kernel name IS the program that ran in the
		// pane — the strongest form of the same identity, available whenever
		// the anchor carries it (#7435 R9).
		agentExe = membership.Root.Comm
	}
	eligibleSet := make(map[int]bool, len(eligible))
	exeMatch := make(map[int]bool)
	for _, c := range eligible {
		eligibleSet[c.PID] = true
		if agentExe != "" && exeMatchesProgram(c, agentExe) {
			exeMatch[c.PID] = true
		}
	}
	var owned []proctree.Process
	for _, process := range eligible {
		if !membership.admits(snap, process) {
			// Root.StartID must be nonzero too: a 0 anchor makes the
			// no-earlier-than bound vacuous and would admit every clean
			// carrier (#7435 R6).
			if eligibleClean[process.PID] && membership.Root.PID > 0 && membership.Root.StartID != 0 &&
				process.StartID >= membership.Root.StartID &&
				tier2ProgramAdmits(snap, eligibleSet, exeMatch, process) {
				// One WARNING per tier-2 admission: the pid, comm, StartID,
				// ancestry chain and session are the audit trail a wrong
				// reap is traced from (#7435 R6, review addition 2).
				log.WarningLog.Printf(
					"tmux session %s: admitting marked pid %d (%s, StartID %d, chain %s) to the vanished-session reap by generation identity — its executable identity places it inside the session's own %s program and it began no earlier than the recorded pane root (#7435 R9)",
					sanitizedName, process.PID, process.Comm, process.StartID,
					ancestryChainForLog(snap, process.PID), agentExe)
				owned = append(owned, process)
				continue
			}
			report(process.PID, fmt.Errorf(
				"captured live pid %d marks session %s but cannot be proven a member of its vanished pane cohort",
				process.PID, sanitizedName))
			continue
		}
		owned = append(owned, process)
	}
	if len(unplaceable) > 0 {
		inspectErrs = append(inspectErrs, blindGenerationRefusal(sanitizedName, unplaceable))
	}
	return owned, snap, errors.Join(inspectErrs...)
}

// ancestryChainForLog renders a candidate's parent chain inside the sweep's
// snapshot for the tier-2 admission WARNING — the evidence an operator needs
// to trace a wrong reap back to the process that justified it. Bounded like
// the classifier's own walk.
func ancestryChainForLog(snap map[int]proctree.Process, pid int) string {
	var b strings.Builder
	for i := 0; i < 8 && pid > 0; i++ {
		p, ok := snap[pid]
		if !ok {
			if i == 0 {
				fmt.Fprintf(&b, "%d(absent)", pid)
			}
			break
		}
		if i > 0 {
			b.WriteString(" -> ")
		}
		fmt.Fprintf(&b, "%d(%s)", p.PID, p.Comm)
		pid = p.PPID
	}
	return b.String()
}

// exeMatchesProgram reports whether a candidate's own executable identity is
// the session's resolved program binary — the same evidence sources the
// af-daemon classifier trusts: the kernel task name (p_comm, what Comm
// carries) or argv[0]'s basename when the argv scan can read it.
func exeMatchesProgram(p proctree.Process, agentExe string) bool {
	if p.Comm == agentExe {
		return true
	}
	if argv := proctree.Argv(p.PID); len(argv) > 0 && filepath.Base(argv[0]) == agentExe {
		return true
	}
	return false
}

// tier2ProgramAdmits is the executable-identity allowlist inside tier 2
// (#7435 R9). It admits a carrier only when the carrier's own identity is one
// of three proven shapes inside the eligible cohort:
//
//   - the carrier IS the session's program — comm or argv[0] names AgentExe
//     (the #3699 re-hosted claude, a process tab's own binary);
//   - the carrier descends from an admitted program through marked eligible
//     ancestors — helpers the re-hosted agent itself spawned;
//   - the carrier is the DIRECT marked parent of one — a one-hop host like
//     #3699's bg-pty-host, the agent binary's own re-host shim.
//
// The host hop is bounded at one level deliberately: a user's own
// multiplexer — screen, zellij, abduco, dtach — sits deeper and stays out
// the way a tmux server already does. Anything else is refused.
func tier2ProgramAdmits(snap map[int]proctree.Process, eligibleSet, exeMatch map[int]bool, p proctree.Process) bool {
	if len(exeMatch) == 0 {
		return false
	}
	if exeMatch[p.PID] {
		return true
	}
	// Descent from an admitted program: walking the marked eligible ancestors
	// up from p, the chain that reaches an exe match places p inside that
	// program's own re-host subtree. The first non-eligible ancestor closes
	// the cohort.
	for ancestor, i := p.PPID, 0; ancestor > 1 && i < 128; i++ {
		q, ok := snap[ancestor]
		if !ok || !eligibleSet[ancestor] {
			break
		}
		if exeMatch[ancestor] {
			return true
		}
		ancestor = q.PPID
	}
	// Host of an admitted program: p is the direct parent of an eligible
	// carrier whose own identity is the agent program.
	for pid := range exeMatch {
		if q, ok := snap[pid]; ok && q.PPID == p.PID {
			return true
		}
	}
	return false
}

// markedGeneration is one live process carrying the vanished session's
// AF_SESSION marker and this home's AF_HOME marker, plus a generation the sweep
// has no captured predecessor to place it against.
type markedGeneration struct {
	pid        int
	generation string
}

// blindGenerationRefusal is the message a blind vanished-session sweep leaves
// behind, and on that path it is the whole explanation an operator gets (#3706).
//
// It deliberately does NOT say "kill these pids". The callers that still reach
// the blind branch after #3700 — the background CleanupSessions sweep and
// unlocked internal cleanup — hold no claim on this session's lifecycle, so they
// cannot rule out that a marked process is a legitimate same-name REPLACEMENT
// created while the sweep was running (#3309). A kill hint here would be a
// footgun aimed at a healthy session, printed by a sweep that runs on its own.
//
// What it can do is hand over the comparison it is not allowed to make itself.
// The generation each pid carries is already in hand; the other half — what the
// LIVE session of that name carries — is one `show-environment` away, and the
// two verdicts that follow from it are unambiguous. So the message names the
// pids and their generations, gives that read, and states both outcomes.
//
// The suggested target is ExactTarget's `=name:` form rather than a bare name on
// purpose: tmux resolves a bare -t as a prefix match, so a pasted suggestion
// could report a DIFFERENT session's generation — in the one message whose
// answer decides what an operator kills.
func blindGenerationRefusal(sanitizedName string, marked []markedGeneration) error {
	named := make([]string, 0, len(marked))
	for _, process := range marked {
		named = append(named, fmt.Sprintf("pid %d (%s=%s)", process.pid, EnvMarkerGeneration, process.generation))
	}
	// sanitizedName preserves `%` by design, so it rides as an ARGUMENT and never
	// gets spliced into the format string (#1211) — including inside the
	// suggestions, which are built from pieces by shellsuggest.
	return fmt.Errorf("marked processes outlived vanished tmux session %s carrying a generation this sweep"+
		" has no captured predecessor to place: %s. Nothing was signalled and nothing was removed — this sweep holds"+
		" no claim on that session name, so a marked process may be a live same-name replacement rather than a"+
		" leftover of the vanished session, and it will not signal what it cannot attribute; it retries on every"+
		" pass. To tell the two apart, compare each generation above against the one the live session of that name"+
		" carries: %s. %s names the af session behind that tmux name, and each pid's own copy stays readable at"+
		" /proc/<pid>/environ. A generation that matches the live session belongs to it — leave that pid alone."+
		" A generation no live session claims is a leftover of the vanished session — that pid is safe to kill,"+
		" and the next sweep then proceeds.",
		sanitizedName, strings.Join(named, ", "),
		shellsuggest.Command("tmux", "show-environment", "-t", ExactTarget(sanitizedName), EnvMarkerGeneration),
		shellsuggest.Command("af", "sessions", "list", "--json"))
}

// orphanGenerationSet is the immutable cohort present when a vanished-session
// sweep begins. A pre-generation process is represented by legacy; every newer
// tmux launch carries a random value. Later marker refreshes may add helpers from
// this cohort, but never processes from a same-named replacement (#3309).
type orphanGenerationSet struct {
	legacy bool
	values map[string]bool
}

func orphanGenerations(candidates []proctree.Process, sanitizedName string) orphanGenerationSet {
	generations := orphanGenerationSet{values: make(map[string]bool)}
	for _, process := range candidates {
		same, err := proctree.SameIdentity(process)
		if err != nil {
			log.WarningLog.Printf("tmux session %s: cannot determine whether captured pid %d is still the same process while establishing the generation cohort: %v",
				sanitizedName, process.PID, err)
			continue
		}
		if !same {
			continue
		}
		environ, err := proctree.Environ(process.PID)
		if err != nil {
			log.WarningLog.Printf("tmux session %s: cannot read the environment of captured live pid %d while establishing the generation cohort: %v",
				sanitizedName, process.PID, err)
			continue
		}
		if session, ok := processEnvValue(environ, EnvMarkerSession); !ok || session != sanitizedName {
			continue
		}
		generation, ok := processEnvValue(environ, EnvMarkerGeneration)
		if !ok {
			generations.legacy = true
			continue
		}
		generations.values[generation] = true
	}
	return generations
}

func (g orphanGenerationSet) contains(environ []string) bool {
	generation, ok := processEnvValue(environ, EnvMarkerGeneration)
	if !ok {
		return g.legacy
	}
	return g.values[generation]
}

func (g orphanGenerationSet) empty() bool {
	return !g.legacy && len(g.values) == 0
}

// refreshOrphanCandidates closes the capture-to-marker TOCTOU window. A pane
// can launch another helper after the pre-marker snapshot but before the marker
// lookup observes that tmux removed the session. Once absence is authoritative,
// no pane remains to launch more work, so a fresh process-table snapshot can add
// every process still tied to the captured kernel sessions/trees plus any
// readable process carrying the exact AF_SESSION marker from the generation
// cohort fixed at sweep entry.
//
// Environment failures on unrelated processes are deliberately ignored: they
// are not evidence of membership. Failures on captured/SID/descendant
// candidates remain visible when markedOrphanProcesses validates the bounded
// set, preserving unknown without making hardened hosts globally unreadable.
func refreshOrphanCandidates(captured []proctree.Process, sanitizedName string, generations *orphanGenerationSet) ([]proctree.Process, error) {
	refreshed, snap, err := refreshCapturedAncestry(captured, sanitizedName)
	if err != nil {
		return captured, err
	}
	byPID := make(map[int]int, len(refreshed))
	for index, process := range refreshed {
		byPID[process.PID] = index
	}
	for _, process := range snap {
		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			continue
		}
		if session, ok := processEnvValue(environ, EnvMarkerSession); ok && session == sanitizedName &&
			(generations == nil || generations.contains(environ)) {
			refreshed = addOrReplaceOrphanCandidate(refreshed, byPID, process)
		}
	}
	return refreshed, nil
}
