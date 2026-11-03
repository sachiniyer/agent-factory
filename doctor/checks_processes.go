package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file owns the process-table checks: whether doctor can read the process
// table at all (checkProcessInspection), how the processes it can read are
// classified as orphaned, escaped, or possible orphans (checkOrphanedProcesses),
// and the observations that keep every state those checks could not prove
// visible instead of counted as clean (processLeakObservations).
//
// Split out of checks.go for the file-length lint (#1145). The cut is along a
// real seam: everything here works from the run's one process snapshot
// (ctx.snap). What it shares with the other checks — describeProc, killFix,
// formatAge, the cpuFraction and withheldEnvCause test seams, and the
// processLeakMinAge and runaway* thresholds — stays in checks.go, shared with
// checkForeignDaemons and the other doctor files that use it.

// checkOrphanedProcesses finds processes carrying the AF_SESSION ancestry
// marker. Marker + dead session = verified orphan (killable with --fix);
// marker + live session but outside its pane trees = escaped (report-only);
// no marker but a TMUX env var pointing at a dead server = possible orphan
// (report-only — could belong to any tmux, not necessarily agent-factory).
// checkProcessInspection reports whether doctor can see the process table at
// all, and it must run BEFORE every check that reads ctx.snap.
//
// This is the check that exists because of what its absence did (#1939).
// proctree was /proc-only with no darwin backend, so on macOS every snapshot
// failed, Run discarded the error, and the process checks below returned early
// on a nil snap. Nothing was reported — not a warning, not a note — so `af
// doctor` printed a clean report on a machine it had never managed to inspect.
// Every macOS user who ran doctor to ask "are there orphaned processes?" was
// told "no" by a program that could not have known either way.
//
// So blindness gets its own row, and that row is a FAIL: it contributes to the
// exit code, because "I could not look" is a broken doctor, not a healthy
// machine. Do not soften this to a Warn, and do not let the process checks
// below start reporting emptiness as health.
func checkProcessInspection(ctx *scanContext, report *Report) {
	if ctx.snap != nil {
		report.Pass(sectionProcesses, "process-inspection",
			fmt.Sprintf("read the process table (%d processes)", len(ctx.snap)))
		return
	}
	detail := fmt.Sprintf("cannot read the process table on %s, so the orphan, escaped-process and "+
		"runaway-CPU checks below could not run: %v", runtime.GOOS, ctx.snapErr)
	remediation := "the checks that depend on the process table are UNKNOWN, not clean — " +
		"do not read this report as evidence that no processes leaked"
	if errors.Is(ctx.snapErr, proctree.ErrUnsupportedPlatform) {
		remediation = fmt.Sprintf("af has no process-table backend for %s; the process checks cannot run here "+
			"and this report says nothing about leaked processes", runtime.GOOS)
	}
	report.Fail(sectionProcesses, "process-inspection", detail, remediation)
}

func checkOrphanedProcesses(ctx *scanContext, report *Report) {
	// Silent only because checkProcessInspection already reported the
	// blindness as a FAIL. Never make this branch the whole story again.
	if ctx.snap == nil {
		return
	}
	// A name missing from `live` is what arms the kill below, so an
	// unavailable listing cannot be allowed to empty this map: every live
	// session would read as dead and every one of its processes as a verified
	// orphan. Silent for the same reason as the branch above — checkTmuxInspection
	// already reported the blindness as a FAIL (#2874).
	live, listErr := ctx.tmuxSessions()
	if listErr != nil {
		return
	}

	marked := map[string][]proctree.Process{}
	var possibles []proctree.Process
	var observations processLeakObservations
	for pid, p := range ctx.snap {
		if ctx.selfAncestors[pid] {
			continue
		}
		// EnvUnknown must NOT land in `marked`: that set is the killable
		// candidate pool, and a process we could not read is a process we
		// cannot claim is ours. It falls through to the report-only paths
		// below, which is the safe direction — we under-report an orphan
		// rather than act on one we never identified.
		name, nameStatus := proctree.LookupEnv(pid, tmux.EnvMarkerSession)
		if nameStatus == proctree.EnvFound && name != "" {
			if observations.oldEnough(ctx, p) {
				marked[name] = append(marked[name], p)
			}
			continue
		}
		if nameStatus == proctree.EnvUnknown {
			observations.noteWithheldEnv(pid)
		}
		if tmuxEnv, st := proctree.LookupEnv(pid, "TMUX"); st == proctree.EnvFound && tmuxServerDead(ctx, tmuxEnv) {
			if observations.oldEnough(ctx, p) {
				possibles = append(possibles, p)
			}
		}
	}

	for _, name := range sortedKeys(marked) {
		procs := marked[name]
		sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
		liveSockets, sessionLive := live[name]
		if sessionLive {
			// The pane tree comes from the EVIDENCE-BEARING capture — on EVERY
			// socket the name lives on, because names derive from repo path
			// and title, not AF home: a same-named session on another server
			// is a different session, and a member of that other copy compared
			// against only the first socket's tree would misclassify as
			// escaped. The best-effort SessionProcessTrees returns nil on any
			// list-panes failure, and reading that nil as a proven-empty tree
			// is the defect this arm exists to avoid: with inSession empty,
			// every marked process for this live session would render as an
			// escaped-process finding the check has no evidence for, because
			// the read that would prove membership never returned. A read that
			// cannot answer is reported once as blindness below (via
			// observations.blindSessions) rather than classified either way.
			var tree []proctree.Process
			var failedSockets int
			liveOnAnySocket := false
			for _, sock := range liveSockets {
				sub, paneErr := captureSessionProcessTrees(ctx, name, sock)
				tree = append(tree, sub...)
				switch {
				case paneErr == nil:
					liveOnAnySocket = true
				case errors.Is(paneErr, tmux.ErrSessionVanishedBeforeCapture):
					// This copy conclusively exited. The name can still be
					// live on another socket: not blindness, and by itself
					// not proof of death.
				default:
					failedSockets++
				}
			}
			// Build the captured-PID set from whatever the partial trees
			// returned (may be non-empty even for a socket that errored, e.g.
			// one pane vanished between list-panes and the process snapshot
			// while other panes' descendants were captured successfully).
			capturedPIDs := map[int]bool{}
			for _, p := range tree {
				capturedPIDs[p.PID] = true
			}
			if !liveOnAnySocket && failedSockets == 0 {
				// The session vanished on EVERY socket it was listed on —
				// conclusively gone: route its surviving candidates through
				// the dead-session (orphaned-process) classification below,
				// just like a session that was never in the live list. The
				// vanished sentinel is not a blindness event because the
				// absence is authoritative, not a failed read.
				sessionLive = false
			} else {
				if failedSockets > 0 {
					// Revalidate candidates before declaring blindness: if every
					// marked process for this session already exited (normal churn),
					// or is already verified by the partial trees, there is nothing
					// left to be blind about and no row is needed.
					var anyBlind bool
					for _, p := range procs {
						if observations.stillPresent(p) && !capturedPIDs[p.PID] {
							anyBlind = true
							break
						}
					}
					if anyBlind {
						observations.blindSessions = append(observations.blindSessions, name)
					}
					continue
				}
			}
			if sessionLive {
				for _, p := range procs {
					if capturedPIDs[p.PID] || !observations.stillPresent(p) {
						continue
					}
					// The same ownership classifier the dead-session arm below
					// uses decides what this marked process is (#6704): on the
					// shared default tmux server a session name is derived
					// from repo path + title and does not encode AF_HOME, so a
					// surviving process from another install can carry a name
					// that now matches one of OUR live sessions, and a
					// matching AF_HOME proves only that the SESSION was this
					// home's — never that the process carrying it is. Only a
					// verdict of ownershipThisInstall attributes the process
					// as an escapee of ours; every other verdict lands on the
					// row that says exactly what was proven. The escaped arm
					// remains report-only either way — no kill path is added.
					cls := ctx.markedProcessOwnership(p)
					if cls.verdict != ownershipThisInstall {
						report.addAdvisoryFinding(nonOwningFinding(p, name, cls, true))
						continue
					}
					report.addAdvisoryFinding(Finding{
						Check: "escaped-process",
						Detail: fmt.Sprintf("%s escaped the pane tree of live session %s "+
							"(left alone while the session is alive)", describeProc(p), name),
					})
				}
				continue
			}
			// sessionLive was cleared by ErrSessionVanishedBeforeCapture:
			// fall through to the orphaned-process classification below.
		}
		for _, p := range procs {
			if !observations.stillPresent(p) {
				continue
			}
			// A kill requires a PROVEN home match, not just a dead-looking
			// session: another agent-factory home (e.g. a play-test sandbox
			// on a private `tmux -L` server) has sessions that are invisible
			// to this server's live list, so its perfectly healthy processes
			// would otherwise masquerade as verified orphans here. Foreign
			// or missing AF_HOME downgrades to report-only.
			// Only EnvFound may arm the kill. EnvUnknown (a denied or
			// redacted read) falls to the default arm — report-only —
			// because "I could not read its home" is not "its home is
			// mine".
			//
			// The proof standard mirrors the live-session escaped arm
			// above. A relative AGENT_FACTORY_HOME is stamped relative to the
			// launching af process's frame (session/tmux.afHomeDir leaves it
			// unchanged), which doctor cannot recover, so two distinct homes —
			// each launched from a different working directory with
			// AF_HOME=.af — both stamp ".af" and would collapse onto the same
			// string here. Treating that as a proven match arms killFix for a
			// process that may belong to another install, so a relative marker
			// is UNPROVEN ownership, not a confirmed match: it never arms the
			// kill. An absolute marker is compared by filesystem identity
			// (os.SameFile via sameHome) rather than filepath.Clean: Clean is
			// lexical and leaves a symlinked AGENT_FACTORY_HOME — or macOS
			// /var vs /private/var, or a case-differing spelling on a
			// case-insensitive volume — comparing unequal to the same home,
			// which would report a genuine same-home orphan as "belongs to
			// another agent-factory home" and leave it unfixed.
			//
			// A kill ALSO requires a corroborated session listing: the dead
			// session this arm infers must be dead on the server that ACTUALLY
			// hosted it, and every socket generation the daemon has used must
			// be accounted for — absence from the union listing is evidence of
			// death only under both. sessionDeathProof asks the process's own
			// TMUX env for the server its session lived on (a live answer there
			// reroutes to the escaped classification), and falls back to the
			// record only when the record can be proven current — the daemon
			// generation that stamped it still serves this home, or a stamped
			// record is the last word a dead daemon left (#6339). Anything else
			// is reported, not killed: a live session on a socket the record
			// does not account for looks exactly like a dead one here.
			// One classifier produces the verdict, and this arm only maps it
			// to a row (#6704): markedProcessOwnership decides ownership from
			// the marker, the process's OWN environment resolved in its own
			// frame, and a fully verified ancestor chain — the same evidence
			// the live arm above acts on. A kill requires the proven
			// same-install verdict: every other verdict — including any a
			// future classifier adds — is refused by default here and lands
			// on a non-attributing advisory row. There is no verdict switch
			// to forget to extend; only ownershipThisInstall reaches the
			// death proof.
			cls := ctx.markedProcessOwnership(p)
			if cls.verdict != ownershipThisInstall {
				report.addAdvisoryFinding(nonOwningFinding(p, name, cls, false))
				continue
			}
			verdict, proofSocket := deathProof(ctx, p, name)
			switch verdict {
			case deathLiveElsewhere:
				// The session is absent from the union listing but live on
				// the socket p's own TMUX env names — the stale-record or
				// adopted-server shape #6339 pins. Classify it the way the
				// live arm would: inside the pane tree, silent; outside it,
				// escaped (report-only — never armed). A capture that
				// cannot answer is neither — a read that cannot answer is
				// not evidence.
				tree, paneErr := captureSessionProcessTrees(ctx, name, proofSocket)
				switch {
				case errors.Is(paneErr, tmux.ErrSessionVanishedBeforeCapture):
					// The session vanished between listing and capture —
					// the proof's live answer is already stale. Leave the
					// process for the next run rather than classify on a
					// racing world.
					continue
				case paneErr != nil:
					report.addAdvisoryFinding(Finding{
						Check: "orphaned-process",
						Detail: fmt.Sprintf("%s marks session %s (home %s), which is live on "+
							"tmux socket %s, but its pane tree could not be read (%v) — "+
							"it is reported, not killed", describeProc(p), name, cls.marker, proofSocket, paneErr),
						Remediation: "inspect the session's pane tree on the tmux socket named above — " +
							"the process is report-only",
					})
					continue
				}
				inTree := false
				for _, tp := range tree {
					if tp.PID == p.PID {
						inTree = true
						break
					}
				}
				if inTree {
					continue
				}
				report.addAdvisoryFinding(Finding{
					Check: "escaped-process",
					Detail: fmt.Sprintf("%s escaped the pane tree of live session %s "+
						"(left alone while the session is alive)", describeProc(p), name),
				})
				continue
			case deathProven:
				report.addActionableFinding(Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s was spawned by dead session %s (home %s)",
						describeProc(p), name, cls.marker),
					FixAction: fmt.Sprintf("kill pid %d", p.PID),
					fix:       killFix(ctx, p),
				})
				continue
			default:
				report.addAdvisoryFinding(Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s marks session %s (home %s) but doctor cannot "+
						"corroborate the session's death on the server that hosts it — the "+
						"daemon's tmux server record is absent, cannot be pinned to a socket, "+
						"or cannot be proven current, and a live session on a socket the "+
						"record does not account for would look the same here; it is "+
						"reported, not killed",
						describeProc(p), name, cls.marker),
					Remediation: ctx.unconfirmedDeathRemedy(p),
				})
				continue
			}
		}
	}

	// Sort by CPU so a core-burning `yes` outranks fifty idle shells, and
	// cap the listing — on a long-lived dev box this class is numerous and
	// mostly idle, and it is report-only by definition.
	type ranked struct {
		p       proctree.Process
		frac    float64
		unknown bool
	}
	rankedPossibles := make([]ranked, 0, len(possibles))
	for _, p := range possibles {
		if !observations.stillPresent(p) {
			continue
		}
		frac, _, err := cpuFraction(p)
		rankedPossibles = append(rankedPossibles, ranked{p, frac, errors.Is(err, proctree.ErrCPUUnknown)})
	}
	sort.Slice(rankedPossibles, func(i, j int) bool {
		if rankedPossibles[i].frac != rankedPossibles[j].frac {
			return rankedPossibles[i].frac > rankedPossibles[j].frac
		}
		return rankedPossibles[i].p.PID < rankedPossibles[j].p.PID
	})
	const maxPossibleOrphans = 15
	for i, r := range rankedPossibles {
		if i == maxPossibleOrphans {
			// The descending sort above makes the overflow tail the coldest
			// portion of the set, but "coldest of N" is not "idle" — a set at
			// 33%-78% CPU has a tail at ≤33%, still a third of a core. The
			// per-process frac the loop just captured on `ranked` is the
			// measured data; describe the hidden remainder from its max and
			// fall back to the idle wording only when the data supports it.
			tail := rankedPossibles[maxPossibleOrphans:]
			maxFrac := 0.0
			unknown := 0
			for _, t := range tail {
				if t.unknown {
					// CPUFraction returns ErrCPUUnknown (not a zero
					// fraction) when it could not read the counter — e.g.
					// a possible owned by another UID or hidden by a
					// restricted process table. A 0 frac here is an
					// unmeasured process, not an idle one; track it so the
					// summary does not claim "idle" about CPU it never saw.
					unknown++
					continue
				}
				if t.frac > maxFrac {
					maxFrac = t.frac
				}
			}
			desc := "all idle or near-idle"
			switch {
			case unknown == len(tail):
				desc = "CPU unmeasured in the remainder"
			case unknown > 0:
				desc = fmt.Sprintf("up to %.0f%% CPU in the remainder (%d unmeasured)", maxFrac*100, unknown)
			case maxFrac >= runawayCPUFraction:
				desc = fmt.Sprintf("some at up to %.0f%% CPU", maxFrac*100)
			case maxFrac > 0:
				desc = fmt.Sprintf("up to %.0f%% CPU in the remainder", maxFrac*100)
			}
			report.addAdvisoryFinding(Finding{
				Check: "possible-orphan",
				Detail: fmt.Sprintf("… and %d more processes of dead tmux servers (%s; "+
					"none carry an agent-factory marker, so none are killed)", len(tail), desc),
			})
			break
		}
		report.addAdvisoryFinding(Finding{
			Check: "possible-orphan",
			Detail: fmt.Sprintf("%s belongs to a dead tmux server (no agent-factory marker — "+
				"cannot verify ownership, so it is reported, not killed)", describeProc(r.p)),
		})
	}
	observations.report(report)
}

// ownershipVerdict is what doctor can prove about which install a marked
// process belongs to (#6578, #6704). ONE classifier produces it for both
// process arms; the arms only map verdicts to report keys. The AF_HOME marker
// proves the session was this home's; it never proves the process carrying it
// is, because a session's environment is inherited by every process its
// children spawn — including a second install launched from inside it.
type ownershipVerdict int

const (
	// ownershipUnprovenUnknown is the zero value: a verdict nobody produced.
	// It must never read as proven-ours — the kill arm's only way through is
	// an explicitly produced ownershipThisInstall (#6716).
	ownershipUnprovenUnknown ownershipVerdict = iota
	// ownershipThisInstall is the only verdict that may arm a kill — and the
	// only verdict that may attribute an escapee to this install: a matching
	// marker AND the process's own environment resolving to this home AND no
	// ancestor declaring a different one.
	ownershipThisInstall
	// ownershipForeignEnv means the process's own environment resolves to a
	// different install's home — its matching AF_HOME marker was inherited.
	ownershipForeignEnv
	// ownershipForeignAncestor means the process's own home is this
	// install's (or is unresolvable) but an ancestor declares a different
	// install's home — it runs under another install's tmux server, daemon
	// or helper, which an env scrub cannot erase.
	ownershipForeignAncestor
	// ownershipForeignMarker means the AF_HOME marker itself is an absolute
	// path naming a different install.
	ownershipForeignMarker
	// ownershipUnprovenNoMarker means no readable AF_HOME marker names any
	// install at all — nothing is proven about which home owns the process.
	ownershipUnprovenNoMarker
	// ownershipUnprovenRelativeMarker means the AF_HOME marker is a relative
	// path — relative to its launching af process's frame, which doctor
	// cannot recover — so it names no identifiable install.
	ownershipUnprovenRelativeMarker
	// ownershipUnprovenRelativeOwnHome means the marker matches this
	// install but the process's own AGENT_FACTORY_HOME is a relative path —
	// resolved against its working directory NOW, not the frame it was set
	// in, which cannot be recovered. af never stamps an absolute marker
	// beside a relative own home, so refusing it costs no kill (#6716).
	ownershipUnprovenRelativeOwnHome
	// ownershipUnprovenOwnEnv means the marker matches this install but the
	// process's own environment establishes no home — it answered, and
	// nothing it says resolves to an install.
	ownershipUnprovenOwnEnv
	// ownershipUnprovenOwnEnvUnreadable means the marker matches this
	// install but the process's own environment could not be read at all.
	ownershipUnprovenOwnEnvUnreadable
	// ownershipUnprovenDerivedMissing means the marker matches this install
	// and the process's own environment derives $HOME/.agent-factory — a
	// directory that does not exist, which is inference, not evidence of an
	// install (#6704).
	ownershipUnprovenDerivedMissing
	// ownershipUnprovenAncestor means the ancestor chain could not be fully
	// verified — an ancestor was missing from the snapshot, no longer names
	// the process it recorded, its environment could not be read, or the
	// chain looped or outran the bound. An ancestor whose home cannot be
	// ruled out is not proven non-foreign.
	ownershipUnprovenAncestor
)

// processOwnership carries the verdict plus the evidence a report row may
// cite: the marker as read, the proven foreign home, the ancestor a verdict
// names, the derived default home that does not exist, and the specific way
// an ancestor chain failed verification.
type processOwnership struct {
	verdict     ownershipVerdict
	marker      string
	foreignHome string
	ancestorPID int
	derivedHome string
	ownHome     string // the process's own home spelling a verdict cites
	cause       ancestorUnproven
}

// markedProcessOwnership resolves which install a marked process belongs to.
// Layer one is the AF_HOME marker: absent or unreadable proves nothing;
// relative names no identifiable install; an absolute marker naming a
// different home is a positive foreign claim. Only a marker matching this
// install proceeds — and a matching marker proves the SESSION, never the
// process, so layer two resolves the process's own home in its own frame
// (daemonProcessHome — the same rule the daemon checks act on) and layer
// three consults its ancestor chain for the shapes its environment cannot
// answer. A HOME-derived default home that does not exist is inference, not
// evidence — unproven, never foreign and never ours (#6704).
func (ctx *scanContext) markedProcessOwnership(p proctree.Process) processOwnership {
	marker, markerStatus := proctree.LookupEnv(p.PID, tmux.EnvMarkerHome)
	if markerStatus != proctree.EnvFound {
		return processOwnership{verdict: ownershipUnprovenNoMarker}
	}
	if !filepath.IsAbs(marker) {
		return processOwnership{verdict: ownershipUnprovenRelativeMarker, marker: marker}
	}
	if !sameHome(marker, ctx.opts.ConfigDir) {
		return processOwnership{verdict: ownershipForeignMarker, marker: marker, foreignHome: marker}
	}
	// A process's own RELATIVE AGENT_FACTORY_HOME is resolved against its
	// working directory — the directory it happens to be in NOW, not the
	// frame it was set in, which cannot be recovered. af never stamps an
	// absolute AF_HOME marker beside a relative own home, so refusing the
	// claim costs no legitimate kill (#6716). Tilde spellings are not
	// relative: resolveHomeIn anchors them at the process's own HOME.
	if raw, found, err := daemonProcessEnvLookup(p.PID, "AGENT_FACTORY_HOME"); err == nil &&
		found && raw != "" && !filepath.IsAbs(raw) && !strings.HasPrefix(raw, "~") {
		return processOwnership{verdict: ownershipUnprovenRelativeOwnHome, marker: marker, ownHome: raw}
	}
	ownHome, known := daemonProcessHome(p.PID)
	if known && !sameHome(ownHome, ctx.opts.ConfigDir) {
		// A foreign-home claim must name a real install. An explicitly
		// declared AGENT_FACTORY_HOME may outlive its directory — still a
		// claim. The HOME-derived default is different: it is doctor's
		// inference, not the process's claim, so a derived home that does
		// not exist on disk proves nothing (#6704).
		if !dirExists(ownHome) && derivedDefaultHome(p.PID) == ownHome {
			return processOwnership{verdict: ownershipUnprovenDerivedMissing, marker: marker, derivedHome: ownHome}
		}
		return processOwnership{verdict: ownershipForeignEnv, marker: marker, foreignHome: ownHome}
	}
	switch res := ancestorWalk(ctx, p); res.verdict {
	case ancestryForeign:
		return processOwnership{verdict: ownershipForeignAncestor, marker: marker,
			foreignHome: res.home, ancestorPID: res.pid}
	case ancestryUnproven:
		return processOwnership{verdict: ownershipUnprovenAncestor, marker: marker,
			ancestorPID: res.pid, cause: res.cause}
	case ancestryClean:
		// The only verdict that may leave ownership to the environment layers.
	default:
		// ancestryUnknown or a future verdict nobody handled: anything other
		// than clean is not a fully verified chain, so the process is not
		// proven ours (#6716).
		return processOwnership{verdict: ownershipUnprovenAncestor, marker: marker,
			ancestorPID: res.pid, cause: res.cause}
	}
	if !known {
		if _, _, err := daemonProcessEnvLookup(p.PID, "HOME"); err != nil {
			return processOwnership{verdict: ownershipUnprovenOwnEnvUnreadable, marker: marker}
		}
		return processOwnership{verdict: ownershipUnprovenOwnEnv, marker: marker}
	}
	return processOwnership{verdict: ownershipThisInstall, marker: marker}
}

// derivedDefaultHome reports the home the process's own environment would
// derive when it declares no AGENT_FACTORY_HOME — $HOME/.agent-factory
// resolved in its own frame — or "" when the process makes an explicit
// declaration or the derivation cannot be computed. It exists so the
// classifier can tell the process's own claim apart from the default
// daemonProcessHome applies on its behalf.
func derivedDefaultHome(pid int) string {
	raw, found, err := daemonProcessEnvLookup(pid, "AGENT_FACTORY_HOME")
	if err != nil || (found && raw != "") {
		return ""
	}
	home, _, err := daemonProcessEnvLookup(pid, "HOME")
	if err != nil || home == "" {
		return ""
	}
	cwd, _ := daemonProcessCwd(pid)
	derived, _ := resolveHomeIn(filepath.Join(home, ".agent-factory"), home, cwd)
	return derived
}

// dirExists reports whether dir names a directory that exists right now —
// the difference between a claimed home and a derived one (a derived home
// that does not exist is no evidence of an install, #6704).
func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// nonOwningFinding renders a marked process the classifier did not prove is
// this install's onto the row that claims exactly what was proven. Both arms
// map the SAME verdicts to the SAME keys — the escaped arm only frames them
// as "carries live session X's name" where the kill arm says "marks dead
// session X". Every row is report-only: the escaped arm never kills, and the
// kill arm refuses by default on every verdict short of ownershipThisInstall.
func nonOwningFinding(p proctree.Process, session string, cls processOwnership, live bool) Finding {
	what := fmt.Sprintf("marks dead session %s", session)
	so := "it is reported, not killed"
	if live {
		what = fmt.Sprintf("carries live session %s's name", session)
		so = "it is not attributed to this install"
	}
	d := describeProc(p)
	const foreignRemedy = "run `af doctor` with the owning AGENT_FACTORY_HOME active, or inspect manually"
	switch cls.verdict {
	case ownershipForeignMarker:
		foreignWhy := "its session may be live on that install's private tmux server, so it is reported, not killed"
		if live {
			foreignWhy = "it is not attributed to this install"
		}
		return Finding{Check: "foreign-home-process",
			Detail: fmt.Sprintf("%s %s but belongs to another agent-factory home (%s) — %s",
				d, what, cls.foreignHome, foreignWhy),
			Remediation: foreignRemedy}
	case ownershipForeignEnv:
		return Finding{Check: "foreign-home-process",
			Detail: fmt.Sprintf("%s %s but belongs to another agent-factory home (%s) — "+
				"the matching AF_HOME marker was inherited from a session of this install, so %s",
				d, what, cls.foreignHome, so),
			Remediation: foreignRemedy}
	case ownershipForeignAncestor:
		return Finding{Check: "foreign-home-process",
			Detail: fmt.Sprintf("%s %s but runs under pid %d, which declares another "+
				"agent-factory home (%s) — the matching marker was inherited, so %s",
				d, what, cls.ancestorPID, cls.foreignHome, so),
			Remediation: foreignRemedy}
	case ownershipUnprovenNoMarker:
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s but carries no readable AF_HOME marker — ownership is "+
				"unproven, so %s", d, what, so),
			Remediation: "inspect the process manually — no readable AF_HOME marker names an install"}
	case ownershipUnprovenRelativeMarker:
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s but its AF_HOME marker (%s) is a relative path that "+
				"cannot be resolved to a specific agent-factory home — ownership is "+
				"unproven, so %s", d, what, cls.marker, so),
			Remediation: "inspect the process's launching frame to identify which install its relative AF_HOME names"}
	case ownershipUnprovenRelativeOwnHome:
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s and its AF_HOME matches this install, but its own "+
				"AGENT_FACTORY_HOME (%s) is a relative path — resolved against a working "+
				"directory that may have changed since it was set, so ownership is "+
				"unproven, so %s", d, what, cls.ownHome, so),
			Remediation: "inspect the process's environment manually to identify its agent-factory home"}
	case ownershipUnprovenOwnEnvUnreadable:
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s and its AF_HOME matches this install, but its own "+
				"environment could not be read — ownership is unproven, so %s", d, what, so),
			Remediation: "inspect the process manually — its environment could not be read"}
	case ownershipUnprovenDerivedMissing:
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s and its AF_HOME matches this install, but the "+
				"agent-factory home its environment would derive (%s) does not exist — "+
				"ownership is unproven, so %s", d, what, cls.derivedHome, so),
			Remediation: "inspect the process manually — the home its environment would derive does not exist"}
	case ownershipUnprovenAncestor:
		// Transient causes (missing / recycled / unreadable) may resolve on a
		// rerun; structural ones (a cycle, the hop bound) truncate the proof
		// every time, so the remedy and the framing differ (#6716). The
		// cycle's loop-back point is described as such — it can be the
		// process's own pid, which is not "the ancestor" in any useful sense.
		basis := fmt.Sprintf("ancestor pid %d could not be verified — %s",
			cls.ancestorPID, cls.cause.describe())
		remedy := "rerun `af doctor` — the process table may have raced this scan; " +
			"inspect the process manually if it persists"
		switch cls.cause {
		case ancestorGapCycle:
			basis = fmt.Sprintf("its ancestor chain loops back onto pid %d", cls.ancestorPID)
			remedy = "inspect the process's ancestor chain manually — it could not be fully verified"
		case ancestorGapDepth:
			basis = fmt.Sprintf("its ancestor chain exceeds the %d-hop verification bound at pid %d",
				ancestorHopLimit, cls.ancestorPID)
			remedy = "inspect the process's ancestor chain manually — it could not be fully verified"
		}
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s and its AF_HOME matches this install, but %s — "+
				"ownership is unproven, so %s", d, what, basis, so),
			Remediation: remedy}
	case ownershipUnprovenOwnEnv:
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s and its AF_HOME matches this install, but its own "+
				"environment establishes no agent-factory home — ownership is unproven, "+
				"so %s", d, what, so),
			Remediation: "inspect the process's environment manually to identify its agent-factory home"}
	default:
		return Finding{Check: "foreign-home-unresolved",
			Detail: fmt.Sprintf("%s %s but which install owns it could not be proven — %s",
				d, what, so),
			Remediation: "inspect the process manually to identify its owning install"}
	}
}

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

// processLeakObservations keeps every unprovable state visible without mixing
// it into the leak counts. A vanished process is normal churn and needs no row;
// missing age or an identity-read failure means the check was blind; a live
// session whose pane tree could not be read is blind to membership rather than
// proven empty (added so a failed list-panes no longer renders as a row of
// false escaped-process findings for that session's ordinary in-pane children).
type processLeakObservations struct {
	unknownAge      int
	unknownIdentity int
	blindSessions   []string
	// envWithheld counts processes whose environment the kernel declined to
	// serve, so none of the classifications below could read their markers
	// (#3584). envWithheldCause is the first attributed cause.
	envWithheld      int
	envWithheldCause string
}

// noteWithheldEnv records pid when its unreadable environment has a cause the
// platform can name. An unattributed unknown (a foreign uid, a process that
// exited) is ordinary and stays uncounted.
func (o *processLeakObservations) noteWithheldEnv(pid int) {
	cause, ok := withheldEnvCause(pid)
	if !ok {
		return
	}
	if o.envWithheld == 0 {
		o.envWithheldCause = cause
	}
	o.envWithheld++
}

func (o *processLeakObservations) oldEnough(ctx *scanContext, p proctree.Process) bool {
	if p.StartedAt.IsZero() {
		o.unknownAge++
		return false
	}
	return ctx.snapAt.Sub(p.StartedAt) >= ctx.opts.minProcessLeakAge
}

func (o *processLeakObservations) stillPresent(p proctree.Process) bool {
	same, err := proctree.SameIdentity(p)
	if err != nil {
		o.unknownIdentity++
		return false
	}
	return same
}

func (o processLeakObservations) report(report *Report) {
	var blind []string
	if o.unknownAge > 0 {
		blind = append(blind, fmt.Sprintf("could not determine the age of %s",
			plural(o.unknownAge, "candidate process", "candidate processes")))
	}
	if o.unknownIdentity > 0 {
		blind = append(blind, fmt.Sprintf("could not revalidate the identity of %s",
			plural(o.unknownIdentity, "candidate process", "candidate processes")))
	}
	if len(blind) > 0 {
		report.Warn(sectionProcesses, "process-leak-inspection",
			strings.Join(blind, " and ")+"; they are omitted from the escaped, orphaned, and possible-orphan counts",
			"those candidates are UNKNOWN, not proven leaks; rerun doctor or inspect them manually", false)
	}
	// On a SIP-enabled Mac the kernel withholds the environment of every Apple
	// system binary, /bin/zsh included, so a leaked shell carries markers doctor
	// cannot read and lands in none of the counts (#3584). Say so once rather
	// than let the missing findings read as a clean machine. A Warn, like the
	// other partial-blindness rows here: the process table was read, and the
	// only remedy (turning SIP off) is not one to suggest.
	if o.envWithheld > 0 {
		report.Warn(sectionProcesses, "process-attribution",
			fmt.Sprintf("the kernel withheld the environment of %s because %s, so af cannot read "+
				"their session markers; a leaked process of that kind is not counted as escaped, "+
				"orphaned, or a possible orphan", plural(o.envWithheld, "process", "processes"), o.envWithheldCause),
			"missing process findings on this host are not proof that nothing leaked; af leaves "+
				"processes it cannot attribute alone", false)
	}
	// A live session whose pane tree could not be read is not a proven-empty
	// tree: the escaped-process arm's membership check had no answer, so it
	// declined to classify that session's marked processes either way. Report
	// it once per session as blindness rather than letting it render as a row
	// of false escaped-process findings (the way an unreadable process table or
	// session list already gets its own row). Reported here, not in the arm
	// itself, so a single blind session is one row rather than one per pid.
	for _, name := range o.blindSessions {
		report.Warn(sectionProcesses, "process-leak-inspection",
			fmt.Sprintf("could not read the pane tree of live session %s, so whether its "+
				"marked processes escaped is UNKNOWN rather than empty", name),
			"that session's processes are unclassified, not proven escapes; restore access to the "+
				"tmux server and rerun `af doctor`", false)
	}
}

// tmuxServerDead parses a TMUX env value ("socketPath,serverPID,sessionIdx")
// and reports whether the process identity it records still belongs to tmux.
// Unparseable values are treated as alive (never accuse on garbage). The socket
// path is deliberately not a liveness signal: a crashed server can leave it
// behind after the kernel recycles the recorded PID to an unrelated process.
func tmuxServerDead(ctx *scanContext, tmuxEnv string) bool {
	parts := strings.Split(tmuxEnv, ",")
	if len(parts) < 2 {
		return false
	}
	serverPID, err := strconv.Atoi(parts[1])
	if err != nil || serverPID <= 0 {
		return false
	}
	server, alive := ctx.snap[serverPID]
	if alive && strings.HasPrefix(server.Comm, "tmux") {
		return false
	}
	// PID gone or recycled to a non-tmux process: the server named by this
	// environment value is dead, regardless of any stale or reused socket path.
	return true
}
