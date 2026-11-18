package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
)

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
//   - AgentExe names the session's resolved AGENT executable (its basename —
//     "claude", "aider"): the only executable identity tier 2 will admit by
//     (#7435 R9/R14/R15). Empty means the session's program is not a
//     supported agent identity — a shell, an interpreter, an arbitrary
//     process tab — and tier 2 cannot run at all.
//   - RecordedPrograms maps each recorded pane root's pid to the agent
//     executable its RECORDED launch program resolved to (#7435 R16): the
//     pane root is af's own `sh` wrapper, so its Comm is never the program —
//     the program sits in the argv the kernel reports for that root, which
//     the capture read while tmux could still answer for it. The cohort
//     consults the ANCHOR root's entry when no caller supplied AgentExe.
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
	// AgentExe is the basename of the session's resolved AGENT executable —
	// "claude" for an agent tab. Tier 2 admits only a candidate whose own
	// executable identity matches it, plus that candidate's marked subtree
	// (#7435 R9/R14). Every other marker carrier without kernel evidence is
	// refused and reported, never signalled — including a refused carrier's
	// host. Empty means the caller could not name the program — or the
	// program is not a supported agent identity at all (#7435 R15) — and
	// tier 2 admits nothing.
	AgentExe string
	// RecordedPrograms maps a recorded pane root's pid to the agent
	// executable its recorded launch program resolved to — see the type
	// comment. The cohort fills AgentExe from the anchor root's entry when
	// the caller did not supply one (#7435 R16).
	RecordedPrograms map[int]string
	// OwnServer is the tmux server this session's pane belonged to,
	// recorded at spawn or capture (#7469). A carrier the attribution
	// channels place on a DIFFERENT live server is foreign — refused and
	// named, never signalled — while an unreadable or absent record keeps
	// every carrier undecidable: no record is never a foreign verdict.
	OwnServer ServerIdentity
	// owners is the once-per-sweep pty-index → server-pid map the
	// attribution consults, built lazily from the first snapshot that needs
	// it; callers share one instance across the sweep's passes (#7469).
	owners *serverPtyOwners
}

func (m cohortMembership) admits(snap map[int]proctree.Process, p proctree.Process, att *proctree.ExeAttestations) bool {
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
	// pid from minting membership it does not hold. The RECORDED pair vouches
	// the same way — it is the launch's own root, kernel proof of the same
	// strength — which is what lets a setsid'd child of a SIGHUP-ignoring
	// recorded root still converge (#7435, Codex 4597265500).
	//
	// The walk ends at a positively-identified tmux server: nothing below a
	// DIFFERENT server's pane inherits membership, even when a proven member
	// sits above the server in the chain — a pane that launched its own tmux
	// server does not make that server's panes ours (#7435, Codex 4597265500).
	// It ends the same way at a positively- or un-provably-identified af
	// daemon: processes below one are another af installation's work, never
	// this cohort's.
	for ancestor, i := p.PPID, 0; ancestor > 1 && i < 128; i++ {
		q, ok := snap[ancestor]
		if !ok {
			break
		}
		if startID, ok := m.Proven[ancestor]; ok && startID == q.StartID {
			return true
		}
		if startID, ok := m.Recorded[ancestor]; ok && startID == q.StartID {
			return true
		}
		// The stops apply only to ancestors that carry no proof of their
		// own — a proven member vouches first.
		if proctree.TmuxIdentityOf(ancestor) == proctree.Tmux || afDaemonClass(q, att) != afDaemonNot {
			break
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
// The error carries only the UNATTRIBUTABLE refusals: a carrier positively
// placed in a foreign subtree (the reaper's own outside a boundary, a proven
// af-daemon's) is proven NOT ours — refused and left alive like any other
// exclusion, but NAMED in the log rather than the refusal set, because a
// carrier the sweep CAN attribute out of the cohort is not what the
// cleanup-authorizing Unknown exists for (#7484 item 1).
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
// att scopes kernel attestation to the sweep this call belongs to: every
// claimant — the same daemon an ancestry walk, the marker gate and admits
// each meet — pays ONE attestation per (pid, StartID) here instead of one
// per candidate path that touches it (#7494 item 2). nil attests directly.
func markedOrphanProcesses(candidates []proctree.Process, sanitizedName, ownHome string, generations orphanGenerationSet, membership cohortMembership, reported map[int]bool, att *proctree.ExeAttestations) ([]proctree.Process, map[int]proctree.Process, error) {
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
	selfNS, _ := proctree.PIDNamespaceID()
	if membership.owners == nil {
		membership.owners = &serverPtyOwners{}
	}
	var unplaceable []markedGeneration
	var owned []proctree.Process
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
			// An unmarked candidate that argv-claims the af daemon but whose
			// kernel executable cannot verify the claim — or whose claim this
			// build's parse cannot decide (#7496 R1) — is named for what it
			// is with the remedy an operator can act on (#7494 item 1): a
			// REAL daemon whose exe was replaced after exec (darwin upgrade)
			// reads exactly like a forged claim, and stopping it — or
			// restarting it under its own AF_HOME — unblocks either way.
			switch class := afDaemonClass(process, att); class {
			case afDaemonUnverified:
				report(process.PID, fmt.Errorf(
					"captured live pid %d has no %s marker for vanished session %s — its %s; %s",
					process.PID, EnvMarkerSession, sanitizedName,
					daemonUnverifiedClaimClause, daemonClaimRemedy(process.PID)))
				continue
			case afDaemonUndecidable:
				report(process.PID, fmt.Errorf(
					"captured live pid %d has no %s marker for vanished session %s — its %s; %s",
					process.PID, EnvMarkerSession, sanitizedName,
					daemonUndecidableClaimClause, daemonClaimRemedy(process.PID)))
				continue
			}
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
		// THE DECISION TABLE (#7435, Codex 4597265500 + 4598089582). The
		// verdict for each candidate is:
		//
		//	admit = membership proof AND NOT foreign evidence about THIS
		//	process.
		//
		// PROOF is this session's kernel SID, the recorded (pid, StartID)
		// pair, a capture-proven pair, or descent from one of those members
		// (cohortMembership.admits). FOREIGN evidence is the reaper's own
		// chain or subtree, a positively-placed protected subtree (an af
		// daemon's), or a generation marker that differs — or is missing —
		// where the cohort gate requires one. A PARENT that is the shared
		// tmux server is NOT foreign evidence about the candidate: a proven
		// member behind it is admitted all the same, which is how this
		// session's own SIGHUP-ignoring pane root converges while the server
		// lives on.
		//
		// Both sides are computed before anything decides — proof, ancestry
		// placement and generation — and the switch below then decides ONCE,
		// because sequencing these as early continues has been wrong in both
		// directions: boundary-ahead-of-proof classified this session's own
		// recorded root as a foreign pane and left it alive and unreported
		// (4597265500), and proof-ahead-of-the-generation-gate admitted a
		// changed-generation descendant of a proven parent (4598089582).
		reason, positive, boundary, unverifiedDaemon := classifyCaptureAncestry(snap, process.PID, self, att)
		proven := membership.admits(snap, process, att)
		processGeneration, hasGeneration := processEnvValue(environ, EnvMarkerGeneration)
		// #7469: attribute the carrier to a tmux server through its TMUX env
		// and its held pty slaves. The verdict consults only the UNPROVEN
		// bucket: a kernel-proven member cannot be overridden by attribution
		// — the proof came from this session's own recorded pane evidence,
		// so a carrier carrying it was never the foreign server's work (R4).
		// Undecidable is today's verdict — it admits nothing and blocks
		// nothing on its own.
		serverVerdict := carrierServerVerdict(process, environ, membership.OwnServer, snap, membership.owners, selfNS)
		switch {
		case selfChain[process.PID]:
			// The reaper and its ancestors keep their refusal — reaching
			// here carrying this session's markers means the reaper is
			// inside the cohort it is judging.
			report(process.PID, fmt.Errorf("refusing to reap reset process or ancestor pid %d for vanished session %s",
				process.PID, sanitizedName))
		case reason != "" && positive:
			// A process POSITIVELY placed in another protected subtree —
			// the reaper's own descendant outside a tmux boundary, or a
			// proven af-daemon subtree — is proven NOT ours: protected
			// like signalling spares it, and NAMED, but named in the LOG
			// rather than the refusal set. Naming an exclusion and
			// blocking on it are separate verdicts: only a carrier the
			// sweep cannot ATTRIBUTE makes the close Unknown, and this
			// one is positively proven foreign (#7484 item 1 —
			// reportableSurvivors and reapSessionProcesses already apply
			// the same rule to their survivors and signalled set). A
			// throwaway daemon launched from a lane pane inherits the
			// lane's markers and would otherwise hold the worktree's
			// cleanup for the daemon's whole lifetime. Foreign evidence
			// still beats proof: a proven member here cannot exist — the
			// protected subtrees are disjoint from the pane tree.
			// reported[] is deliberately NOT marked — a pid that a later
			// pass re-verdicts unattributable must still produce its
			// blocking refusal. WARNING, not INFO: a Known close lets
			// teardown remove the worktree while this process — typically
			// with its cwd inside it — keeps running, so the line names
			// the pid, its placement, and that it was left alive (#7484
			// review).
			log.WarningLog.Printf("captured live pid %d marks session %s but its ancestry places it under %s — "+
				"proven foreign: refused, LEFT ALIVE, never signalled, and not blocking cleanup (#7484)",
				process.PID, sanitizedName, reason)
		case unverifiedDaemon > 0:
			// A cohort member whose ancestry ends at an UNVERIFIED af-daemon
			// claimant — or at an af binary this build's parse cannot decide
			// (#7496 R1) — blocks: the daemon may be a real af daemon whose
			// exe was replaced (#7494 item 1) or a foreign carrier mounting
			// the claim — and the refusal must be actionable: it carries the
			// ancestry walk's own reason, which names the claimant pid and
			// says only what the argv CLAIMS (#7496 R3), plus the remedy
			// that acts on that pid. The claim still spares both processes
			// from signals.
			report(process.PID, fmt.Errorf(
				"captured live pid %d marks session %s but is %s; %s",
				process.PID, sanitizedName, reason, daemonClaimRemedy(unverifiedDaemon)))
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
		case hasGeneration && !generations.values[processGeneration]:
			// A generation that DIFFERS is foreign evidence about this
			// process even when kernel proof exists: the marker checks
			// decide membership only for a process whose launch markers
			// still say it is ours — a descendant that rewrote its
			// generation is refused exactly as if it were a replacement
			// (#3309 review, Codex 4598089582). Marker scans are filtered
			// before they enter this bounded set. A different generation
			// already present here may be a descendant that changed its
			// environment after capture, so absence is not proved. It
			// must remain a refusal rather than being silently
			// reclassified as a replacement.
			report(process.PID, fmt.Errorf("captured live pid %d marks generation %s outside the vanished session %s generation cohort",
				process.PID, processGeneration, sanitizedName))
		case !hasGeneration && !generations.legacy:
			// A MISSING generation where the cohort gate requires one is
			// the same verdict as a differing one — foreign evidence about
			// the process itself, so proof cannot waive it (Codex
			// 4598089582).
			report(process.PID, fmt.Errorf("captured live pid %d marks session %s but has no %s generation marker",
				process.PID, sanitizedName, EnvMarkerGeneration))
		case proven:
			// Proof with NO foreign evidence: the candidate is a member
			// even while a tmux server parents it — a SIGHUP-ignoring pane
			// root whose session vanished under a live shared server is
			// exactly that shape, and the boundary rule below would have
			// classified it foreign (Codex 4597265500). Proof also outranks
			// the server attribution: the record it would be judged against
			// can only ever be older than the incarnation the kernel
			// evidence describes (#7469 R4).
			owned = append(owned, process)
		case boundary:
			// A positively-identified tmux process in the chain means this
			// unproven carrier lives under SOME server — a foreign pane
			// (#6846's global-env copy, a second server stamped with our
			// markers), or a member the evidence cannot reach. Either way
			// it is UNATTRIBUTABLE: refused, reported, and never signalled
			// — an excluded carrier is never left silently (#7435, Codex
			// 4597265500). An ATTACHED foreign carrier takes this arm — its
			// blocking refusal is unchanged (#7469 R4).
			report(process.PID, fmt.Errorf(
				"captured live pid %d marks session %s but sits behind a tmux server and cannot be proven a member — refusing to attribute it; left alive, never signalled",
				process.PID, sanitizedName))
		default:
			if serverVerdict == serverVerdictForeign {
				// #7469: a DETACHED carrier positively attributed to a tmux
				// server OTHER than the recorded one — a different socket
				// in its TMUX env, or a live foreign server holding its
				// pty master — while holding no kernel membership proof.
				// The invariant a vanished session must keep: no unproven
				// process descended from a foreign server is signalled.
				// This arm lives ONLY in the unproven bucket (R4): a
				// kernel-proven member is admitted above whatever the
				// attribution says, and an attached foreign carrier keeps
				// the boundary arm's blocking refusal. Like the
				// positive-ancestry arm, this is a NAMED WARNING, not a
				// blocking refusal — the carrier is attributed, it is
				// simply not ours, so teardown proceeds while it stays
				// alive (its markers inherited from the pane are the trap
				// #7469 closes).
				log.WarningLog.Printf("captured live pid %d marks session %s but its %s places it on a foreign tmux server — "+
					"proven foreign: refused, LEFT ALIVE, never signalled, and not blocking cleanup (#7469)",
					process.PID, sanitizedName, serverVerdictReason(environ, membership.OwnServer))
				continue
			}
			// No proof and no foreign evidence: a MEMBERSHIP verdict only —
			// the tier-2 allowlist needs the whole eligible cohort before
			// it can tell an agent program's host from a look-alike
			// (#7435 R9).
			eligible = append(eligible, process)
			eligibleClean[process.PID] = reason == ""
		}
	}
	// Environment is proof of CARRYING the session's launch markers, never
	// of MEMBERSHIP: a daemon or helper that setsid'd out of the pane keeps
	// inherited markers while joining a session this sweep has no claim on
	// (#7435 item 5 — tmux's global environment can expose them to unrelated
	// later panes too, #6846). Membership is decided in two tiers. Tier 1
	// is kernel evidence: a SID one of the proven pane roots led, a
	// (pid, StartID) pair recorded at spawn, capture inside the proven
	// live tree, or descent from one of those proven members. Tier 2
	// (#7435 R6/R9/R14/R15) is the re-hosted shape kernel membership cannot
	// see — but as an ALLOWLIST on the session's AGENT identity, not the
	// open "clean ancestry plus StartID" rule it replaced, which admitted
	// anything pty-host-shaped: a screen server and its window, an af
	// daemon's double-forked helper, the reaper's own escaped helper. A
	// carrier is admitted only by its OWN executable identity — comm or
	// argv[0], the same sources the af-daemon classifier uses — matching
	// the session's resolved agent executable, or by descending from such
	// a carrier through marked eligible ancestors. The #3699 chain
	// (claude daemon run → bg-pty-host → claude) converges on own identity
	// alone: the re-exec'd host itself answers argv[0] "claude" (#7435 R14
	// — the one-hop "host of a match" rule it replaced let a screen server
	// hosting a program-named window in, and the sweep then killed the
	// screen and every other window it held). Tier 2 only runs at all for
	// an AGENT tab's identity (#7435 R15): a shell or interpreter program —
	// sh, bash, zsh, fish, python, node — is a general-purpose runtime a
	// marked process could share with anything, so a shell or process tab
	// leaves AgentExe empty and gets kernel evidence only. A screen
	// server, a multiplexer window, or any other carrier failing the
	// identity match is REFUSED and reported, never signalled — the safe
	// direction for a process af cannot prove it owns, with its pids named
	// for the operator. Tier 2 still requires the R6 guards: a recorded
	// root, an eligible-clean ancestry verdict above (every positive
	// outside — tmux boundary, af-daemon subtree, reaper chain — already
	// excluded, and an unproven ancestor stays excluded), and StartID at or
	// after the root's. An unadmitted carrier is REPORTED, never silently
	// dropped: "nothing found" on this path authorizes worktree cleanup, so
	// an unattributable survivor must block it exactly like an unproven
	// home (#7435 R3, Codex 4593576722).
	agentExe := membership.AgentExe
	eligibleSet := make(map[int]bool, len(eligible))
	exeMatch := make(map[int]bool)
	for _, c := range eligible {
		eligibleSet[c.PID] = true
		// An identity ANCHOR is a carrier that passes EVERY tier-2 guard —
		// clean ancestry, a nonzero recorded root, and StartID no earlier
		// than it — not merely an exe match: a pre-root carrier matching the
		// program's identity is REFUSED below, and letting it anchor descent
		// would signal children of an agent this pane cannot attribute to
		// itself (Codex 4598849310). exeMatchesProgram's own agentExe==""
		// guard makes an empty allowlist anchor nothing.
		if eligibleClean[c.PID] &&
			membership.Root.PID > 0 && membership.Root.StartID != 0 &&
			c.StartID >= membership.Root.StartID &&
			exeMatchesProgram(c, agentExe) {
			exeMatch[c.PID] = true
		}
	}
	for _, process := range eligible {
		// Kernel membership was already consulted inside the candidate loop —
		// BEFORE the ancestry exclusions (#7435, Codex 4597265500) — so a
		// carrier reaching here has no kernel proof left; only the tier-2
		// allowlist can still admit it. Root.StartID must be nonzero too: a 0
		// anchor makes the no-earlier-than bound vacuous and would admit
		// every clean carrier (#7435 R6).
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
	if agentExe == "" {
		// An EMPTY allowlist matches nothing — without this a carrier
		// whose own Comm could not be read would satisfy "" == "" and
		// admit itself under a shell tab's shut tier 2 (#7435 R15).
		return false
	}
	if p.Comm == agentExe {
		return true
	}
	if argv := proctree.Argv(p.PID); len(argv) > 0 && filepath.Base(argv[0]) == agentExe {
		return true
	}
	return false
}

// recordedPaneProgramExe names the AGENT executable a pane root's recorded
// launch program resolves to, or "" when it cannot attribute one (#7435
// R15/R16). The pane root is af's own `sh` wrapper — its Comm answers "sh"
// for every launch, never the program — so the identity must come from the
// command the pane was launched with, read out of the root's live argv while
// tmux can still answer for it.
//
// Only a supported AGENT identity is returned. A shell, an interpreter, or
// any other general-purpose program is not an identity specific to the
// agent's re-host: under it, every marked carrier of that runtime would look
// like a member (#7435 R15). The predicate is the SAME one the launch's
// credential boundary trusts (sessionenv.AgentForCommand), so tier 2 opens
// exactly where af already stakes an account identity on the answer — and a
// shell or process tab resolves to "" and gets kernel evidence only.
func recordedPaneProgramExe(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	command := argv[0]
	if len(argv) >= 3 && argv[1] == "-c" && sessionenv.KnownShellName(filepath.Base(argv[0])) {
		// `shell -c <command>` — argv[2] is the recorded command directly.
		// The shell gate is what makes the read safe: `-c` takes a config
		// operand on codex and a program STRING on python, neither of which
		// is the pane's launch command (Codex 4599228958). Only behind a
		// known shell basename does argv[2] hold a shell command line.
		// Caught inside the shim's pre-exec window it is af's exec-shim
		// line instead, which carries the recorded command as its LAST
		// word (every `__af-…-exec` protocol ends that way).
		command = argv[2]
		if tokens, _ := SplitShellTokens(command); len(tokens) > 1 &&
			strings.HasPrefix(tokens[1], "__af-") {
			command = tokens[len(tokens)-1]
		}
	} else if len(argv) > 1 {
		// The root already exec'd through: argv[0] IS the program, and the
		// args keep wrappers like `env FOO=1 claude` classifiable. Each
		// element is quoted so an agent installed under a path with spaces
		// still parses as ONE word — a bare Join would split `/opt/My
		// Apps/claude` into words the parser cannot attribute (Codex
		// 4598849320).
		command = joinArgvPreservingWords(argv)
	}
	if sessionenv.AgentForCommand(command) == "" {
		return ""
	}
	exe, err := sessionenv.LocateProgramExecutable(command)
	if err != nil || exe.Name == "" {
		return ""
	}
	return filepath.Base(exe.Name)
}

// joinArgvPreservingWords renders a captured argv as one shell command line
// without losing element boundaries (Codex 4598849320). Every element is
// single-quoted — the only POSIX quoting literal for every byte — with an
// embedded quote re-closed and re-opened around a quoted literal. The result
// tokenizes back to exactly argv under the shell parsers AgentForCommand and
// LocateProgramExecutable use.
func joinArgvPreservingWords(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}

// tier2ProgramAdmits is the executable-identity allowlist inside tier 2
// (#7435 R9/R14). It admits a carrier only when its own identity is one of
// two proven shapes inside the eligible cohort:
//
//   - the carrier IS the session's program — comm or argv[0] names AgentExe
//     (the #3699 re-hosted claude, a process tab's own binary) AND passes
//     every tier-2 guard itself — clean ancestry, nonzero recorded root,
//     StartID no earlier than it. An exe match alone is NOT an anchor: a
//     pre-root carrier is refused, and must not vouch for descendants it
//     happens to hold (Codex 4598849310);
//   - the carrier descends from such an admitted anchor through marked
//     eligible ancestors — helpers the re-hosted agent itself spawned.
//
// A host is admitted by its OWN executable identity, never by its child's
// (#7435 R14): the earlier one-hop "direct marked parent" rule let a
// multiplexer — screen, zellij, abduco, dtach — in whenever a window of it
// ran the program, and the sweep then killed the screen and every other
// window it held. The rule was never needed for the real #3699 shape:
// `bg-pty-host` IS `claude` — the re-exec'd binary answers the program name
// in its own argv[0] — so the genuine host passes the same identity match
// its child does. Anything else is refused, and a refused process's host is
// never signalled through it.
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
