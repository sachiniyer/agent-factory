package doctor

import (
	"errors"
	"fmt"
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
					// The same foreign-home attribution that the dead-session
					// arm applies below: on the shared default tmux server a
					// session name is derived from repo path + title and does
					// not encode AF_HOME, so a surviving process from another
					// install can carry a name that now matches one of OUR live
					// sessions (temporal reuse once that install's session died).
					// A non-matching AF_HOME proves it is not ours; a matching
					// one proves only that the SESSION was this home's — never
					// that the process carrying it is — so a matching marker is
					// resolved to the process's own home the way the dead arm
					// does (#6579), below. The escaped arm remains report-only
					// either way — no kill path is added here.
					home, homeStatus := proctree.LookupEnv(p.PID, tmux.EnvMarkerHome)
					if homeStatus == proctree.EnvFound {
						// A relative AGENT_FACTORY_HOME is stamped unchanged by
						// session/tmux.afHomeDir, so it is relative to the launching
						// af process's frame, not the doctor's. normalizeHome resolves
						// a path in THIS process's frame, which would resolve a
						// relative marker against the directory doctor was invoked
						// from — collapsing two distinct homes (each launched from a
						// different working directory with AF_HOME=.af) onto the same
						// doctor-relative path and letting a genuine foreign escapee
						// compare equal to ours, falling through as an escapee of this
						// install. The marker's originating frame is not recoverable
						// from here, so treat a relative marker as unproven rather
						// than normalizing it in the current frame: report it under the
						// foreign-home key without attributing the escape to us.
						if !filepath.IsAbs(home) {
							// A relative marker is unproven ownership, not a
							// confirmed foreign home: the marker's originating
							// frame is not recoverable from here, so the
							// collapsed row must not assert the definite "from
							// another agent-factory home" claim the absolute
							// arm makes. A distinct check key keeps this out of
							// the foreign-home-process collapse, whose summary
							// ("N processes from another agent-factory home")
							// would otherwise state a conclusion the detector
							// withheld for a relative marker in the default
							// (non-verbose) CLI and JSON output.
							report.addAdvisoryFinding(Finding{
								Check: "foreign-home-unresolved",
								Detail: fmt.Sprintf("%s carries live session %s's name but its AF_HOME (%s) "+
									"is a relative path that cannot be resolved to a specific agent-factory "+
									"home — not attributed to this install", describeProc(p), name, home),
							})
							continue
						}
						// Canonicalize both sides through normalizeHome (the same
						// home-identity normalization checkForeignDaemons uses) rather
						// than filepath.Clean: Clean is lexical and leaves a symlinked
						// AGENT_FACTORY_HOME — or macOS /var vs /private/var — comparing
						// unequal to the same home, which would report a genuine
						// escapee as foreign. Compare by filesystem identity first
						// (os.SameFile via sameHome): on a case-insensitive macOS
						// volume EvalSymlinks preserves the spelling of non-symlink
						// components, so ".Agent-Factory" and ".agent-factory" are the
						// same directory but compare unequal as normalized strings;
						// os.SameFile sees through the case difference and keeps a
						// genuine same-home escapee off the foreign-home report.
						if !sameHome(home, ctx.opts.ConfigDir) {
							// A distinct check key keeps this out of the escaped-process
							// collapse, whose row ("N processes escaped live session
							// pane trees") would re-attribute a foreign process to this
							// install in the default (non-verbose) CLI and JSON output.
							report.addAdvisoryFinding(Finding{
								Check: "foreign-home-process",
								Detail: fmt.Sprintf("%s carries live session %s's name but belongs to another "+
									"agent-factory home (%s) — not attributed to this install",
									describeProc(p), name, home),
							})
							continue
						}
						// A matching AF_HOME proves the SESSION was this home's —
						// never that the process carrying it is. A second install
						// launched inside one of this home's sessions inherits
						// AF_SESSION/AF_HOME beside its own AGENT_FACTORY_HOME (and
						// TMUX_TMPDIR), so its daemon, its tmux server and everything
						// they spawn carry a matching marker while belonging to the
						// other install — and the union listing cannot see that
						// install's live sessions, so they read as escaped here.
						// Ownership is established the way the dead-session arm below
						// establishes it: from the process's OWN environment, resolved
						// in its own frame through daemonProcessHome — the HOME-derived
						// default included, so a real same-home pane process (which
						// declares no AGENT_FACTORY_HOME of its own) still resolves to
						// this install — and, where its environment cannot answer, the
						// home its ancestors declare. Only a process proven to be this
						// install's is attributed as an escapee; a foreign or unproven
						// one is reported without attributing the escape to this install.
						ownership, foreignHome, foreignPID := ctx.markedProcessOwnership(p)
						switch ownership {
						case ownershipForeignEnv:
							report.addAdvisoryFinding(Finding{
								Check: "foreign-home-process",
								Detail: fmt.Sprintf("%s carries live session %s's name but belongs to another "+
									"agent-factory home (%s) — the matching AF_HOME marker was inherited "+
									"from a session of this install, so it is not attributed to this install",
									describeProc(p), name, foreignHome),
							})
							continue
						case ownershipForeignAncestor:
							report.addAdvisoryFinding(Finding{
								Check: "foreign-home-process",
								Detail: fmt.Sprintf("%s carries live session %s's name but runs under "+
									"pid %d, which declares another agent-factory home (%s) — the "+
									"matching marker was inherited, so it is not attributed to this install",
									describeProc(p), name, foreignPID, foreignHome),
							})
							continue
						case ownershipUnproven:
							report.addAdvisoryFinding(Finding{
								Check: "foreign-home-unresolved",
								Detail: fmt.Sprintf("%s carries live session %s's name and its AF_HOME "+
									"matches this install, but its own environment establishes no "+
									"agent-factory home — ownership is unproven, so it is not "+
									"attributed to this install", describeProc(p), name),
							})
							continue
						case ownershipUnprovenAncestor:
							report.addAdvisoryFinding(Finding{
								Check: "foreign-home-unresolved",
								Detail: fmt.Sprintf("%s carries live session %s's name and its AF_HOME "+
									"matches this install, but ancestor pid %d could not be verified "+
									"— it is missing from the process snapshot, changed identity "+
									"since the snapshot was taken, or its environment could not be "+
									"read — ownership is unproven, so it is not attributed to this "+
									"install", describeProc(p), name, foreignPID),
							})
							continue
						}
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
			home, homeStatus := proctree.LookupEnv(p.PID, tmux.EnvMarkerHome)
			hasHome := homeStatus == proctree.EnvFound
			switch {
			case hasHome && filepath.IsAbs(home) && sameHome(home, ctx.opts.ConfigDir):
				// A matching AF_HOME proves the SESSION was this home's —
				// never that the process carrying it is. A second install
				// launched inside one of this home's sessions inherits
				// AF_SESSION/AF_HOME beside its own AGENT_FACTORY_HOME (and
				// TMUX_TMPDIR), so its daemon, its tmux server and everything
				// they spawn carry a matching marker while belonging to the
				// other install — and the union listing cannot see that
				// install's live sessions, so they read as verified orphans
				// here (#6578). Ownership is established the way the daemon
				// checks establish which home a daemon serves: the process's
				// OWN environment, resolved in its own frame through
				// daemonProcessHome — the HOME-derived default included, so a
				// real same-home pane process (which declares no
				// AGENT_FACTORY_HOME of its own) still resolves to this
				// install — and, where its environment cannot answer, the
				// home its ancestors declare. The chain is verified end to
				// end: an ancestor that is missing from the snapshot, no
				// longer the process it recorded, or whose environment
				// cannot be read leaves ownership unproven rather than
				// clean (#6703). Only a positively different answer
				// disqualifies — and so does a chain that could not be
				// fully verified; what the process's frame cannot resolve
				// is reported, not killed.
				ownership, foreignHome, foreignPID := ctx.markedProcessOwnership(p)
				switch ownership {
				case ownershipForeignEnv:
					report.addAdvisoryFinding(Finding{
						Check: "foreign-home-process",
						Detail: fmt.Sprintf("%s marks dead session %s (home %s) but belongs to another "+
							"agent-factory home (%s) — the matching marker was inherited from a "+
							"session of this install, so it is reported, not killed; run "+
							"`af doctor` with that home active",
							describeProc(p), name, home, foreignHome),
					})
					continue
				case ownershipForeignAncestor:
					report.addAdvisoryFinding(Finding{
						Check: "foreign-home-process",
						Detail: fmt.Sprintf("%s marks dead session %s (home %s) but runs under "+
							"pid %d, which declares another agent-factory home (%s) — the "+
							"matching marker was inherited, so it is reported, not killed",
							describeProc(p), name, home, foreignPID, foreignHome),
					})
					continue
				case ownershipUnproven:
					report.addAdvisoryFinding(Finding{
						Check: "orphaned-process",
						Detail: fmt.Sprintf("%s marks dead session %s (home %s) but its own "+
							"environment establishes no agent-factory home — ownership is "+
							"unproven, so it is reported, not killed",
							describeProc(p), name, home),
					})
					continue
				case ownershipUnprovenAncestor:
					report.addAdvisoryFinding(Finding{
						Check: "orphaned-process",
						Detail: fmt.Sprintf("%s marks dead session %s (home %s) but ancestor "+
							"pid %d could not be verified — it is missing from the process "+
							"snapshot, changed identity since the snapshot was taken, or its "+
							"environment could not be read — ownership is unproven, so it is "+
							"reported, not killed",
							describeProc(p), name, home, foreignPID),
					})
					continue
				}
				verdict, proofSocket := ctx.sessionDeathProof(p, name)
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
								"it is reported, not killed", describeProc(p), name, home, proofSocket, paneErr),
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
							describeProc(p), name, home),
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
							describeProc(p), name, home),
					})
					continue
				}
			case hasHome && filepath.IsAbs(home):
				report.addAdvisoryFinding(Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s marks dead session %s but belongs to another "+
						"agent-factory home (%s) — its session may be live on that install's "+
						"private tmux server, so it is not fixed from here; run `af doctor` "+
						"with that home active", describeProc(p), name, home),
				})
			case hasHome:
				// A relative marker is unproven ownership, not a confirmed
				// foreign home: the marker's originating frame is not
				// recoverable from here, so the collapsed row must not assert
				// the definite "from another agent-factory home" claim the
				// absolute arm makes. A distinct check key (mirroring the
				// escaped arm) keeps this out of the orphaned-process collapse,
				// whose summary would otherwise either count an unproven
				// process as "safe to clean" (if it were actionable) or state a
				// foreign-home conclusion the detector withheld.
				report.addAdvisoryFinding(Finding{
					Check: "foreign-home-unresolved",
					Detail: fmt.Sprintf("%s marks dead session %s but its AF_HOME (%s) "+
						"is a relative path that cannot be resolved to a specific agent-factory "+
						"home — not attributed to this install", describeProc(p), name, home),
				})
			default:
				report.addAdvisoryFinding(Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s marks dead session %s but carries no readable "+
						"home marker — cannot prove which install owns it, so it is "+
						"reported, not killed", describeProc(p), name),
				})
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
// process belongs to, independent of its AF_HOME marker (#6578). The marker
// proves the session was this home's; it never proves the process carrying it
// is, because a session's environment is inherited by every process its
// children spawn — including a second install launched from inside it.
type ownershipVerdict int

const (
	// ownershipThisInstall is the only verdict that may arm a kill: the
	// process's own environment resolves to this home and no ancestor
	// declares a different one.
	ownershipThisInstall ownershipVerdict = iota
	// ownershipForeignEnv means the process's own environment resolves to a
	// different install's home — its matching AF_HOME marker was inherited.
	ownershipForeignEnv
	// ownershipForeignAncestor means the process's own home is this
	// install's (or is unresolvable) but an ancestor declares a different
	// install's home — it runs under another install's tmux server, daemon
	// or helper, which an env scrub cannot erase.
	ownershipForeignAncestor
	// ownershipUnproven means the process's own environment establishes no
	// home at all: nothing names another install, but the marker alone is
	// not proof of this one either.
	ownershipUnproven
	// ownershipUnprovenAncestor means the ancestor chain could not be fully
	// verified — an ancestor was missing from the snapshot, no longer names
	// the process it recorded, or its environment could not be read. An
	// ancestor whose home cannot be ruled out is not proven non-foreign.
	ownershipUnprovenAncestor
)

// markedProcessOwnership resolves which install a marked process belongs to,
// independent of its inherited-able AF_HOME marker. The process's own home is
// resolved by daemonProcessHome — the same rule checkForeignDaemons and the
// leaked-daemon checks act on, computed in the process's own frame — then its
// ancestor chain is consulted for the shapes its environment cannot answer (a
// tmux server's descendants share its inherited markers even when their own
// AGENT_FACTORY_HOME was never set or was scrubbed). The chain must verify
// end to end: an ancestor the snapshot cannot show, cannot revalidate, or
// cannot read leaves the verdict unproven rather than clean (#6703). Returns
// the verdict, the home an ancestor declared where one was proven foreign,
// and the ancestor pid the verdict names (0 when the process's own
// environment did).
func (ctx *scanContext) markedProcessOwnership(p proctree.Process) (ownershipVerdict, string, int) {
	ownHome, known := daemonProcessHome(p.PID)
	if known && !sameHome(ownHome, ctx.opts.ConfigDir) {
		return ownershipForeignEnv, ownHome, p.PID
	}
	switch verdict, home, pid := ctx.ancestorHomeProof(p); verdict {
	case ancestryForeign:
		return ownershipForeignAncestor, home, pid
	case ancestryUnproven:
		return ownershipUnprovenAncestor, "", pid
	}
	if !known {
		return ownershipUnproven, "", 0
	}
	return ownershipThisInstall, "", 0
}

// ancestryVerdict is the outcome of walking a marked process's ancestor
// chain: clean (every ancestor up to init was verified and none declares
// another install's home), foreign (a verified ancestor declares another
// install's home), or unproven (the chain could not be fully verified).
type ancestryVerdict int

const (
	ancestryClean ancestryVerdict = iota
	ancestryForeign
	ancestryUnproven
)

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
func (ctx *scanContext) ancestorHomeProof(p proctree.Process) (ancestryVerdict, string, int) {
	seen := map[int]bool{p.PID: true}
	pid := p.PPID
	for hops := 0; pid > 1 && hops < ancestorHopLimit; hops++ {
		if seen[pid] {
			// A cycle back onto an already-verified ancestor — nothing
			// new remains to check.
			return ancestryClean, "", 0
		}
		seen[pid] = true
		a, ok := ctx.snap[pid]
		if !ok {
			// The recorded parent is absent from the snapshot: it exited
			// between the stat pass and now, or the table never showed
			// it. Either way it cannot be interrogated.
			return ancestryUnproven, "", pid
		}
		if same, err := ancestorIdentity(a); err != nil || !same {
			// The pid was recycled since the snapshot — or cannot be
			// revalidated — so the record describes a different process
			// than the one running now.
			return ancestryUnproven, "", pid
		}
		home, declared, readable := declaredAgentFactoryHome(pid)
		if !readable {
			return ancestryUnproven, "", pid
		}
		if declared && !sameHome(home, ctx.opts.ConfigDir) {
			return ancestryForeign, home, pid
		}
		pid = a.PPID
	}
	if pid > 1 {
		// The hop bound ran out mid-chain — the unexamined tail is
		// unverified.
		return ancestryUnproven, "", pid
	}
	return ancestryClean, "", 0
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
