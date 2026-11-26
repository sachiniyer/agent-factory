package tmux

import (
	"fmt"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/afcli"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// The PROTECTED SET: which pids in a captured tree a reap must never signal,
// and the ancestry classifier that places them. The verdict each protected
// pid carries is two-dimensional: every exclusion spares the process from
// signals, but only a POSITIVE placement — identity a bare claim cannot
// fake — may also keep the close it spared it from going Unknown (#7491 item
// 1; a same-uid process attesting by exec'ing a real af build is out of
// scope — the verdict grants it nothing it cannot already do, #7561).

// protectedVerdict is why a protected pid is excluded, and whether the
// placement is POSITIVE — proven foreign: excluded, named, and NOT a
// cleanup-blocking refusal — or merely BLOCKING: still never signalled, but
// unproven enough that a cleanup-authorizing verdict must refuse on it
// (#7491 item 1).
type protectedVerdict struct {
	reason   string
	positive bool
	// unverifiedDaemon is the pid a remedy must act on when this exclusion
	// stands on an af-daemon CLAIM — the claimant's own pid for a process
	// whose argv is itself the claim, or the claimant ancestor's for a
	// descendant spared by it. Zero for every other reason.
	unverifiedDaemon int
}

// protectedSet is the pid set a captured-tree reap must never signal, each
// mapped to its verdict, so the log can say WHY a member of a session's own
// captured tree was spared (#7435).
type protectedSet map[int]protectedVerdict

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
	// protectReasonSnapshotMissing is the nil-snapshot verdict (#7827): with
	// no process table, no captured process's ancestry can be walked at all,
	// so every member of the capture stands on the same BLOCKING terms one
	// unprovable ancestry earns. unverifiedDaemon stays zero — nothing was
	// observed to claim, so no claimant pid exists for a remedy to name.
	protectReasonSnapshotMissing = "a process whose daemon ancestry cannot be proven — the process-table snapshot is missing"
)

func protectReasonDaemonDescendant(daemonPID int) string {
	return fmt.Sprintf("a descendant of agent-factory daemon pid %d", daemonPID)
}

// The BLOCKING placements for a process whose argv is an af-daemon claim the
// kernel executable does not attest (#7491 item 1: `exec -a af … --daemon`
// can write argv, it cannot write the binary the kernel links), or whose
// af-daemon claim this build's own parse cannot decide (#7496 R1: an unknown
// flag, a positional that may name a subcommand the recognizer does not
// carry — a REAL daemon's launch shape in a newer build). On darwin a REAL
// daemon whose executable was replaced after exec reads exactly like a
// forged claim: its text vnode lost every name, and an unlinked vnode has no
// userspace opener (#7494 item 1). Every string says the argv CLAIMS a
// daemon — never that it IS one — names the claimant's pid, and the refusal
// it feeds carries the remedy that acts on THAT pid: stop it, or restart it
// under its own AF_HOME (#7496 R3 — `af daemon restart` acts on the
// invoker's home, not the claimant's). A marked process that IS the claimant
// gets its own wording: it does not "descend" from itself.
const (
	daemonUnverifiedClaimClause  = "argv claims an agent-factory daemon its kernel executable could not verify"
	daemonUndecidableClaimClause = "argv could not be decided by af's own parse — it may be an agent-factory daemon this build cannot verify"
)

// daemonClaimRemedy renders the remedy every refusal on an af-daemon
// claimant carries: it acts on the CLAIMANT'S pid — stop that pid, or
// restart it under its own AF_HOME — never a bare `af daemon restart`,
// which would bounce the operator's OWN daemon and leave the claimant
// blocking (#7496 R3).
func daemonClaimRemedy(claimantPID int) string {
	return fmt.Sprintf("stop pid %d, or restart it under its own AF_HOME, to unblock the close", claimantPID)
}

func protectReasonDaemonUnverifiedSelf() string {
	return "the claimant itself — its " + daemonUnverifiedClaimClause
}

func protectReasonDaemonUnverifiedDescendant(daemonPID int) string {
	return fmt.Sprintf("a descendant of pid %d, whose %s", daemonPID, daemonUnverifiedClaimClause)
}

func protectReasonDaemonUndecidableSelf() string {
	return "the claimant itself — its " + daemonUndecidableClaimClause
}

func protectReasonDaemonUndecidableDescendant(daemonPID int) string {
	return fmt.Sprintf("a descendant of pid %d, whose %s", daemonPID, daemonUndecidableClaimClause)
}

// unverifiedDaemonSurvivorClause renders the named-remedy tail of a
// survivors refusal: for every remaining process an af-daemon claim stands
// behind, one "; pid N is <reason> — <remedy>" clause. The reason is the
// exclusion's own — it says only what the argv CLAIMS, and words the
// claimant itself apart from a descendant (#7496 R3) — and the remedy acts
// on the claimant's pid, not the invoker's home (#7496 R2/R3).
func unverifiedDaemonSurvivorClause(remaining []proctree.Process, protected protectedSet) string {
	var b strings.Builder
	for _, p := range remaining {
		v, ok := protected[p.PID]
		if !ok || v.unverifiedDaemon == 0 {
			continue
		}
		fmt.Fprintf(&b, "; pid %d is %s — %s", p.PID, v.reason, daemonClaimRemedy(v.unverifiedDaemon))
	}
	return b.String()
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
//     inherited-marker child of the reaper is not this session's to kill.
//     Only the POSITIVE identification lifts this exclusion — a tmux-named
//     link the live read could not classify reports the boundary WITHOUT
//     ending the walk (#7848), so a snapshot's task name can no longer
//     strip this protection the way a proven server still can;
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
// A nil snapshot is the limit of that rule (#7827): with no process table at
// all, no captured process's ancestry can be walked, so a missing snapshot
// must never WIDEN what may be signalled. Every captured pid is excluded on
// the same blocking terms an unprovable ancestry earns — never signalled,
// still reported to cleanup-authorizing callers as alive and unattributable.
// Only an EMPTY capture narrows the set to the reaper's own pid, because then
// there was nothing to protect — and it says so without a kill-path warning.
//
// newExeAttestations is the sweep-memo constructor behind a var so the
// scope-threading test can count how many memos a sweep opens: one (#7509).
var newExeAttestations = proctree.NewExeAttestations

// Protection is decided only for captured processes, so in the common case —
// no captured process sits in the reaper's ancestry or subtree — no identity
// walk runs at all.
//
// att scopes kernel attestation to the sweep this set is computed for: a
// caller that owns one passes it through so every identity walk in the sweep
// shares the memo; nil allocates a set-local scope, which is all a
// single-call path needs (#7509).
func reapProtectedSet(snap map[int]proctree.Process, captured []proctree.Process, att *proctree.ExeAttestations) protectedSet {
	self := os.Getpid()
	protected := protectedSet{self: {reason: protectReasonSelf, positive: true}}
	if snap == nil {
		// #7827: a missing snapshot proves NOTHING about any captured
		// process's ancestry, so protection must never narrow to the
		// reaper's own pid below a non-empty capture — that is the fail-open
		// direction the asymmetric-error rule above exists to forbid. Every
		// captured pid stands protected on unproven terms: excluded from
		// signalling, still reported by the survivor paths so a
		// cleanup-authorizing caller refuses rather than reading "nothing
		// found". An empty capture is the common case — the session was
		// already gone, so the capture failed — and there is nothing to
		// protect: the failed capture is already reported upstream, and a
		// kill-path warning here would only be noise.
		if len(captured) > 0 {
			log.WarningLog.Printf("the process-table snapshot is missing; every captured process is excluded from signalling and stays reported rather than narrowing protection to the reaper's own pid")
			for _, p := range captured {
				if !protected.protects(p.PID) {
					protected[p.PID] = protectedVerdict{reason: protectReasonSnapshotMissing}
				}
			}
		}
		return protected
	}
	for pid := range selfAndAncestorProcesses(snap) {
		if pid != self {
			protected[pid] = protectedVerdict{reason: protectReasonAncestor, positive: true}
		}
	}
	if att == nil {
		att = newExeAttestations()
	}
	for _, p := range captured {
		if protected.protects(p.PID) {
			continue
		}
		if reason, positive, _, unverified := classifyCaptureAncestry(snap, p.PID, self, att); reason != "" {
			protected[p.PID] = protectedVerdict{reason: reason, positive: positive, unverifiedDaemon: unverified}
		}
	}
	return protected
}

// classifyCaptureAncestry walks pid's parent chain inside the capture's own
// snapshot and returns the reason the process is protected — or "" when it
// stays reapable — plus whether the walk met a tmux-boundary link before it
// ended. The first decisive verdict wins; the boundary flag is the UNION of
// every tmux-identified link the walk met along the way (#7848):
//
//   - the reaper ends the walk protected: the captured process is the
//     reaper's own descendant, excluded like an af daemon's subtree (#7435);
//   - a positively-identified agent-factory daemon ends the walk protected;
//   - a pid whose argv cannot be read ends the walk protected (unproven);
//   - a POSITIVELY identified tmux process ends the walk eligible at the
//     boundary, so a pane process under a server that itself descends from
//     the reaper still ends eligible at the server — the boundary verdict
//     only fails to protect the reaper's own children when no PROVEN server
//     lies between them and it;
//   - the top of the chain ends it eligible: an orphan whose parent died
//     reaches that end like any other chain.
//
// The boundary flag is reported but does not itself protect: on a capture
// path the server a pane still sits under is our own mid-kill and its
// members stay reapable. The VANISHED-session cohort reads it after
// membership proof (#7435, Codex 4597265500): a carrier whose SID, recorded
// pair, or proven descent places it in the cohort is a member even while a
// server parents it — a SIGHUP-ignoring pane root outlives its session
// exactly that way — and only a carrier proof could NOT place is refused
// behind the boundary, reported and never signalled.
//
// The two boundary recognitions act differently (#7848). A POSITIVE live
// read of tmux ends the walk at the boundary, as it always has. A process
// the snapshot's own kernel task name still identifies as tmux while the
// live process-table read could only answer TmuxUnknown (#7238) SETS the
// boundary flag WITHOUT ending the walk: TmuxUnknown is a third answer, not
// the negative, so only NotTmux licenses crossing — an unproven carrier
// whose chain ends above the link keeps the `case boundary:` refusal the
// live-server race needs (a stranger agent in another pane of the same live
// shared server, or a pane of a nested server this session's pane launched,
// must not mint eligibility through a read that could not answer). But the
// flag is ALL the non-positive read may take: a positive protection it was
// never the link's to lift still lands — a verdict above the link that is
// the reaper or a proven af daemon protects exactly as it did before the
// unreadable link learned to report the boundary (the asymmetric-error rule
// above: a false inclusion kills, so a protection a NON-positive read would
// lift is kept). A routinely-unknown NON-tmux link (darwin's routine
// refused-argv launchd ancestor) is still crossed: the snapshot task name
// does not name tmux there, so the walk proceeds exactly as #7433's
// protective direction intends.
func classifyCaptureAncestry(snap map[int]proctree.Process, pid, self int, att *proctree.ExeAttestations) (reason string, positive bool, tmuxBoundary bool, unverifiedDaemon int) {
	start := pid
	for range 128 {
		if pid <= 0 {
			return "", false, tmuxBoundary, 0
		}
		p, ok := snap[pid]
		if !ok {
			return "", false, tmuxBoundary, 0
		}
		identity := proctree.TmuxIdentityOf(pid)
		switch {
		case pid == self:
			return protectReasonSelfSubtree, true, tmuxBoundary, 0
		case identity == proctree.Tmux:
			// A POSITIVE live read ends the walk at the boundary —
			// unchanged. Only the unreadable link below accumulates into
			// the flag instead of ending here.
			return "", false, true, 0
		default:
			// A link the snapshot's task name still calls tmux while the
			// live read could not classify it SETS the boundary flag but
			// never ENDS the walk (#7848): the walk still consults this
			// link's own daemon class and every ancestor above, so the
			// verdict is the union — an unproven carrier keeps the
			// boundary refusal the flag reports, and a positive
			// protection the chain still holds (the reaper, a proven af
			// daemon) is not lifted by a read that could not answer.
			if identity == proctree.TmuxUnknown && proctree.IsTmuxTaskName(p.Comm) {
				tmuxBoundary = true
			}
			switch afDaemonClass(p, att) {
			case afDaemonIs:
				// VERIFIED daemon ancestry — the kernel attested the
				// executable is an af build (#7491 item 1): a positive
				// placement, excluded and named, that does not block.
				if pid == start {
					return protectReasonAFDaemon, true, tmuxBoundary, 0
				}
				return protectReasonDaemonDescendant(pid), true, tmuxBoundary, 0
			case afDaemonUnverified:
				// argv CLAIMS daemon ancestry the kernel executable will
				// not attest — `exec -a af … --daemon` writes argv, not
				// the binary. The claim still spares the process from
				// signals (safe direction) but can never authorize the
				// close it tried to veto around: BLOCKING like unproven.
				// The claimant's pid rides out so the refusal can NAME
				// the daemon and its remedy, not just the candidate
				// (#7494 item 1).
				if pid == start {
					return protectReasonDaemonUnverifiedSelf(), false, tmuxBoundary, pid
				}
				return protectReasonDaemonUnverifiedDescendant(pid), false, tmuxBoundary, pid
			case afDaemonUndecidable:
				// The argv is an af binary's, but this build's parse cannot
				// say whether it runs the daemon — an unknown flag or a
				// positional that may name a subcommand (#7496 R1). "Can't
				// tell" is BLOCKING like unverified, never Not: collapsing
				// it to Not is exactly how `af --daemon -- x` lost its
				// signal protection.
				if pid == start {
					return protectReasonDaemonUndecidableSelf(), false, tmuxBoundary, pid
				}
				return protectReasonDaemonUndecidableDescendant(pid), false, tmuxBoundary, pid
			case afDaemonUncertain:
				return protectReasonUnproven, false, tmuxBoundary, 0
			}
		}
		pid = p.PPID
	}
	return "", false, tmuxBoundary, 0
}

// reportableSurvivors filters a survivor set the way signalling filtered its
// kill set (#7435 R3): a process the protected set spares by PROOF — the
// reaper's own chain, a positively-identified af-daemon subtree — is not a
// leak and must not read as one forever. A process spared only because its
// daemon ancestry could not be proven is different: alive, unattributable,
// and never "nothing found" — it stays reported so a cleanup-authorizing
// caller still refuses. snap is the SAME table the survivor set was built
// from, so both verdicts classify against identical ancestry. att is the
// caller's sweep memo — the survivor check reclassifies the same claimants
// the sweep already attested, so it must share their scope rather than open
// a fresh one (#7509).
func reportableSurvivors(snap map[int]proctree.Process, survivors []proctree.Process, att *proctree.ExeAttestations) []proctree.Process {
	if len(survivors) == 0 {
		return survivors
	}
	protected := reapProtectedSet(snap, survivors, att)
	var reportable []proctree.Process
	for _, p := range survivors {
		if verdict, ok := protected[p.PID]; ok && verdict.positive {
			continue
		}
		reportable = append(reportable, p)
	}
	return reportable
}

type afDaemonVerdict int

const (
	afDaemonNot afDaemonVerdict = iota
	// afDaemonIs is the VERIFIED verdict: argv claims the daemon AND the
	// kernel-attested executable is an af build. Only this verdict may
	// authorize a cleanup — it is the one a bare claim cannot fake (#7491;
	// a same-uid exec of a real af build attests honestly, out of scope
	// per #7561).
	afDaemonIs
	// afDaemonUnverified is the CLAIMED verdict: argv names an af daemon
	// but the kernel executable does not attest it. Still never signalled
	// (the claim may be true for a binary af cannot read), but it blocks
	// every cleanup verdict it sits in front of.
	afDaemonUnverified
	// afDaemonUndecidable is the CAN'T-TELL verdict (#7496 R1): argv names
	// an af binary, but this build's own parse cannot decide whether it
	// runs the daemon — an unknown flag or a tail it cannot resolve. On
	// the reap path that is Unverified's semantics exactly — protected and
	// blocking, never Not — because collapsing "can't tell" to Not is how
	// `af --daemon -- x` lost its signal protection. Attestation cannot
	// settle it: the exe being an af build does not prove THIS argv ran
	// the daemon.
	afDaemonUndecidable
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
// att scopes kernel attestation to one sweep: callers holding a
// *proctree.ExeAttestations pay the attestation read once per claimant no
// matter how many candidates its ancestry walk meets (#7494 item 2). nil
// attests directly — the verdict is unchanged.
func afDaemonClass(p proctree.Process, att *proctree.ExeAttestations) afDaemonVerdict {
	if p.KernelThread {
		return afDaemonNot
	}
	if argv := proctree.Argv(p.PID); len(argv) > 0 {
		switch proctree.ArgvDaemonClaim(argv) {
		case afcli.DaemonClaimNo:
			return afDaemonNot
		case afcli.DaemonClaimYes:
			// argv CLAIMS the daemon — and argv is the one field the
			// process can write (`exec -a af … --daemon`, #7491 item 1).
			// Only kernel attestation promotes the claim to a placement
			// that may authorize cleanup: an exe whose Go build info
			// names this module is af at any path, and a foreign binary
			// named af is not.
			if att.AttestsAF(p) {
				return afDaemonIs
			}
			return afDaemonUnverified
		default:
			// DaemonClaimUndecidable: this build's parse cannot decide.
			// Never Not — protected and blocking like a claim the kernel
			// refused to attest (#7496 R1).
			return afDaemonUndecidable
		}
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
