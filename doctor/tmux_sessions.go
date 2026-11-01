package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// listTmuxSessions returns every session name on the tmux server the daemon
// actually hosts this home's sessions on, or an error when tmux could not tell
// us.
//
// THE INVARIANT, stated rather than borrowed: A FAILED READ IS NOT AN EMPTY
// RESULT. Every consumer below treats a name's ABSENCE as evidence the session
// is dead, and two of them spend that evidence on a kill or an rm -rf.
//
// It DELEGATES rather than shelling out — the second read in this file to do so,
// after tmuxSessionHomeMarker, and for the same reason. The copy that lived here
// fell outside both invariants session/tmux maintains:
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
func listTmuxSessions(ctx *scanContext) ([]string, error) {
	socket, _ := ctx.recordedSocket()
	return tmux.ListSessionNamesOnSocket(ctx.opts.Exec, socket)
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

// sessionDeathCorroborated reports whether the run's tmux session listing is
// evidence that the session hosting p is genuinely dead — the standard the
// orphaned-process kill arm must meet before it arms a kill.
//
// It is true when the listing was pinned to the daemon's recorded socket (the
// server that actually hosts this home's sessions): a name absent from THAT
// listing is absent from the server, so p's session is dead. It is false when a
// daemon record exists but doctor cannot pin the listing to the socket it names
// (unreadable, or names no socket): the ambient listing may then be looking at a
// different server, so a live session on the daemon's (unreadable) socket would
// be absent from it and read as a verified orphan. The kill stays report-only.
//
// The record-absent state (no tmux-server-state.json AND no daemon currently
// serving this home) is the historical "this home never ran the record-writing
// daemon" reading under which the ambient listing is the only evidence. But
// absence is not proof the daemon never ran: the daemon may have hosted
// sessions and then died (leaving sessions live on a socket the record never
// named), macOS never writes a record at all, and recordServerState silently
// ignores write failures. In that state the ambient listing corroborates p's
// session death ONLY when p's own TMUX env (set by tmux on every pane process)
// names the SAME socket the ambient listing resolves to — proving the ambient
// server IS the one that hosted p's session. A p whose TMUX env names a
// different socket (a live session on a non-ambient server reads dead here,
// the #6271 wrong-server kill) or no socket at all cannot be proven dead from
// the ambient listing, so it is reported, not killed — the same rule the
// unreadable-record guard enforces, narrowed to the processes the ambient
// listing actually covers.
func (c *scanContext) sessionDeathCorroborated(p proctree.Process) bool {
	_, kind := c.recordedSocket()
	switch kind {
	case recordedSocketPinned:
		return true
	case recordedSocketAbsent:
		return ambientListingCoversProcess(p)
	default:
		return false
	}
}

// ambientListingCoversProcess reports whether the ambient tmux listing (the
// unpinned `tmux ls` doctor runs when no daemon record exists) names the same
// server that hosted p's session. Every pane process carries a TMUX env of the
// form "socketPath,serverPID,sessionIdx" set by tmux itself, so the socket p
// records is the server that actually hosted its session. Only when that
// socket agrees with the socket a bare `tmux` resolves to in doctor's own
// environment (tmux.DefaultSocketPath) is the session's absence from the
// ambient listing evidence the session is dead, rather than evidence the
// listing looked at the wrong server. A p with no readable TMUX env (the
// marker was inherited, but tmux's was cleared or never set) is not provably
// dead from the ambient listing, so it is reported rather than killed.
func ambientListingCoversProcess(p proctree.Process) bool {
	tmuxEnv, status := proctree.LookupEnv(p.PID, "TMUX")
	if status != proctree.EnvFound || tmuxEnv == "" {
		return false
	}
	procSocket, _, _ := strings.Cut(tmuxEnv, ",")
	procSocket = strings.TrimSpace(procSocket)
	if procSocket == "" {
		return false
	}
	return sameSocket(procSocket, tmux.DefaultSocketPath())
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
// daemon's recorded socket so the escaped-process arm reads the SAME server
// the session listing named the session live on. Against doctor's ambient
// (wrong) server a session the listing proved live answers
// ErrSessionVanishedBeforeCapture, which clears sessionLive and falls through
// to the orphaned-process kill arm for a process that simply belongs to a live
// session on another socket — so the pane capture must be pinned whenever the
// listing is.
func captureSessionProcessTrees(ctx *scanContext, name string) ([]proctree.Process, error) {
	socket, _ := ctx.recordedSocket()
	return tmux.CaptureSessionProcessTreesOnSocket(ctx.opts.Exec, name, socket)
}

// tmuxSessions returns the run's memoized session listing, or the error that
// made it unavailable. A caller that treats a missing name as proof the session
// is DEAD must handle the error and do nothing; see the scanContext field
// comments. The doctor run is sequential, so a plain memo is enough (mirrors
// daemonProcs).
func (c *scanContext) tmuxSessions() ([]string, error) {
	if !c.tmuxScanned {
		c.tmuxNames, c.tmuxErr = listTmuxSessions(c)
		c.tmuxScanned = true
	}
	return c.tmuxNames, c.tmuxErr
}

// checkTmuxInspection turns the run's session listing into a report row, and is
// the ONE place its failure is announced — the checks that consume the list then
// stay silent rather than each repeating it, exactly as they do for
// checkProcessInspection and an unreadable process table (#1939).
//
// It must run BEFORE those checks, or the operator reads their silence before
// learning why they were silent.
func checkTmuxInspection(ctx *scanContext, report *Report) {
	names, err := ctx.tmuxSessions()
	if err == nil {
		report.Pass(sectionProcesses, "tmux-inspection",
			fmt.Sprintf("listed the tmux server's sessions (%d)", len(names)))
		return
	}
	report.Fail(sectionProcesses, "tmux-inspection",
		fmt.Sprintf("cannot list tmux sessions, so the orphaned-process, escaped-process, runaway-child, "+
			"leaked-session and stale-temp-home checks below could not establish which sessions are live: %v", err),
		"those checks are UNKNOWN, not clean — nothing was reported as orphaned or abandoned and `--fix` "+
			"proposed no kill or removal derived from session liveness on this run. Restore access to the "+
			"tmux server and re-run")
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
	names, listErr := ctx.tmuxSessions()
	if ctx.snap == nil || listErr != nil {
		return
	}
	unmeasurable := 0
	var blindSessions []string
	for _, name := range names {
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
		procs, paneErr := captureSessionProcessTrees(ctx, name)
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
	names, listErr := ctx.tmuxSessions()
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
	for _, name := range names {
		if strings.HasPrefix(name, tmux.TmuxPrefix) && !recorded[name] {
			leaked = append(leaked, name)
		}
	}
	sort.Strings(leaked)
	// The leaked-session listing is pinned to the daemon's recorded socket
	// when one was readable, so the manual remedy must target THAT server: a bare
	// `tmux kill-session -t ...` resolves doctor's own $TMUX/$TMUX_TMPDIR and would
	// miss the session (or, on a shared name, kill an unrelated one on the
	// ambient server) — the same wrong-server divergence the listing pinning
	// closed. With no record the listing was ambient, so the remedy stays ambient.
	socket, kind := ctx.recordedSocket()
	for _, name := range leaked {
		origin := "no ancestry marker"
		ownedByActiveHome := false
		switch home, present, markerErr := tmuxSessionHomeMarker(ctx, name); {
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
		killCmd := shellsuggest.Command("tmux", "kill-session", "-t", "="+name+":")
		if kind == recordedSocketPinned {
			// "=name:" is tmux's exact-match target syntax — one argument, so
			// it is one piece the seam quotes as a whole. `-S <socket>` pins
			// the remedy to the server the listing found the session on.
			killCmd = shellsuggest.Command("tmux", "-S", socket, "kill-session", "-t", "="+name+":")
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
	names, err := ctx.tmuxSessions()
	if err != nil {
		return nil, err
	}
	return unionLiveTmuxHomes(ctx, names, false)
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
	names, err := listTmuxSessions(ctx)
	if err != nil {
		return nil, err
	}
	return unionLiveTmuxHomes(ctx, names, true)
}

// unionLiveTmuxHomes unions the AF homes the recorded-socket listing `names`
// claims with whatever doctor's ambient server can ALSO see. The kill arms pin
// to the recorded socket alone — a name absent from THAT listing is evidence
// of death — but the rm -rf guards this set feeds need the union, not the
// pinned view: a stale temp home's live session can sit on a socket other than
// the active home's recorded one (its own daemon's, or the ambient server this
// process reaches), and a listing of only the recorded socket concludes the
// home is unused and removes it from under the session claiming it. The union
// can only over-preserve a home, never over-remove one, and an unreadable
// ambient listing fails the guard the same way an unreadable pinned one does.
func unionLiveTmuxHomes(ctx *scanContext, names []string, fresh bool) (map[string]bool, error) {
	socket, _ := ctx.recordedSocket()
	homes, err := tmuxHomesFor(ctx, names, socket)
	if err != nil {
		return nil, err
	}
	if socket == "" {
		// The listing already was the ambient one — nothing more to see.
		return homes, nil
	}
	var ambient []string
	if fresh {
		ambient, err = tmux.ListSessionNamesOnSocket(ctx.opts.Exec, "")
	} else {
		ambient, err = ctx.ambientTmuxSessions()
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
	return homes, nil
}

// ambientTmuxSessions returns the run's memoized listing of doctor's AMBIENT
// tmux server — the second half of the union the home-removal guards need, kept
// as one view for detection the way tmuxSessions is one view for liveness. The
// fix-time recheck bypasses this memo for a fresh listing for the same reason
// it bypasses the pinned one.
func (c *scanContext) ambientTmuxSessions() ([]string, error) {
	if !c.tmuxAmbientScanned {
		c.tmuxAmbientNames, c.tmuxAmbientErr = tmux.ListSessionNamesOnSocket(c.opts.Exec, "")
		c.tmuxAmbientScanned = true
	}
	return c.tmuxAmbientNames, c.tmuxAmbientErr
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
func tmuxSessionHomeMarker(ctx *scanContext, name string) (string, bool, error) {
	socket, _ := ctx.recordedSocket()
	return tmux.SessionHomeMarkerOnSocket(ctx.opts.Exec, name, socket)
}
