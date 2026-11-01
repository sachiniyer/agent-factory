package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Everything `af doctor` learns from the tmux SERVER, and the one rule that
// governs all of it: A FAILED READ IS NOT AN EMPTY RESULT.
//
// Split out of checks.go for the file-length lint, but the grouping is the real
// point (#2874). Four checks reason from this data by treating an ABSENCE as
// evidence — a session missing from the list is dead, a home no session names is
// abandoned, a session in no record leaked — and two of them spend that evidence
// on a kill or an rm -rf. Every read here is therefore three-valued, and a
// consumer that cannot get an answer does nothing at all.

// socketListing is one socket's memoized `tmux ls` result — the error is held
// alongside the names because a failed read is not an empty result.
type socketListing struct {
	names []string
	err   error
}

// sessionNamesOnSocket lists the session names on ONE tmux socket, memoized so
// the union build and the per-process corroboration share a single view of
// each socket. "" names doctor's ambient server, exactly as it does for
// tmux.ListSessionNamesOnSocket.
func (c *scanContext) sessionNamesOnSocket(socket string) ([]string, error) {
	if c.socketSessions == nil {
		c.socketSessions = make(map[string]socketListing)
	}
	if l, seen := c.socketSessions[socket]; seen {
		return l.names, l.err
	}
	names, err := tmux.ListSessionNamesOnSocket(c.opts.Exec, socket)
	c.socketSessions[socket] = socketListing{names: names, err: err}
	return names, err
}

// liveTmuxSessionSockets returns the run's tmux session listing as a union
// map — each live session name to the socket it was listed on — covering the
// daemon's recorded socket PLUS every prior socket generation its record
// names (#6339), or an error when tmux could not tell us about any one of
// them.
//
// THE INVARIANT, stated rather than borrowed: A FAILED READ IS NOT AN EMPTY
// RESULT — and AN UNACCOUNTED SOCKET GENERATION IS NOT AN EMPTY SOCKET. Every
// consumer below treats a name's ABSENCE as evidence the session is dead, and
// two of them spend that evidence on a kill or an rm -rf. A restart can move
// the daemon to a new socket while its previous server and sessions stay
// alive on the old one, so a listing of only the recorded socket reads a
// still-live session as absent: absence is evidence of death only once EVERY
// socket the daemon has recorded has been listed. A prior generation that
// cannot be listed is a generation doctor cannot account for, so it fails the
// whole union rather than produce a partially-true view — the same rule that
// makes the primary listing's failure blind rather than empty.
//
// It DELEGATES rather than shelling out — the second read in this file to do
// so, after tmuxSessionHomeMarker, and for the same reason. The copy that
// lived here fell outside both invariants session/tmux maintains:
//
//   - the classification of tmux's ambiguous exit 1, which it got wrong: every
//     failure became an empty list, an unreachable socket read as "no sessions
//     are live", and checkOrphanedProcesses turned that into an armed
//     `kill pid N` for every live session's processes (#2874);
//   - the tmuxCommandTimeout BOUND, which it lacked entirely. `af doctor --fix`
//     re-lists before removing a stale home, so against a server that wedged
//     mid-run an unbounded listing hangs the cleanup instead of refusing the
//     removal it can no longer justify (#2910).
//
// The listing is PINNED to the daemon's recorded socket (`tmux -S <socket> ls`,
// via recordedSocket) rather than left to the bare `tmux ls` the daemon's own
// teardown uses. Doctor is a separate process from the daemon, and the two can
// resolve DIFFERENT $TMUX/$TMUX_TMPDIR values (session/tmux/socket.go documents
// exactly this divergence): an ambient listing names whatever server doctor's
// own environment reaches, so a live session on the daemon's socket is absent
// from it and every process carrying its marker reads as a verified orphan —
// and `--fix` SIGKILLs it. Pinning makes the listing name the server the daemon
// recorded. With no record (a home that never ran the Linux daemon that writes
// one) the socket is empty and the listing is the historical ambient one.
//
// Do not restore a "mirrors X" comment in either direction, and do not
// re-inline the shell-out. State the property; keep the mechanism in one place.
func liveTmuxSessionSockets(ctx *scanContext, fresh bool) (map[string][]string, error) {
	list := ctx.sessionNamesOnSocket
	if fresh {
		list = func(socket string) ([]string, error) {
			return tmux.ListSessionNamesOnSocket(ctx.opts.Exec, socket)
		}
	}
	socket, _ := ctx.recordedSocket()
	// Each name maps to EVERY socket it was listed on, not just the first:
	// session names derive from repository path and title, not AF home or
	// socket, so two servers can legitimately host a same-named session that
	// belongs to different installs. Deduplicating to one socket would lose
	// the dropped copy's ownership marker — and a live session's home is the
	// veto that keeps rm -rf away from it.
	live := map[string][]string{}
	names, err := list(socket)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		live[name] = append(live[name], socket)
	}
	if st, ok, rerr := ctx.recordedServer(); rerr == nil && ok {
		for _, prior := range st.PriorSockets {
			if prior == "" || sameSocket(prior, socket) {
				continue
			}
			priorNames, perr := list(prior)
			if perr != nil {
				return nil, fmt.Errorf("cannot list the daemon's previously recorded tmux socket %s: %w",
					prior, perr)
			}
			for _, name := range priorNames {
				live[name] = append(live[name], prior)
			}
		}
	}
	return live, nil
}

// recordedSocketKind names how the run's tmux listing relates to the daemon's
// recorded socket — three states because a name's absence from the listing
// arms a kill, and the meaning of that absence differs by state.
type recordedSocketKind int

const (
	// recordedSocketAbsent: no tmux-server-state.json record exists AND no
	// daemon is currently serving this home — the reading under which "no
	// record" historically meant "this home never ran the Linux daemon that
	// writes one," so the ambient listing was the only evidence available.
	// Absence is not proof of that, though: recordServerState silently
	// ignores write failures, macOS never writes a record at all, and a
	// dead daemon leaves sessions live on a socket the record never named.
	// The kill arm therefore corroborates an ambient listing's absence
	// PER PROCESS against the socket that process's own TMUX env names
	// (see sessionDeathCorroborated), arming only when the two sockets
	// agree — i.e. only when the ambient server is positively the one that
	// hosted the dead session. A process whose TMUX env names a different
	// socket (a live session on a non-ambient server reads dead here, the
	// #6271 wrong-server kill) or no socket at all is reported, not killed.
	recordedSocketAbsent recordedSocketKind = iota
	// recordedSocketPinned: a record named a socket, and the listing is run
	// against it with `tmux -S <socket> ls`. A name absent from THAT listing is
	// genuinely gone, so the orphaned-process kill arm is corroborated.
	recordedSocketPinned
	// recordedSocketUnconfirmed: a record exists but could not be read (or named
	// no socket), so the daemon DID host sessions on a socket doctor cannot pin
	// to. The ambient listing may be looking at a different server, so a name's
	// absence from it is NOT proof of death — a live session on the daemon's
	// (unreadable) socket would look the same here. The orphaned-process kill
	// arm stays report-only in this state.
	recordedSocketUnconfirmed
)

// recordedServer resolves the daemon's tmux-server-state.json once per run and
// memoizes it, so checkTmuxServerState's environment row and the socket-pinned
// tmux reads share one observation — the same one-read contract the session
// listing and the process table already keep.
func (c *scanContext) recordedServer() (tmux.ServerState, bool, error) {
	if !c.recordedServerStateResolved {
		c.recordedServerState, c.recordedServerStateOK, c.recordedServerStateErr = tmux.ReadServerState(c.opts.ConfigDir)
		c.recordedServerStateResolved = true
	}
	return c.recordedServerState, c.recordedServerStateOK, c.recordedServerStateErr
}

// recordedSocket derives the socket to pin doctor's tmux reads to and the kind
// of evidence the resulting listing carries. See recordedSocketKind for the
// contract; an empty socket with recordedSocketAbsent or recordedSocketUnconfirmed
// means the caller runs the ambient (unpinned) listing.
func (c *scanContext) recordedSocket() (string, recordedSocketKind) {
	state, ok, err := c.recordedServer()
	switch {
	case err != nil:
		return "", recordedSocketUnconfirmed
	case !ok:
		// No record exists. The historical reading is "this home never ran
		// the Linux daemon that writes one," and the ambient listing is the
		// only evidence. But absence is not proof the daemon never ran:
		// recordServerState silently ignores write failures, macOS never
		// writes a record, and the file may have been deleted while a daemon
		// remains live. When a daemon IS serving this home it should have
		// written a record, so its absence means doctor cannot prove the
		// ambient server is the one the daemon hosts sessions on — a live
		// session on the daemon's (unrecorded) socket would read dead here
		// exactly as it does for an unreadable record. Treat a live daemon
		// as unconfirmed rather than arming kills from the ambient listing.
		//
		// The dead-daemon window (the daemon hosted sessions and then died,
		// so activeHomeDaemons is empty) falls through to recordedSocketAbsent
		// rather than unconfirmed: this home may genuinely never have run a
		// record-writing daemon (the case the ambient listing was kept for),
		// and the per-process TMUX-env corroboration sessionDeathCorroborated
		// applies there arms the kill only when the ambient listing is
		// positively the server the session lived on — closing the wrong-
		// server kill for this window without forgoing --fix cleanup for the
		// never-ran-a-daemon homes that do corroborate.
		if len(activeHomeDaemons(c)) > 0 {
			return "", recordedSocketUnconfirmed
		}
		return "", recordedSocketAbsent
	case state.Socket != "":
		return state.Socket, recordedSocketPinned
	default:
		// A present record that names no socket: the daemon wrote something but
		// did not record which server it hosts sessions on, so doctor cannot pin
		// to it. Treat as unconfirmed rather than trusting the ambient listing,
		// since a live session on the daemon's (unnamed) socket would read dead
		// here exactly as it does for an unreadable record.
		return "", recordedSocketUnconfirmed
	}
}

// sessionDeathVerdict is what doctor can prove about a marked process's
// session once that session is absent from the union listing — the standard
// the orphaned-process kill arm must meet before it arms a kill.
type sessionDeathVerdict int

const (
	// deathProven: the session is dead on the server that hosted it AND every
	// socket generation the daemon has used is accounted for — absence reads
	// as death, so the kill arm may fire.
	deathProven sessionDeathVerdict = iota
	// deathUnproven: doctor cannot corroborate the session's death — the
	// record could not be proven current, the socket hosting it could not be
	// listed, or no evidence names the server it lived on. The process is
	// reported, never killed.
	deathUnproven
	// deathLiveElsewhere: the session is absent from the union listing but
	// live on the socket the process's own TMUX env names — verified by
	// listing THAT socket and finding it there. socket carries where.
	deathLiveElsewhere
)

// recordedServerCurrent reports whether the daemon's server-state record was
// written by the daemon generation currently serving this home — the
// precondition for trusting the record's socket set as complete (#6339).
//
// The record is stamped with its author's pid (ServerState.DaemonPID). When a
// daemon IS serving this home, the stamped pid must be one of the live daemon
// pids: a mismatch — or no stamp at all — means a previous generation left
// the record behind, and a record a dead generation wrote may name a socket
// the live daemon no longer uses. When NO daemon is live, a stamped record is
// still the last word a daemon generation left — its socket set is the best
// accounting there is, so it stands; an UNSTAMPED record predates generation
// binding entirely and cannot be proven to account for every socket the
// daemon ran, so it does not corroborate a kill on its own.
func (c *scanContext) recordedServerCurrent() bool {
	st, ok, err := c.recordedServer()
	if err != nil || !ok {
		return false
	}
	daemons := activeHomeDaemons(c)
	if len(daemons) == 0 {
		return st.DaemonPID != 0
	}
	for _, d := range daemons {
		if st.DaemonPID == d.PID {
			return true
		}
	}
	return false
}

// sessionDeathProof decides whether the orphaned-process arm may treat a
// marked process's session as DEAD — absent from the union listing — and arm
// its kill. The union covers every socket the record names, but absence there
// is still not proof by itself: the record may not be current (a restart that
// adopted a server used to leave the previous generation's socket recorded,
// #6339), and a live session hosted on a socket outside the record's set
// reads exactly like a dead one here.
//
// So the proof asks the process itself first: tmux stamps every pane process
// with TMUX="socketPath,serverPID,sessionIdx", naming the server its session
// lived on. When that server is dead (its pid gone or recycled — see
// tmuxServerDead) the session died with it: proven. When it is alive, doctor
// lists THAT socket — a present name means the session is live elsewhere (and
// routes to the escaped-process arm), an absent one proves death on the very
// server the session lived on, and a failed listing proves nothing. Only when
// the process carries no usable TMUX env does the proof fall back to the
// record: it corroborates a kill only when the record is current AND every
// socket it names — recorded plus prior generations — listed the session
// absent, which is exactly what absence from the union map means.
func (c *scanContext) sessionDeathProof(p proctree.Process, name string) (sessionDeathVerdict, string) {
	tmuxEnv, status := proctree.LookupEnv(p.PID, "TMUX")
	if status == proctree.EnvFound && tmuxEnv != "" {
		if tmuxServerDead(c, tmuxEnv) {
			return deathProven, ""
		}
		if procSocket, _, _ := strings.Cut(tmuxEnv, ","); strings.TrimSpace(procSocket) != "" {
			procSocket = strings.TrimSpace(procSocket)
			names, err := c.sessionNamesOnSocket(procSocket)
			if err == nil {
				if slices.Contains(names, name) {
					return deathLiveElsewhere, procSocket
				}
				return deathProven, ""
			}
			// The process names a server doctor cannot list: no fallback can
			// substitute for a listing of the socket its session lived on.
			return deathUnproven, ""
		}
	}
	_, kind := c.recordedSocket()
	if kind != recordedSocketPinned {
		return deathUnproven, ""
	}
	if !c.recordedServerCurrent() {
		return deathUnproven, ""
	}
	// The union listing succeeded — this arm only runs after it did — so it
	// has already listed every socket the record names. A name still absent
	// is absent from every socket generation the daemon has used.
	live, err := c.tmuxSessions()
	if err != nil {
		return deathUnproven, ""
	}
	if sockets, found := live[name]; found {
		return deathLiveElsewhere, sockets[0]
	}
	// A record whose socket history hit its cap has DROPPED a generation that
	// may still host the session; absence from the sockets it retains is not
	// complete coverage, so the union cannot corroborate a kill.
	if st, ok, rerr := c.recordedServer(); rerr != nil || !ok || st.HistoryTruncated {
		return deathUnproven, ""
	}
	return deathProven, ""
}

// sameSocket reports whether two tmux socket paths identify the same socket.
// Compares by filesystem identity (os.SameFile) when both stat, so a symlinked
// TMUX_TMPDIR — or macOS /var vs /private/var — does not split one socket into
// two; falls back to a cleaned lexical comparison when either path cannot be
// statted (a dead server leaves no socket file to stat, and the comparison
// must still answer for it).
func sameSocket(a, b string) bool {
	if a == b {
		return true
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// captureSessionProcessTrees is the doctor wrapper around
// tmux.CaptureSessionProcessTreesOnSocket, pinning the pane capture to the
// socket the listing found the session on so the escaped-process arm reads
// the SAME server the session listing named it live on. Against doctor's
// ambient (wrong) server a session the listing proved live answers
// ErrSessionVanishedBeforeCapture, which clears sessionLive and falls through
// to the orphaned-process kill arm for a process that simply belongs to a live
// session on another socket — so the pane capture must be pinned whenever the
// listing is.
func captureSessionProcessTrees(ctx *scanContext, name, socket string) ([]proctree.Process, error) {
	return tmux.CaptureSessionProcessTreesOnSocket(ctx.opts.Exec, name, socket)
}

// tmuxSessions returns the run's memoized session listing — live session name
// to every socket it was listed on — or the error that made it unavailable. A
// caller that treats a missing name as proof the session is DEAD must handle
// the error and do nothing; see the scanContext field comments. The doctor run
// is sequential, so a plain memo is enough (mirrors daemonProcs).
func (c *scanContext) tmuxSessions() (map[string][]string, error) {
	if !c.tmuxScanned {
		c.tmuxLive, c.tmuxErr = liveTmuxSessionSockets(c, false)
		c.tmuxScanned = true
	}
	return c.tmuxLive, c.tmuxErr
}

// checkTmuxInspection turns the run's session listing into a report row, and is
// the ONE place its failure is announced — the checks that consume the list then
// stay silent rather than each repeating it, exactly as they do for
// checkProcessInspection and an unreadable process table (#1939).
//
// It must run BEFORE those checks, or the operator reads their silence before
// learning why they were silent.
func checkTmuxInspection(ctx *scanContext, report *Report) {
	live, err := ctx.tmuxSessions()
	if err == nil {
		sockets := map[string]bool{}
		for _, listed := range live {
			for _, socket := range listed {
				sockets[socket] = true
			}
		}
		detail := fmt.Sprintf("listed the tmux server's sessions (%d)", len(live))
		if len(sockets) > 1 {
			detail = fmt.Sprintf("listed %d sessions across %d tmux socket generations",
				len(live), len(sockets))
		}
		report.Pass(sectionProcesses, "tmux-inspection", detail)
		return
	}
	report.Fail(sectionProcesses, "tmux-inspection",
		fmt.Sprintf("cannot list tmux sessions, so the orphaned-process, escaped-process, runaway-child, "+
			"leaked-session and stale-temp-home checks below could not establish which sessions are live: %v", err),
		"those checks are UNKNOWN, not clean — nothing was reported as orphaned or abandoned and `--fix` "+
			"proposed no kill or removal derived from session liveness on this run. Restore access to the "+
			"tmux server and re-run")
}

// checkTmuxRecordCurrency warns when the daemon's server-state record admits
// a gap doctor must not act past: either a live daemon serves this home while
// the record was stamped by a different generation — a restart that adopts an
// already-running server used to leave the previous generation's socket
// recorded (#6339) — or the socket history's bound dropped a generation the
// record can no longer account for. The recorded sockets may then not cover
// where sessions live, so the orphaned-process kill arm refuses to
// corroborate from it; these rows say why rather than letting the refusal
// look like whim. It runs after the process snapshot so the live daemon set
// it compares against is real — environment checks run before it and could
// only report on a snapshot that does not exist yet.
func checkTmuxRecordCurrency(ctx *scanContext, report *Report) {
	state, ok, err := ctx.recordedServer()
	if !ok || err != nil || state.Mode == "" {
		return
	}
	if state.HistoryTruncated {
		report.Warn(sectionProcesses, "tmux server record",
			"the daemon's tmux socket history hit its bound and dropped a socket generation "+
				"the record can no longer account for — a live session on the dropped socket "+
				"reads exactly like a dead one, so record-only death proofs are refused",
			"the bound keeps the record small but the admission never clears; if doctor reports "+
				"sessions it cannot classify, list the live tmux servers and kill leftovers by hand", false)
	}
	daemons := activeHomeDaemons(ctx)
	if len(daemons) == 0 || ctx.recordedServerCurrent() {
		return
	}
	var livePIDs []string
	for _, d := range daemons {
		livePIDs = append(livePIDs, strconv.Itoa(d.PID))
	}
	report.Warn(sectionProcesses, "tmux server record",
		fmt.Sprintf("the daemon's tmux server record was written by daemon pid %d but live daemon "+
			"pid(s) %s serve this home — the recorded socket %s may not be where sessions live, and "+
			"the orphaned-process kill arm cannot corroborate from it",
			state.DaemonPID, strings.Join(livePIDs, ", "), state.Socket),
		"the daemon normally rewrites this record at startup; restarting the daemon refreshes it. "+
			"Until then, sessions live on a socket the record does not name are reported, never killed", false)
}

// checkRunawayChildren reports (never kills) descendants of live af_
// sessions that have averaged a pegged core for an extended period.
func checkRunawayChildren(ctx *scanContext, report *Report) {
	// See checkOrphanedProcesses: blindness is reported once — by
	// checkProcessInspection for the table, checkTmuxInspection for the session
	// list — rather than swallowed here. This check is report-only, so an
	// unavailable listing costs an under-report rather than a wrong action, but
	// an under-report that renders as "no runaway processes" is the very thing
	// this package refuses to print.
	live, listErr := ctx.tmuxSessions()
	if ctx.snap == nil || listErr != nil {
		return
	}
	unmeasurable := 0
	var blindSessions []string
	for _, name := range sortedKeys(live) {
		if !strings.HasPrefix(name, tmux.TmuxPrefix) {
			continue
		}
		// The pane tree comes from the EVIDENCE-BEARING capture so a per-session
		// list-panes failure is not swallowed as an empty result. A session
		// whose pane tree could not be read is named in a blindness row so the
		// function does not print "no runaway processes" over a session it never
		// fully inspected — the same treatment the unmeasurable counter gives
		// CPU blindness. Partial captures still yield verified processes that
		// are evaluated for runaway findings before recording the blindness.
		procs, paneErr := captureSessionProcessTrees(ctx, name, live[name][0])
		if paneErr != nil {
			// A vanished session is conclusively no longer live: it is not a
			// blind live session, so it gets no blindness row and the operator
			// needs no nudge to inspect it — it simply exited.
			if errors.Is(paneErr, tmux.ErrSessionVanishedBeforeCapture) {
				continue
			}
			// Partial captures (non-nil procs alongside an error) still carry
			// verified processes; evaluate them for runaway findings before
			// recording the blindness warning so a CPU-pegged child in an
			// unaffected pane is not silently dropped.
			blindSessions = append(blindSessions, name)
		}
		sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
		for _, p := range procs {
			if ctx.selfAncestors[p.PID] {
				continue
			}
			frac, age, err := proctree.CPUFraction(p)
			if errors.Is(err, proctree.ErrCPUUnknown) {
				// Counted, not swallowed. A check that cannot answer must say
				// so: silently skipping every process would render "no runaway
				// processes" — the exact shape of the report this package
				// exists to stop printing. Reported once below rather than per
				// process, since the cause is usually systemic (a subset=pid
				// procfs hides /proc/uptime, so NO process has an age).
				unmeasurable++
				continue
			}
			if err != nil || frac < runawayCPUFraction || age < runawayMinAge.Seconds() {
				continue
			}
			report.addAdvisoryFinding(Finding{
				Check: "runaway-cpu",
				Detail: fmt.Sprintf("%s in live session %s has averaged a pegged core — "+
					"check the session; doctor never kills children of live sessions", describeProc(p), name),
			})
		}
	}
	if len(blindSessions) > 0 {
		// Pane-listing blindness, not swallowed. A session whose pane tree
		// could not be read renders no runaway finding for itself, so the
		// function's "refuses to print 'no runaway processes'" principle
		// applies to it exactly as it does to the unmeasurable counter below.
		// The session names are sorted for deterministic output and printed so
		// the operator knows WHICH sessions were blind rather than a count only.
		sort.Strings(blindSessions)
		report.Warn(sectionProcesses, "runaway-cpu",
			fmt.Sprintf("could not read the pane tree of %s (%s), so this check reports nothing about them",
				plural(len(blindSessions), "live session", "live sessions"),
				strings.Join(blindSessions, ", ")),
			"a process pegging a core in those sessions would not be spotted here; inspect the tmux server and rerun", false)
	}
	if unmeasurable > 0 {
		report.Warn(sectionProcesses, "runaway-cpu",
			fmt.Sprintf("could not measure CPU for %s in live sessions, so this check reports nothing "+
				"about them", plural(unmeasurable, "process", "processes")),
			"a process pegging a core would not be spotted here; inspect the sessions yourself", false)
	}
}

// checkLeakedTmuxSessions reports af_ tmux sessions with no backing record
// in this home's storage. Report-only even under --fix: a session with no
// record here may be owned by another agent-factory home on the same tmux
// server, and killing someone else's live session is worse than a leak.
func checkLeakedTmuxSessions(ctx *scanContext, report *Report) {
	// This check reads TWO surfaces and calls a session leaked on the ABSENCE of
	// evidence in both, so neither may answer "absent" when it merely failed.
	// The session list is reported once by checkTmuxInspection; the record store
	// is this check's own input, so its failure is reported here.
	//
	// No --fix closure is armed either way, which is not the same as harmless:
	// every row prints a `tmux kill-session` command for the operator to run, so
	// an unreadable record store hands them a kill command for each of their
	// healthy sessions (#2874).
	live, listErr := ctx.tmuxSessions()
	if listErr != nil {
		return
	}
	recorded, recordErr := recordedTmuxNames()
	if recordErr != nil {
		report.Fail(sectionProcesses, "session-records",
			fmt.Sprintf("cannot read this home's session records, so no tmux session can be classified as "+
				"leaked: %v", recordErr),
			"the leaked-session check is UNKNOWN, not clean — repair or remove the unreadable records under "+
				"<AF_HOME>/instances and re-run")
		return
	}
	report.Pass(sectionProcesses, "session-records",
		fmt.Sprintf("read this home's session records (%d recorded tmux name(s))", len(recorded)))
	var leaked []string
	for _, name := range sortedKeys(live) {
		if strings.HasPrefix(name, tmux.TmuxPrefix) && !recorded[name] {
			leaked = append(leaked, name)
		}
	}
	// The leaked-session listing is pinned per session to the socket the union
	// found it on, so the manual remedy must target THAT server: a bare
	// `tmux kill-session -t ...` resolves doctor's own $TMUX/$TMUX_TMPDIR and would
	// miss the session (or, on a shared name, kill an unrelated one on the
	// ambient server) — the same wrong-server divergence the listing pinning
	// closed. A session on the ambient listing's socket keeps the ambient remedy.
	for _, name := range leaked {
		origin := "no ancestry marker"
		ownedByActiveHome := false
		// When the same name lives on several sockets the union preserves every
		// pair, but this row reports one session: the first socket's copy is
		// the one the marker and the manual remedy below pin to. Reading only
		// the first errs advisory — a same-named session on another socket
		// claiming this home keeps the row unactionable rather than arming a
		// kill command for a session that might not be ours.
		switch home, present, markerErr := tmuxSessionHomeMarker(ctx, name, live[name][0]); {
		case markerErr != nil:
			// Unanswered, not unmarked. Stays advisory below — the actionable
			// row asserts this install created the session, and we do not know
			// that (#2874).
			origin = fmt.Sprintf("ownership could not be read: %v", markerErr)
		case present && home != "" && !filepath.IsAbs(home):
			// A relative AGENT_FACTORY_HOME is stamped relative to the
			// launching af process's frame (session/tmux.afHomeDir leaves it
			// unchanged), which doctor cannot recover, so it cannot prove
			// which install created this session. Normalizing it in doctor's
			// frame would collapse two distinct homes — each launched from a
			// different working directory with AF_HOME=.af — onto the same
			// doctor-relative path and let a genuine foreign session compare
			// equal to ours, arming a manual `tmux kill-session` for a session
			// that may belong to another install. Treat a relative marker as
			// unproven ownership rather than a confirmed match — it stays
			// advisory below, mirroring the orphaned-process arm.
			origin = fmt.Sprintf("created from a relative AGENT_FACTORY_HOME (%s) that cannot be resolved to a specific install",
				home)
		case present && home != "" && sameHome(home, ctx.opts.ConfigDir):
			// Canonicalized by filesystem identity (os.SameFile via sameHome)
			// rather than filepath.Clean: Clean is lexical and leaves a
			// symlinked AGENT_FACTORY_HOME — or macOS /var vs /private/var, or
			// a case-differing spelling on a case-insensitive volume —
			// comparing unequal to the same home, which would report a
			// genuine same-install leak as "another home" and keep it
			// advisory instead of actionable.
			origin = "created by this install"
			ownedByActiveHome = true
		case present && home != "":
			origin = "created by another agent-factory home: " + home
		}
		killCmd := shellsuggest.Command("tmux", "kill-session", "-t", tmux.ExactTarget(name))
		if socket := live[name][0]; socket != "" {
			// tmux.ExactTarget spells "=name:", one argument, so it is one
			// piece the seam quotes as a whole. `-S <socket>` pins
			// the remedy to the server the listing found the session on.
			killCmd = shellsuggest.Command("tmux", "-S", socket, "kill-session", "-t", tmux.ExactTarget(name))
		}
		finding := Finding{
			Check: "leaked-tmux-session",
			Detail: fmt.Sprintf("tmux session %s has no backing record in %s (%s); "+
				"kill it with: %s", name, ctx.opts.ConfigDir, origin, killCmd),
		}
		if ownedByActiveHome {
			// This install's marker plus no record is a proven leak with an
			// exact manual remedy. --fix support is not required for a row to
			// be actionable.
			report.addActionableFinding(finding)
		} else {
			// A shared tmux server can expose another install's healthy session,
			// and a pre-marker session has unknown ownership.
			report.addAdvisoryFinding(finding)
		}
	}
}

// recordedTmuxNames loads every persisted tmux session name (agent + tabs)
// from this home's storage, read-only. Legacy records without an explicit
// TmuxName fall back to the derived repo-scoped name.
// recordedTmuxNames returns every tmux session name this home's records claim,
// or an error when the record store could not be read.
//
// The error is not optional detail: the only consumer decides a session is
// LEAKED by its absence from this set, so a read failure that returned an empty
// set would indict every live session (#2874).
//
// It takes no configDir: doctor may be pointed at a ConfigDir other than the
// ambient one only in tests, which also set AGENT_FACTORY_HOME, and
// LoadAllRepoInstances always reads the ambient home.
func recordedTmuxNames() (map[string]bool, error) {
	names := map[string]bool{}
	all, skipped, err := config.LoadAllRepoInstancesReportingSkips()
	if err != nil {
		return nil, err
	}
	// A repo the loader could not read contributes no names, and every one of
	// its live sessions would then look leaked. Only the ReportingSkips form
	// makes those repos visible at all — the plain loader drops them silently.
	if len(skipped) > 0 {
		return nil, fmt.Errorf("session records for %s could not be read", strings.Join(skipped, ", "))
	}
	type tabRec struct {
		TmuxName string `json:"tmux_name"`
	}
	type instRec struct {
		Title    string   `json:"title"`
		Path     string   `json:"path"`
		TmuxName string   `json:"tmux_name"`
		Tabs     []tabRec `json:"tabs"`
	}
	for repoID, raw := range all {
		var instances []instRec
		if err := json.Unmarshal(raw, &instances); err != nil {
			// Same rule one level down: a repo whose records will not parse
			// names no sessions, so continuing here would indict every session
			// it owns.
			return nil, fmt.Errorf("session records for %s could not be parsed: %w", repoID, err)
		}
		for _, inst := range instances {
			if inst.TmuxName != "" {
				names[inst.TmuxName] = true
			} else if inst.Title != "" {
				repoPath := inst.Path
				// Derive a missing legacy name from current repository identity only
				// when it agrees with the record's storage key. That gives a bare
				// clone's linked worktree its bare identity (#3358), while preserving
				// the raw-path name for rows retained under an older or otherwise
				// mismatched key.
				if repo, err := config.RepoFromPath(inst.Path); err == nil && repo.ID == repoID {
					repoPath = repo.IdentityPath()
				}
				names[tmux.NewTmuxSessionForRepo(inst.Title, repoPath, "").SanitizedName()] = true
			}
			for _, tab := range inst.Tabs {
				if tab.TmuxName != "" {
					names[tab.TmuxName] = true
				}
			}
		}
	}
	return names, nil
}

// liveTmuxHomes returns the AF homes that live tmux sessions claim, or an error
// when that set cannot be established.
//
// The error matters more here than anywhere else in this file: this set is the
// ONLY guard between a temp home with live tmux sessions and an rm -rf, because
// such a home holds no daemon lock for the authoritative probe to find. An
// incomplete set is indistinguishable from an empty one at the call site, so
// "incomplete" has to be an error — both when the session list is unavailable
// and when a listed session's ownership marker cannot be read.
func liveTmuxHomes(ctx *scanContext) (map[string]bool, error) {
	live, err := ctx.tmuxSessions()
	if err != nil {
		return nil, err
	}
	return unionLiveTmuxHomes(ctx, live, false)
}

// liveTmuxHomesNow is liveTmuxHomes against a FRESH listing rather than the
// run's memo, for the fix-time recheck.
//
// The memo is right for detection — one run, one view. It is wrong for the
// recheck, and the difference is the whole point of rechecking: findings are
// applied after detection, so the session that must veto an rm -rf is precisely
// the one that STARTED in that window, and it cannot appear in a listing taken
// before the window opened. Re-reading markers for the old list would miss it.
func liveTmuxHomesNow(ctx *scanContext) (map[string]bool, error) {
	live, err := liveTmuxSessionSockets(ctx, true)
	if err != nil {
		return nil, err
	}
	return unionLiveTmuxHomes(ctx, live, true)
}

// unionLiveTmuxHomes unions the AF homes the union session listing claims with
// whatever doctor's ambient server can ALSO see. The kill arms pin to the
// recorded socket generations alone — a name absent from every one of them is
// evidence of death — but the rm -rf guards this set feeds need the union, not
// the pinned view: a stale temp home's live session can sit on a socket other
// than the active home's recorded ones (its own daemon's, or the ambient server
// this process reaches), and a listing of only the recorded sockets concludes
// the home is unused and removes it from under the session claiming it. The
// union can only over-preserve a home, never over-remove one, and an unreadable
// ambient listing fails the guard the same way an unreadable pinned one does.
func unionLiveTmuxHomes(ctx *scanContext, live map[string][]string, fresh bool) (map[string]bool, error) {
	socket, _ := ctx.recordedSocket()
	// Every (name, socket) pair gets its ownership marker read: session names
	// derive from repo path and title, so a same-named session on another
	// socket can claim a DIFFERENT home — dropping the pair drops the veto
	// that keeps rm -rf away from a live session's home.
	bySocket := map[string][]string{}
	for name, sockets := range live {
		for _, sock := range sockets {
			bySocket[sock] = append(bySocket[sock], name)
		}
	}
	homes := map[string]bool{}
	for sock, names := range bySocket {
		sockHomes, err := tmuxHomesFor(ctx, names, sock)
		if err != nil {
			return nil, err
		}
		for home := range sockHomes {
			homes[home] = true
		}
	}
	if socket == "" {
		// The listing already was the ambient one — nothing more to see.
		return homes, nil
	}
	var ambient []string
	var err error
	if fresh {
		ambient, err = tmux.ListSessionNamesOnSocket(ctx.opts.Exec, "")
	} else {
		ambient, err = ctx.sessionNamesOnSocket("")
	}
	if err != nil {
		return nil, err
	}
	more, err := tmuxHomesFor(ctx, ambient, "")
	if err != nil {
		return nil, err
	}
	for home := range more {
		homes[home] = true
	}
	// A record whose socket history hit its bound has dropped a generation
	// that may still host a session claiming any home here — no home's tmux
	// use can then be ruled out, so the removal guard this feeds refuses.
	if st, ok, rerr := ctx.recordedServer(); rerr == nil && ok && st.HistoryTruncated {
		return nil, fmt.Errorf("the daemon's tmux socket history was truncated: a dropped socket " +
			"generation may still host sessions, so no home's tmux use can be ruled out")
	}
	return homes, nil
}

// tmuxHomesFor answers which AF homes the sessions in `names` claim; `socket`
// is the server `names` was listed from, so each session's marker is read on
// the server it actually lives on rather than the recorded one it may not be
// on.
func tmuxHomesFor(ctx *scanContext, names []string, socket string) (map[string]bool, error) {
	homes := map[string]bool{}
	var unreadable []string
	for _, name := range names {
		if !strings.HasPrefix(name, tmux.TmuxPrefix) {
			continue
		}
		home, present, markerErr := tmux.SessionHomeMarkerOnSocket(ctx.opts.Exec, name, socket)
		if markerErr != nil {
			// tmux did not answer for this session, so it cannot be ruled OUT as
			// the claimant of any home. A session that answered and carries no
			// marker (present=false, nil error) claims nothing and is skipped —
			// collapsing those two is the bug this function exists to avoid.
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", name, markerErr))
			continue
		}
		if present && home != "" {
			homes[normalizeHome(home)] = true
		}
	}
	if len(unreadable) > 0 {
		return nil, fmt.Errorf("live tmux session(s) would not report which AF home they belong to: %s",
			strings.Join(unreadable, "; "))
	}
	return homes, nil
}

// tmuxSessionHomeMarker asks which AF home a session belongs to, three-valued:
// (home, true, nil) it said so, ("", false, nil) it answered and claims none,
// and a non-nil error when it did not answer.
//
// It delegates to session/tmux rather than shelling out here. The version it
// replaces was `if err != nil { return "", false }`, which reported an
// unanswered query as a session that claims no home — and doctor treats "claims
// no home" as clearance to rm -rf that home (#2874). The shared probe also
// carries the timeout bound and the multiline-forgery guard this one lacked.
func tmuxSessionHomeMarker(ctx *scanContext, name, socket string) (string, bool, error) {
	return tmux.SessionHomeMarkerOnSocket(ctx.opts.Exec, name, socket)
}
