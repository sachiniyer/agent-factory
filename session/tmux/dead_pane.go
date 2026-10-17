package tmux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// Held dead panes (#4479, #4506 review). A process tab runs under
// remain-on-exit, so when its command finishes tmux keeps the pane, and keeps
// reporting its pane_pid. Measured on tmux 3.4:
//
//	command                                   pane_dead  status  signal  time  pane_pid
//	sh -c 'exit 7'                            1          7               set   absent from the process table
//	sh -c 'kill -9 $$'                        1                  9       set   absent
//	root closes its tty, keeps running        1                                alive, child of the tmux server
//	sh -c 'bg-child & exit 3'                 1          3               set   absent; the child keeps the root's SID
//
// So pane_dead alone does not prove the command exited: tmux marks a pane dead
// when its terminal closes, and reports a status, a signal or a death time only
// after it has reaped the root. Every reader below keys on that second fact.
//
// Once tmux has reaped the root, the kernel may hand its pid to an unrelated
// process. That is why a reaped pane's pid is never waited on or signalled. Its
// kernel session is still usable, though: a pid stays reserved while any process
// still has it as a session id, so while the pid is not a live process, every
// process whose SID equals it was started inside that pane — unless it called
// setsid, in which case it holds its own SID and is invisible to the SID scan.
// deadPaneMarkerSurvivors closes that gap with an AF_SESSION/AF_HOME marker scan.

// paneFieldSeparator keeps pane fields positional. tmux prints an unset field as
// nothing, so a whitespace split would shift later fields left. Every field is
// numeric, a pty name, or a predicate digit — none can contain the separator.
const paneFieldSeparator = "|"

// paneRowFormat is what the teardown asks list-panes and display-message for.
// The third field concatenates every value tmux reports only after reaping the
// root, so it is non-empty exactly when the root is known to be gone. The
// fourth carries the pane's @af_pane_pid identity stamp: tmux ≥3.8 empties
// pane_pid once the root exits (#5319), and the stamp is how a held dead pane
// still names its root. The last three fields are the stamp's freshness proof
// — the deaths counter, the live pty name, and the command pin — because a
// pane option outlives the pane incarnation that wrote it (see paneStampPID).
//
// A reply without the separator is a bare pane_pid, which is what a tmux that
// cannot expand the later fields, or a test executor, returns. It reads as a
// live pane, the behaviour from before #4479.
const paneRowFormat = "#{pane_pid}" + paneFieldSeparator + "#{pane_dead}" + paneFieldSeparator +
	"#{pane_dead_status}#{pane_dead_signal}#{pane_dead_time}" + paneFieldSeparator + "#{" + PanePidMarker + "}" +
	paneFieldSeparator + "#{" + paneDeathsMarker + "}" + paneFieldSeparator + "#{pane_tty}" +
	paneFieldSeparator + paneCommandMatchField

// paneCommandMatchField asks the server — not the row — whether the pane's
// recorded start command is still the exact command the stamp pinned: one
// digit, answered by == equality rather than a substring match, so a foreign
// respawn whose argv merely contains a launch marker still answers 0. The
// comparison runs server-side because the recorded command is free-form text
// that can contain both spaces and the field separator.
const paneCommandMatchField = "#{==:#{" + paneCommandMarker + "},#{pane_start_command}}"

// paneRow is one pane of a list-panes or display-message reply.
type paneRow struct {
	pid int
	// dead: tmux holds the pane after its terminal closed.
	dead bool
	// reaped: tmux collected the root, so the pid no longer names it.
	reaped bool
}

func parsePaneRow(field string) (paneRow, error) {
	parts := strings.Split(strings.TrimSpace(field), paneFieldSeparator)
	row := paneRow{}
	if len(parts) > 1 {
		row.dead = strings.TrimSpace(parts[1]) == "1"
	}
	if len(parts) > 2 {
		row.reaped = row.dead && strings.TrimSpace(parts[2]) != ""
	}
	// The stamp is a fallback authority only, consulted when tmux itself can
	// no longer answer: a held dead pane on tmux ≥3.8 reports the pane_pid
	// FIELD EMPTY. A populated pane_pid is tmux's own maintained value and
	// always wins — after a respawn-pane the new root has a different pid
	// while the old stamp is simply stale — so the two are never compared
	// against each other, only substituted. A malformed pane_pid stays
	// malformed; only an empty one is the tmux ≥3.8 shape the stamp exists
	// to cover. And the stamp counts only while it still names the pane's
	// current incarnation — pane options outlive respawn-pane, so paneStampPID
	// proves the stamp was written by the run that most recently died.
	stampPID, verdict := 0, stampUnusable
	if len(parts) > 3 {
		var deaths, tty, cmdMatch string
		if len(parts) > 4 {
			deaths = strings.TrimSpace(parts[4])
		}
		if len(parts) > 5 {
			tty = strings.TrimSpace(parts[5])
		}
		if len(parts) > 6 {
			cmdMatch = strings.TrimSpace(parts[6])
		}
		stampPID, verdict = paneStampPID(strings.TrimSpace(parts[3]), deaths, tty, cmdMatch)
	}
	panePIDRaw := strings.TrimSpace(parts[0])
	pid, err := strconv.Atoi(panePIDRaw)
	switch {
	case err == nil:
		// pane_pid answered; the stamp is never consulted.
	case panePIDRaw == "" && verdict == stampFresh:
		pid = stampPID
	case panePIDRaw == "" && row.dead && verdict == stampPending:
		return paneRow{}, fmt.Errorf("dead pane carries a %s stamp for root %d whose incarnation proof is one counter short — tmux reports pane_dead before its queued pane-died hook increments the deaths counter, so the hook has not counted the death yet and the process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion: %w", PanePidMarker, stampPID, errPaneStampPending)
	case panePIDRaw == "" && row.dead && stampPID > 0:
		return paneRow{}, fmt.Errorf("dead pane carries a %s stamp for root %d from an earlier incarnation — the pane was respawned or its stamp write failed, so its process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion", PanePidMarker, stampPID)
	case panePIDRaw == "" && row.dead:
		return paneRow{}, fmt.Errorf("dead pane reports no pane_pid and no %s stamp; it predates pid stamping or was not created by af, so its process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion", PanePidMarker)
	default:
		return paneRow{}, fmt.Errorf("invalid pane pid %q", field)
	}
	row.pid = pid
	return row, nil
}

// paneStampVerdict is paneStampPID's ruling on the stamp read from one row:
// how far the incarnation proof got, and specifically whether its only
// shortfall is a counter the queued pane-died hook has not run yet (#5335).
type paneStampVerdict int

const (
	// stampUnusable means the stamp names nobody: unset, malformed, or a
	// non-positive pid — the pane reads exactly like an unstamped one.
	stampUnusable paneStampVerdict = iota
	// stampStale means a readable stamp that cannot name the pane's current
	// incarnation: a deaths gap that is not 0-or-1, a moved pty, or a
	// replaced start command — a respawn, or a stamp write that never
	// landed.
	stampStale
	// stampPending means every freshness input matches except the deaths
	// counter, whose gap is exactly 0 — the pane-died hook has not counted
	// this death yet, not a stale record. tmux reports pane_dead before the
	// queued hook runs (#5335), so a reader that can wait re-reads the row
	// until the hook lands; one that cannot must treat the stamp as still
	// unproven, never as proven-false.
	stampPending
	// stampFresh means the stamp names the run that most recently died:
	// deaths gap exactly one, pty and command pins intact.
	stampFresh
)

// errPaneStampPending marks a dead-pane row whose stamp is provable on every
// input but the deaths counter — the pane-died hook has not run yet (#5335).
// panePID re-reads on this error instead of refusing; by the time it escapes,
// the wait deadline has passed and the message stands as the refusal.
var errPaneStampPending = errors.New("the pane-died hook has not counted the death")

// paneStampPID extracts the pane-root pid a @af_pane_pid stamp recorded —
// "<pid>:<deaths>:<pane_tty>" — and rules on whether it still names the
// pane's current incarnation. The pane option outlives the run that wrote it
// (respawn-pane keeps the pane struct and its options), so three proofs must
// all hold before the pid is trusted:
//
//   - deathsNow == stamped deaths + 1: the pane-died hook bumps the counter
//     on every natural root exit, so a stamp from an earlier run shows a
//     wider gap. A gap of exactly 0 is the one case that is NOT stale: the
//     hook runs on tmux's command queue and pane_dead is observable first
//     (#5335), so a fresh pane's own death can still be uncounted — that is
//     stampPending, and it resolves when the queued hook runs.
//   - stamp tty == the pane's reported tty: every respawn allocates a fresh
//     pty. The name is only corroborating evidence — `respawn-pane -k` frees
//     the devpts index before allocating the replacement and Linux can
//     reissue the same pathname to the new allocation (#5328 review), so a
//     matching name alone proves nothing. What normally makes the stale
//     record unreachable for a re-run is the shim's restamp-or-die
//     (sessionenv.stampPaneRootPid): a respawn re-executes the shim, which
//     either lands a fresh bundle or, when the option provably still names
//     an earlier run, exits without running the command. The accepted
//     residual is the shim that can neither write nor read the option at
//     all — manual `respawn-pane -e` sabotage of the pane env — which
//     launches anyway because a first launch must never die over
//     bookkeeping; a stale record that survives these pins is bounded to
//     the stamped session's own earlier run.
//   - cmdMatch == "1": the pane's recorded start command still byte-equals
//     the command the stamp pinned, so a replacement respawn — even one
//     whose argv happens to contain a launch marker — fails it, while an
//     identical commandless re-run re-executed the shim and restamped.
//
// A stamp written before the incarnation proof existed (a bare pid, or a
// malformed bundle) has no second colon and fails closed like an unstamped
// pane. A deaths field left empty at stamp time is the counter's unset
// state, i.e. zero.
func paneStampPID(stamp, deathsNow, ttyNow, cmdMatch string) (int, paneStampVerdict) {
	pidRaw, rest, hasRest := strings.Cut(stamp, ":")
	pid, err := strconv.Atoi(pidRaw)
	if err != nil || pid <= 0 {
		return 0, stampUnusable
	}
	if !hasRest {
		// A bare pid is a stamp written before the incarnation proof
		// existed: readable, but unprovable — it fails closed like an
		// unstamped pane, only with a nameable pid.
		return pid, stampStale
	}
	deathsRaw, ttyRaw, ok := strings.Cut(rest, ":")
	if !ok {
		return pid, stampStale
	}
	stampedDeaths := 0
	if deathsRaw != "" {
		stampedDeaths, err = strconv.Atoi(deathsRaw)
		if err != nil || stampedDeaths < 0 {
			return pid, stampStale
		}
	}
	current := 0
	if deathsNow != "" {
		current, err = strconv.Atoi(deathsNow)
		if err != nil || current < 0 {
			return pid, stampStale
		}
	}
	pins := ttyRaw == ttyNow && cmdMatch == "1"
	switch gap := current - stampedDeaths; {
	case gap == 1 && pins:
		return pid, stampFresh
	case gap == 0 && pins:
		return pid, stampPending
	default:
		return pid, stampStale
	}
}

// deadPaneProcesses resolves one held dead pane for the process capture.
//
// asLive is true when the pid should be handled exactly like a live pane root:
// tmux has not reaped it, so it is still the pane's process. procs are the
// processes that outlived a root that is gone. A nil procs with asLive false
// means nothing of this pane can still be running. An error means the pane's
// process set could not be established, and the capture reports it as such.
//
// markerSurvivors is the session-wide result of deadPaneMarkerSurvivors, computed
// once per capture (see CaptureSessionProcessTrees) and shared across every dead
// pane — the marker criteria and the snapshot it scans do not vary by pane. A nil
// markerSurvivors means the scan could not run; the caller reports that failure
// once per capture as incompleteness, and the SID members and verified live tree
// the pre-existing logic established without the scan are still returned (the
// partial capture a Close that discards captureErr still reaps), so the per-pane
// error is only the pid-reuse determination, which drops the pane (#5345).
func deadPaneProcesses(snap map[int]proctree.Process, sanitizedName string, row paneRow, markerSurvivors []proctree.Process) (procs []proctree.Process, asLive bool, err error) {
	_, present := snap[row.pid]
	switch {
	case present && row.reaped:
		// Reaped, and the pid is live again: the kernel reused it. The old
		// assumption that every survivor held the pid as its session id — and
		// so kept the pid from being reused — breaks for a helper the pane
		// command started with setsid(): that helper holds its own SID and
		// reserves nothing, so the root's pid can be reused underneath a marked
		// helper that is still alive. Run the marker scan before declaring
		// nothing survives; the scan's empty result is the positive evidence the
		// recycled pid alone can no longer provide. Same-SID members are not
		// consulted here: any process holding the pid as its SID would have
		// kept it from being reused, so with the pid reused none of this pane
		// is among them.
		log.InfoLog.Printf("tmux session %s: dead pane pid %d now names another process; not capturing it",
			sanitizedName, row.pid)
		return markerSurvivors, false, nil
	case present:
		// Not reaped, so tmux still holds the pid as its child: normally the
		// root, still running after its terminal closed. tmux older than 3.3
		// never reports a signal death, though, so there the pid may have been
		// reaped and reused. A launch marker naming a different session proves
		// that. Without one, the live-root checks decide.
		if marker, status := proctree.LookupEnv(row.pid, EnvMarkerSession); status == proctree.EnvFound && marker != sanitizedName {
			log.InfoLog.Printf("tmux session %s: dead pane pid %d belongs to session %s; not capturing it",
				sanitizedName, row.pid, marker)
			// The pid was reused by a process from another session, so it is
			// not this pane's root and the tree/SID scan below it are not ours.
			// On tmux < 3.3 a dead pane's pid can be reused without reaping,
			// so a setsid'd helper this pane started can still be alive with this
			// session's markers while the pid itself is foreign; the marker scan
			// is the only path that recovers it, and it is scoped to this
			// session's generation so it cannot adopt the reused pid's session.
			return markerSurvivors, false, nil
		}
		// The root is still alive, so the caller captures its PPID tree and
		// original SID below (asLive). A helper that already daemonized with
		// setsid plus a double-fork is reparented outside that tree and holds a
		// different SID, so neither the tree nor the SID scan recovers it; the
		// marker scan catches it the same way it does on the reaped path. The
		// root and its in-tree descendants already carry the pane's markers and
		// are deduplicated by the caller, so combining here only adds the
		// escapee. When the marker scan could not run, asLive is still returned so
		// the caller captures the verified tree and SID members; the escapee scan
		// is the additional capture the caller reports as incompleteness.
		return markerSurvivors, true, nil
	}
	members := proctree.SessionMembers(snap, row.pid)
	// SessionMembers matches only processes whose SID equals the dead root's
	// pid. A helper the pane command started with setsid() holds its own SID,
	// and once the reaped root reparents it to init no ppid walk can recover it
	// either — so it is invisible to SessionMembers whether or not same-SID
	// members exist. Finding SID members does not prove there are no additional
	// marker-only survivors, so the marker scan runs unconditionally and its
	// results are combined with the SID members on every survivor-returning path
	// below. Every process spawned inside the pane inherits AF_SESSION and AF_HOME
	// via the pane environment, and /proc/<pid>/environ preserves them after
	// reparenting and setsid, so a marker scan is the positive evidence the SID
	// scan cannot provide. An empty scan means nothing else of the pane survives;
	// a hit is a survivor the capture must include so the teardown reaps it
	// rather than deleting the worktree underneath it. When the marker scan could
	// not run, markerSurvivors is nil and the SID members below are still returned
	// as the partial capture the pre-existing logic established; the escapee scan's
	// incompleteness is reported by the caller, not a reason to drop the pane.
	if len(members) == 0 || pidGone(row.pid) {
		return append(members, markerSurvivors...), false, nil
	}
	// The snapshot did not list the pid, but something holds it now.
	_, lookupErr := proctree.Lookup(row.pid)
	switch {
	case errors.Is(lookupErr, proctree.ErrProcessExited) && !row.reaped:
		// The snapshot leaves out zombies. tmux has not collected the root yet,
		// so the zombie is the root, and it still reserves the pid.
		return append(members, markerSurvivors...), false, nil
	case lookupErr == nil || errors.Is(lookupErr, proctree.ErrProcessExited):
		// The pid was reused after the snapshot, so the pane's own session
		// members were already gone by then (their reservation is what kept the
		// pid free only while they lived). A setsid helper can still survive, so
		// the marker scan alone is the captured set; it can otherwise hold only
		// exited processes or the new owner's session, and neither is ours.
		log.InfoLog.Printf("tmux session %s: dead pane pid %d was reused during capture; not capturing its session",
			sanitizedName, row.pid)
		return markerSurvivors, false, nil
	case pidGone(row.pid):
		return append(members, markerSurvivors...), false, nil
	default:
		return nil, false, fmt.Errorf("cannot tell whether dead pane pid %d was reused: %w", row.pid, lookupErr)
	}
}

// deadPaneMarkerSurvivors scans the process-table snapshot for processes that
// carry this session's AF_SESSION and AF_HOME markers but escaped the SID scan
// in deadPaneProcesses — the setsid() escapee the reaper was built to catch
// (#1104/#802). A setsid'd helper holds its own kernel session (SID = its own
// pid), and once the reaped pane root reparents it to init, no ppid walk can
// recover it either. SessionMembers returns empty for it, and without a marker
// scan the capture would treat that empty as conclusive proof nothing survives.
//
// The scan mirrors the one the vanished-session sweep already uses
// (refreshOrphanCandidates/markedOrphanProcesses): every process spawned inside
// the pane inherits AF_SESSION and AF_HOME via the pane environment, and
// /proc/<pid>/environ preserves them after reparenting and setsid. AF_HOME
// scopes the match to this agent-factory install so a same-named session from
// another home is not adopted. AF_SESSION_GEN scopes it to the session's own
// generation: a tmux session name is reused across incarnations (the same
// project and title give the same af_<hash>_<title>), so a teardown of
// incarnation N must not reap a process incarnation N+1 spun up under the same
// name and home — the heal's recreate is the load-bearing case (#5345). The
// generation set is the tearing-down session's own AF_SESSION_GEN, read before
// capture; a process carrying a generation the set does not contain is not a
// survivor, and a process carrying none is adopted only when the session
// carried none (the legacy case). An unreadable generation is a capture error,
// not an empty result: the scan could not prove what it was asked to prove.
//
// This process's own chain is excluded — reaping the running af or an ancestor
// is never the operation the caller asked for — and so is the agent-factory
// daemon's subtree, which setsids out of the pane session and inherits its
// markers while being a shared process no single pane teardown may reap (see
// deadPaneDaemonSubtree). A shared tmux server is excluded the same way
// (proctree.IsTmuxProcess): af starting a server from inside a pane inherits
// that pane's AF_SESSION/AF_HOME/AF_SESSION_GEN, and on Linux the server is moved
// into a separate systemd scope, so it is neither in this process's self chain
// nor the daemon subtree. Without this exclusion the marker scan would admit the
// server as a survivor and the teardown would signal it, terminating every
// session on that server — the same exclusion the worktree reaper applies
// (session/git/worktree_reap). A foreign-user-owned process is excluded too: the
// markers are environment strings anyone can copy, the same ownership gate
// markedOrphanProcesses already enforces for the vanished-session sweep.
//
// The teardown requester is NOT excluded here. It stays in the captured set so
// reapSessionProcesses can spare it per-signal while its registration holds and
// reconsider it if the reprieve ends mid-reap — the same dynamic the requested-
// teardown path already relies on (close.go). Dropping it here would permanently
// remove a still-alive requester the background reaper could otherwise have
// terminated.
//
// An empty result is positive evidence that nothing of the pane survives: the
// scan examined every process in the snapshot and found none carrying this
// session's markers. A non-empty result is the set of survivors the capture
// must include. An error means the scan could not be performed (e.g. the
// agent-factory home could not be resolved), and the capture reports it rather
// than treating it as an empty result — the same standard every other
// unprovable survivor in this file meets.
func deadPaneMarkerSurvivors(snap map[int]proctree.Process, sanitizedName string, generations orphanGenerationSet) ([]proctree.Process, error) {
	ownHome, err := afHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot attribute dead pane survivors of session %s to this agent-factory home: %w",
			sanitizedName, err)
	}
	cleanHome := filepath.Clean(ownHome)
	selfUID, selfUIDKnown := proctree.UID(os.Getpid())
	if !selfUIDKnown {
		return nil, fmt.Errorf("cannot determine teardown process uid for dead pane marker scan of session %s",
			sanitizedName)
	}
	selfChain := selfAndAncestorProcesses(snap)
	daemonOwned := deadPaneDaemonSubtree(snap, cleanHome)
	var survivors []proctree.Process
	for _, process := range snap {
		if selfChain[process.PID] || daemonOwned[process.PID] || proctree.IsTmuxProcess(process.PID) {
			// The tmux server is the case that matters; see deadPaneMarkerSurvivors.
			continue
		}
		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			// An unreadable environ is not evidence of membership, and on a
			// hardened host every process may be unreadable — treating each as
			// a blocker would make successful absence impossible. The same
			// trade refreshOrphanCandidates makes: ignore failures on
			// processes the capture did not already place.
			continue
		}
		session, hasSession := processEnvValue(environ, EnvMarkerSession)
		if !hasSession || session != sanitizedName {
			continue
		}
		home, hasHome := processEnvValue(environ, EnvMarkerHome)
		if !hasHome {
			continue
		}
		if filepath.Clean(home) != cleanHome {
			continue
		}
		// AF_SESSION_GEN scopes the match to this session's incarnation so a
		// teardown of one incarnation does not adopt a process a same-named
		// successor already spun up (#5345): the heal reaps a dead root and
		// recreates under the same title, and the new pane carries the same
		// AF_SESSION/AF_HOME but a fresh generation. A generation the
		// tearing-down session does not carry is not this pane's, and a
		// generation the session carries but the process does not is a
		// pre-generation or hardened-environment process the sweep's own
		// legacy handling covers, not this scan's — the dead-pane path names
		// one generation, so a missing marker is adopted only when the session
		// itself carried none.
		if !generations.contains(environ) {
			continue
		}
		uid, uidKnown := proctree.UID(process.PID)
		if !uidKnown || uid != selfUID {
			// The markers are environment strings anyone can copy. Under a
			// privileged af (notably root) a foreign user's process could
			// carry them and be reaped without this check, the same ownership
			// gate markedOrphanProcesses already enforces for the
			// vanished-session sweep. An unreadable uid is not evidence of
			// membership — treat it like an unreadable environ and skip.
			continue
		}
		survivors = append(survivors, process)
	}
	return survivors, nil
}

// deadPaneDaemonSubtree returns the agent-factory daemon this home launched
// plus its ppid-descendants, so deadPaneMarkerSurvivors can leave them out of a
// dead pane's survivors. The daemon setsids to outlive the pane that
// autostarted it (see daemon.process.startDaemonChild), and the helpers it
// forks — watch tasks copy os.Environ — inherit that pane's AF_SESSION/AF_HOME
// while being neither pane descendants nor SID members: exactly the marker-only
// attribution this scan was built to catch, and exactly the shared process a
// single pane's teardown must never reap. The daemon PID file names the
// process; isAgentFactoryDaemonArgv (mirrored from daemon.process, which this
// package cannot import without a cycle) guards against a stale entry whose PID
// the kernel recycled onto an unrelated process, so a foreign PID is never
// excluded. Returns an empty set when no live af daemon is named.
func deadPaneDaemonSubtree(snap map[int]proctree.Process, ownHome string) map[int]bool {
	excluded := make(map[int]bool)
	daemonPID, ok := readDaemonPIDFileNoFollow(filepath.Join(ownHome, "daemon.pid"))
	if !ok || !isAgentFactoryDaemonArgv(proctree.Argv(daemonPID)) {
		return excluded
	}
	for _, p := range proctree.TreeOf(snap, daemonPID) {
		excluded[p.PID] = true
	}
	return excluded
}

// readDaemonPIDFileNoFollow reads this home's daemon.pid through a descriptor
// opened O_NOFOLLOW|O_NONBLOCK and validated by fstat, mirroring the daemon
// package's readManagedFileNoFollow (daemon.process): the marker scan runs
// synchronously from CaptureSessionProcessTrees before kill-session, so a
// FIFO, device, or symlink swapped in for the PID file must not hold the
// capture there the way it would block a plain os.ReadFile. A missing,
// malformed, oversized, or non-regular entry reports no daemon rather than an
// error, so an unreadable PID file leaves the scan unchanged.
func readDaemonPIDFileNoFollow(path string) (int, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64 {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || int64(len(data)) > 64 {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// isAgentFactoryDaemonArgv reports whether argv identifies an agent-factory
// daemon process: argv[0] is the "af"/"agent-factory" binary carrying a
// "--daemon" flag. Mirrored from daemon.process.isAgentFactoryDaemon because
// daemon imports session/tmux, so this package cannot import daemon without a
// cycle (the same trade afHomeDir makes with config). A "--daemon=false"
// client is explicitly not a daemon.
func isAgentFactoryDaemonArgv(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch filepath.Base(args[0]) {
	case "af", "agent-factory":
	default:
		return false
	}
	for _, a := range args {
		if a == "--daemon" {
			return true
		}
		if value, ok := strings.CutPrefix(a, "--daemon="); ok {
			if enabled, err := strconv.ParseBool(value); err == nil {
				return enabled
			}
			return true
		}
	}
	return false
}

// pidGone reports whether no process has this pid. EPERM means one does.
func pidGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// paneRowHasNoPane recognizes the reply a missing session produces on this
// multi-field format: every pane-sourced field empty. Field 1 — pane_dead —
// is the discriminator because it is a boolean tmux always answers for a real
// pane, while on an empty target it stays empty; the only non-empty field in
// the missing-session reply is the trailing command-equality digit, which is
// "1" even there (empty == empty). Measured on tmux 3.8: `||||||1`. A bare
// pane_pid reply has no separator at all and is not this shape.
func paneRowHasNoPane(trimmed string) bool {
	parts := strings.Split(trimmed, paneFieldSeparator)
	return len(parts) > 1 && strings.TrimSpace(parts[1]) == ""
}

// panePID reads the session's pane pid and whether that pane is held dead. It
// must run before kill-session, because afterwards there is nothing to ask.
//
// A stamped dead pane whose freshness inputs all match but whose deaths gap
// is 0 is re-read until the pane-died hook lands — tmux reports pane_dead
// before the queued hook increments the counter (#5335), so a gap of 0 is a
// hook still running, never a stale stamp. The re-read is bounded by
// paneDeathsWait, and each read inside it by what is left of that wait, so a
// hook that never runs converts back into the same bounded refusal a stale
// stamp gets rather than holding the teardown.
//
// The wait also nudges the server, because pane-died fires when the exit
// status is collected, not when pane_dead is marked: on an Ubuntu-class
// tmux the root's own SIGCHLD can be lost in utempter's SIG_DFL window
// (#4682), which leaves pane_dead set forever with no collection and no
// hook — measured ~1/4 of held deaths on a loaded 3.4. On a server that
// merely queued the hook the nudge is a harmless trivial job.
func (t *TmuxSession) panePID() (paneRow, error) {
	row, err := t.readPaneRow(tmuxCommandTimeout)
	deadline := time.Now().Add(paneDeathsWait)
	pause := 5 * time.Millisecond
	nudges := 0
	for errors.Is(err, errPaneStampPending) {
		budget := time.Until(deadline)
		if budget <= 0 {
			break
		}
		if nudges < maxReapNudges {
			nudges++
			t.nudgeReap(budget)
		}
		time.Sleep(min(pause, budget))
		pause = min(2*pause, 50*time.Millisecond)
		if budget = time.Until(deadline); budget <= 0 {
			// The sleep consumed what was left of the wait: keep the
			// pending verdict — an expired read would swap it for
			// ErrTmuxTimeout and misreport a healthy server as wedged.
			break
		}
		row, err = t.readPaneRow(budget)
	}
	return row, err
}

// readPaneRow performs one panePID read within budget.
func (t *TmuxSession) readPaneRow(budget time.Duration) (paneRow, error) {
	// exactTarget forces an exact session match, mirroring ExistsOrUnknown.
	// (The bare `=name` form returns an empty pane_pid for display-message —
	// the trailing `:` in exactTarget is what makes the pid resolve. See #1006.)
	//
	// Bounded by tmuxCommandTimeout (#1917): this is the FIRST tmux command on
	// the kill teardown, so an unbounded stall here wedges the kill before
	// kill-session is even attempted.
	ctx, cancel := tmuxTimeoutContextWithin(budget)
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), paneRowFormat)
	if err != nil {
		if ctx.Err() != nil {
			return paneRow{}, fmt.Errorf("%w: display-message pane_pid after %s", ErrTmuxTimeout, min(budget, tmuxCommandTimeout))
		}
		// Diagnostic-only, deliberately: this is the half of
		// sessionGoneWithNoPaneObserved that gates a worktree deletion, and
		// display-message has no `no current target` gap to close. Measured on a
		// server holding no sessions, it answers exit 0 with EMPTY output — the
		// branch below — rather than the exit 1 that made has-session and
		// list-panes need tmuxProvedSessionAbsent (#3469).
		if missingTmuxSession(err, t.sanitizedName) {
			return paneRow{}, errPaneQueryFoundNoPane
		}
		return paneRow{}, fmt.Errorf("failed to query pane pid: %w", err)
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || paneRowHasNoPane(trimmed) {
		// tmux answered and named no pane. Measured: this is what a missing
		// session produces — exit 0, empty output — so it is the one pane-query
		// failure that is evidence of ABSENCE rather than of an unreadable answer.
		return paneRow{}, errPaneQueryFoundNoPane
	}
	row, err := parsePaneRow(trimmed)
	if err != nil {
		return paneRow{}, err
	}
	if row.pid <= 0 {
		return paneRow{}, fmt.Errorf("unexpected pane pid output %q", string(output))
	}
	return row, nil
}

// PaneRootProcess resolves the pane root's process identity — the (PID,
// StartID) pair proctree treats as one process instance — so a launch can
// record what it started and a later reattach can prove whether the same
// process still stands behind the reused session name (#5066). A held dead
// pane, an unanswerable query, or an uninspectable pid each return an error:
// "cannot prove" must never masquerade as an identity.
func (t *TmuxSession) PaneRootProcess() (proctree.Process, error) {
	row, err := t.panePID()
	if err != nil {
		return proctree.Process{}, err
	}
	if row.dead {
		return proctree.Process{}, fmt.Errorf("tmux session %s: pane root is a held dead pane", t.sanitizedName)
	}
	proc, err := proctree.Lookup(row.pid)
	if err != nil {
		return proctree.Process{}, fmt.Errorf("cannot inspect pane root pid %d: %w", row.pid, err)
	}
	return proc, nil
}

// PaneRootProcessMatches reports whether SOME pane of this session is rooted
// at the recorded (pid, start-time) identity — not only the active one, which
// is what panePID resolves: between launch and a daemon restart the user can
// split the window or focus a shell tab, moving the active-pane pointer while
// the launched agent keeps running in its own pane. The recorded pair names
// exactly one kernel process, so a session-wide scan cannot attribute it to a
// wrong pane (#5066 review). A reaped row carrying the pid means the kernel
// handed it elsewhere — no match. An unanswerable list or a vanished session
// returns an error: "cannot prove" must never masquerade as either verdict.
func (t *TmuxSession) PaneRootProcessMatches(pid int, startID uint64) (bool, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "list-panes", "-s", "-t", exactTarget(t.sanitizedName), "-F", paneRowFormat)
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("%w: list-panes for %s after %s", ErrTmuxTimeout, t.sanitizedName, tmuxCommandTimeout)
		}
		if missingTmuxSession(err, t.sanitizedName) {
			return false, errPaneQueryFoundNoPane
		}
		return false, fmt.Errorf("failed to list session panes: %w", err)
	}
	if strings.TrimSpace(string(output)) == "" {
		return false, errPaneQueryFoundNoPane
	}
	for _, field := range strings.Fields(string(output)) {
		row, err := parsePaneRow(field)
		if err != nil {
			return false, fmt.Errorf("%w (list-panes row %q)", err, field)
		}
		if row.pid != pid || row.reaped {
			continue
		}
		proc, err := proctree.Lookup(pid)
		if err != nil {
			return false, fmt.Errorf("cannot inspect pane root pid %d: %w", pid, err)
		}
		if proc.StartID == startID {
			return true, nil
		}
	}
	// No pane carries the recorded identity: the launched root was killed, its
	// pane closed, or the pid reused — not the recorded launch.
	return false, nil
}

// ProbePaneExit reports whether the command in the session's pane has finished,
// and, when it has, the exit status and death time tmux recorded. It is how a
// process tab's completion is observed rather than inferred from absence
// (#4479): remain-on-exit keeps the pane around precisely so this probe can
// answer.
//
// dead means the root process is gone, not merely that tmux marked the pane
// dead: a root that closed its terminal is still running, and reports as not
// dead. When tmux reports none of status, signal or death time, the pane pid
// decides.
//
// A root that has exited but that tmux has not collected yet is dead, and tmux
// will report how it ended as soon as it collects it: tmux marks the pane dead
// when its terminal closes, which can come first (#4682). The probe waits for
// that report, up to paneStatusWait, rather than answer a plain `exit 7` with
// an unknown status. A process tab's immediate-failure check and its durable
// exit record both read this answer.
//
// It also asks the server to collect the root (nudgeReap), because the root's
// own SIGCHLD may never reach tmux. tmux before 3.6 built with utempter (Ubuntu
// and Debian) loses it: closing the pane's terminal runs libutempter's helper
// with SIGCHLD set to SIG_DFL, and a root that exits in that window stays a
// zombie, with no status, until the server's next SIGCHLD, which on a quiet
// server never comes. Measured on Ubuntu 24.04's tmux 3.4: 25 of 400 held
// `exit 7` panes, and 0 of 400 with utempter stubbed out. Upstream fixed it in
// fa5f3cef3d (tmux/tmux#4559, released in 3.6); the nudge can go once 3.6 is
// the oldest tmux af supports.
//
// The known contract matches ProbeSession: known=false means tmux could not
// answer (timeout, socket policy), never that the pane is alive. status is
// meaningful only when statusKnown: a signal death, a tmux too old to report
// the field, or a root tmux did not collect within paneStatusWait leaves it
// unknown.
func (t *TmuxSession) ProbePaneExit() (dead bool, status int, statusKnown bool, at time.Time, known bool) {
	exit, uncollected := t.readPaneExit(tmuxCommandTimeout)
	// Every re-read is bounded by what is left of the wait, so a tmux that
	// stalls mid-wait cannot hold the caller for a whole command timeout.
	deadline := time.Now().Add(paneStatusWait)
	pause := 5 * time.Millisecond
	nudges := 0
	for uncollected {
		if nudges < maxReapNudges {
			nudges++
			t.nudgeReap(time.Until(deadline))
		}
		time.Sleep(pause)
		pause = min(2*pause, 50*time.Millisecond)
		budget := time.Until(deadline)
		if budget <= 0 {
			break
		}
		next, stillUncollected := t.readPaneExit(budget)
		if !next.dead && exit.known {
			// The root's exit is already established; a re-read that cannot
			// confirm it (tmux failed, the session went away) must not undo
			// it. A provisional pending reading — known=false — is no such
			// established exit: a resolving re-read that reports the stamped
			// pid still running answers live, and replaces it.
			break
		}
		exit, uncollected = next, stillUncollected
	}
	if nudges > 0 {
		version := exit.version
		if version == "" {
			version = "an unknown version"
		}
		log.InfoLog.Printf("tmux session %s: pane root exited but its tmux server (tmux %s) had not collected it; nudged the server %d time(s), status known after: %t",
			t.sanitizedName, version, nudges, exit.statusKnown)
	}
	return exit.dead, exit.status, exit.statusKnown, exit.at, exit.known
}

// paneStatusWait bounds how long ProbePaneExit waits for tmux to collect a pane
// root that has already exited. tmux collects it from its event loop, so this is
// normally milliseconds; the bound only matters when tmux is stalled. var, not
// const, so tests can lower it.
var paneStatusWait = 2 * time.Second

// paneDeathsWait bounds how long panePID re-reads a row whose stamp is pending
// — dead on every freshness input but the deaths counter. The pane-died hook
// runs on tmux's command queue and pane_dead is observable first (#5335), so a
// gap of 0 resolves to 1 within milliseconds on a healthy server; the bound
// only matters when the hook is gone entirely — including the Ubuntu-class
// build that loses the root's SIGCHLD and never collects the death the hook
// fires on (#4682), which panePID nudges the server out of first. var, not
// const, so tests can lower it.
var paneDeathsWait = 2 * time.Second

// maxReapNudges caps how many times one probe nudges the server. One is enough
// unless the nudge's own SIGCHLD lands in another pane's utempter window.
const maxReapNudges = 3

// nudgeReap makes the pane's tmux server run a trivial job. The job's exit
// delivers a SIGCHLD, and tmux's handler collects every exited child with
// waitpid(WAIT_ANY), so it collects the stranded pane root too and records its
// status. On a tmux that lost nothing, the job just runs and exits. It goes
// through the same executor as the probe's reads, so it reaches the same
// server. A failure is ignored, and the probe falls back to waiting.
func (t *TmuxSession) nudgeReap(budget time.Duration) {
	ctx, cancel := tmuxTimeoutContextWithin(budget)
	defer cancel()
	_ = t.runTmuxBounded(ctx, "run-shell", "-b", "true")
}

// paneExit is one reading of ProbePaneExit's answer.
type paneExit struct {
	dead, statusKnown, known bool
	status                   int
	at                       time.Time
	// version is the tmux SERVER's own version, for the nudge's log line: the
	// installed client can be newer than a server started before an upgrade.
	version string
}

// readPaneExit reads the pane once, within budget. uncollected reports a pane
// whose root has exited but that tmux has not collected, so it has reported
// nothing about how the root ended yet.
func (t *TmuxSession) readPaneExit(budget time.Duration) (exit paneExit, uncollected bool) {
	ctx, cancel := tmuxTimeoutContextWithin(budget)
	defer cancel()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), paneExitFormat)
	if err != nil {
		return paneExit{}, false
	}
	// Split, not Fields: an empty field is information (#4506 review).
	fields := strings.Split(strings.TrimSpace(string(out)), paneFieldSeparator)
	field := func(i int) string {
		if i < len(fields) {
			return strings.TrimSpace(fields[i])
		}
		return ""
	}
	if field(0) != "1" {
		return paneExit{known: true}, false
	}
	exit = paneExit{dead: true, known: true, version: field(5)}
	if code, perr := strconv.Atoi(field(1)); perr == nil {
		exit.status, exit.statusKnown = code, true
	}
	if unix, perr := strconv.ParseInt(field(2), 10, 64); perr == nil && unix > 0 {
		exit.at = time.Unix(unix, 0)
	}
	if field(1) == "" && field(2) == "" && field(3) == "" {
		// tmux has not reaped the root, or is too old to say it did. On tmux
		// ≥3.8 pane_pid is already empty for a dead pane (#5319); the stamped
		// fallback names the same root while it was alive — but only while the
		// stamp still names the pane's current incarnation (paneStampPID).
		// The fallback engages only on an EMPTY pane_pid: a populated one is
		// tmux's maintained value and wins, and a malformed one is itself the
		// failure.
		pid, perr := strconv.Atoi(field(4))
		if perr != nil && field(4) == "" {
			switch stamped, verdict := paneStampPID(field(6), field(7), field(8), field(9)); verdict {
			case stampFresh:
				pid, perr = stamped, nil
			case stampPending:
				// pane_dead is visible before the queued pane-died hook
				// increments the counter (#5335): the stamp cannot name the
				// root yet, so keep probing inside the same uncollected wait
				// the root's own reap races — dead:true is what keeps the
				// wait alive. But pane_dead only says tmux MARKED the pane
				// dead; the pid checks below are what prove the root is gone
				// ("closed its terminal but still running" reads dead only
				// when pidGone/exitedUncollected agree). Leaving known unset
				// makes a hook that never lands answer unknown rather than a
				// conclusively dead root — the difference between not stamping
				// a TabExit and recording one a live root never earned.
				return paneExit{dead: true, version: exit.version}, true
			}
		}
		if perr != nil || pid <= 0 {
			return paneExit{}, false
		}
		if pidGone(pid) {
			return exit, false
		}
		if !exitedUncollected(pid) {
			return paneExit{known: true}, false
		}
		return exit, true
	}
	return exit, false
}

// exitedUncollected reports whether pid is a process that has exited and waits
// for its parent to collect it. A pane root in that state has finished, even
// though tmux has not reported how yet.
func exitedUncollected(pid int) bool {
	_, err := proctree.Lookup(pid)
	return errors.Is(err, proctree.ErrProcessExited)
}

// paneExitFormat keeps the probe's fields positional: tmux leaves
// pane_dead_status EMPTY for a command killed by a signal while still filling
// pane_dead_time, so a whitespace-separated answer collapses and reads the death
// time as the exit status (#4506 review). The last fields are the server's
// version, for the nudge's log line, and the pane's @af_pane_pid stamp plus
// its incarnation proof (deaths counter, pty name, command pin — see
// paneStampPID): tmux ≥3.8 empties pane_pid on death (#5319), the
// uncollected-root check below needs a pid to test either way, and the stamp
// is trusted only while it still names the pane's current incarnation.
const paneExitFormat = "#{pane_dead}" + paneFieldSeparator + "#{pane_dead_status}" + paneFieldSeparator +
	"#{pane_dead_time}" + paneFieldSeparator + "#{pane_dead_signal}" + paneFieldSeparator + "#{pane_pid}" +
	paneFieldSeparator + "#{version}" + paneFieldSeparator + "#{" + PanePidMarker + "}" +
	paneFieldSeparator + "#{" + paneDeathsMarker + "}" + paneFieldSeparator + "#{pane_tty}" +
	paneFieldSeparator + paneCommandMatchField

// LaunchPending reports whether the pane's root is still af's launch shim, the
// environment filter that execs the pane command, so the command itself has
// not started yet. It covers both stages: the default shell running the shim's
// command line, and the shim. Any doubt answers false.
func (t *TmuxSession) LaunchPending() bool {
	row, err := t.panePID()
	if err != nil || row.dead {
		return false
	}
	return argvIsLaunchShim(proctree.Argv(row.pid))
}

func argvIsLaunchShim(argv []string) bool {
	isMarker := func(word string) bool {
		switch word {
		case sessionenv.ExecMarker, sessionenv.AccountExecMarker, sessionenv.AccountEnvironmentExecMarker:
			return true
		}
		return false
	}
	switch {
	case len(argv) >= 2 && isMarker(argv[1]):
		return true
	case len(argv) >= 3 && argv[1] == "-c":
		for _, word := range strings.Fields(argv[2]) {
			if isMarker(word) {
				return true
			}
		}
	}
	return false
}

// FinishedAndQuiet reports whether the session holds only a finished command
// with nothing of it still running: the pane's root has exited, and its kernel
// session has no surviving member. Such a pane carries no process to stop, so a
// caller can keep its output instead of killing it. Any doubt answers false.
func (t *TmuxSession) FinishedAndQuiet() bool {
	dead, _, _, _, known := t.ProbePaneExit()
	if !known || !dead {
		return false
	}
	procs, err := CaptureSessionProcessTrees(t.cmdExec, t.sanitizedName)
	return err == nil && len(procs) == 0
}

// FinishedPaneOutput returns the last lines a finished command printed, as
// plain text, for an error message. tmux's own "Pane is dead" banner is left
// out. It is best-effort: any failure returns "".
func (t *TmuxSession) FinishedPaneOutput(maxLines int) string {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := t.outputTmuxBounded(ctx, "capture-pane", "-p", "-J", "-S", "-200", "-t", exactTarget(t.sanitizedName))
	if err != nil {
		return ""
	}
	var kept []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Pane is dead") {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) > maxLines {
		kept = kept[len(kept)-maxLines:]
	}
	return strings.Join(kept, "\n")
}
