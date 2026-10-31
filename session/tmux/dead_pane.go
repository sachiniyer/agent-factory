package tmux

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
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
// process whose SID equals it was started inside that pane.

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
	case panePIDRaw == "" && row.dead && verdict == stampStale:
		return paneRow{}, fmt.Errorf("dead pane carries a %s stamp for root %d from an earlier incarnation — the pane was respawned or its stamp write failed, so its process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion", PanePidMarker, stampPID)
	case panePIDRaw == "" && row.dead && stampPID > 0:
		return paneRow{}, fmt.Errorf("dead pane carries a %s stamp for root %d that predates the incarnation proof, so its process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion", PanePidMarker, stampPID)
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
	// stampLegacy means a readable stamp carrying no incarnation bundle at
	// all — a bare pid written before the proof fields existed. It cannot
	// name the current incarnation, but unlike a stale bundle nothing in it
	// positively contradicts the current run either. Evidence that does not
	// come from the stamp — tmux's own collected status — may stand beside
	// it; the stamped pid itself stays unusable, exactly like an unstamped
	// pane's.
	stampLegacy
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
// A stamp written before the incarnation proof existed — a bare pid with no
// second colon — is stampLegacy: it fails every proof it was never given
// rather than contradicting one, so readers choose how much that absence
// costs. A bundle that did set out to carry the proof and is malformed still
// fails closed as stale. A deaths field left empty at stamp time is the
// counter's unset state, i.e. zero.
func paneStampPID(stamp, deathsNow, ttyNow, cmdMatch string) (int, paneStampVerdict) {
	pidRaw, rest, hasRest := strings.Cut(stamp, ":")
	pid, err := strconv.Atoi(pidRaw)
	if err != nil || pid <= 0 {
		return 0, stampUnusable
	}
	if !hasRest {
		// A bare pid is a stamp written before the incarnation proof
		// existed: readable, but unprovable — it cannot name the current
		// incarnation, only fail the proofs it was never given. That is
		// weaker than a stale bundle, which DID carry the proof and
		// contradicted it.
		return pid, stampLegacy
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
func deadPaneProcesses(snap map[int]proctree.Process, sanitizedName string, row paneRow) (procs []proctree.Process, asLive bool, err error) {
	_, present := snap[row.pid]
	switch {
	case present && row.reaped:
		// Reaped, and the pid is live again: the kernel reused it. A process of
		// this pane would still hold the pid as its session id, which would have
		// kept it from being reused, so nothing of the pane survives.
		log.InfoLog.Printf("tmux session %s: dead pane pid %d now names another process; not capturing it",
			sanitizedName, row.pid)
		return nil, false, nil
	case present:
		// Not reaped, so tmux still holds the pid as its child: normally the
		// root, still running after its terminal closed. tmux older than 3.3
		// never reports a signal death, though, so there the pid may have been
		// reaped and reused. A launch marker naming a different session proves
		// that. Without one, the live-root checks decide.
		if marker, status := proctree.LookupEnv(row.pid, EnvMarkerSession); status == proctree.EnvFound && marker != sanitizedName {
			log.InfoLog.Printf("tmux session %s: dead pane pid %d belongs to session %s; not capturing it",
				sanitizedName, row.pid, marker)
			return nil, false, nil
		}
		return nil, true, nil
	}
	members := proctree.SessionMembers(snap, row.pid)
	if len(members) == 0 || pidGone(row.pid) {
		return members, false, nil
	}
	// The snapshot did not list the pid, but something holds it now.
	_, lookupErr := proctree.Lookup(row.pid)
	switch {
	case errors.Is(lookupErr, proctree.ErrProcessExited) && !row.reaped:
		// The snapshot leaves out zombies. tmux has not collected the root yet,
		// so the zombie is the root, and it still reserves the pid.
		return members, false, nil
	case lookupErr == nil || errors.Is(lookupErr, proctree.ErrProcessExited):
		// The pid was reused after the snapshot, so the pane's own session
		// members were already gone by then. The captured set can only hold
		// exited processes or the new owner's session, and neither is ours.
		log.InfoLog.Printf("tmux session %s: dead pane pid %d was reused during capture; not capturing its session",
			sanitizedName, row.pid)
		return nil, false, nil
	case pidGone(row.pid):
		return members, false, nil
	default:
		return nil, false, fmt.Errorf("cannot tell whether dead pane pid %d was reused: %w", row.pid, lookupErr)
	}
}

// pidGone reports whether no process has this pid. EPERM means one does.
func pidGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// paneRootGone is pidGone as readPaneExit asks it, a variable so a test can decide
// what happens between the probe's format read and this check.
var paneRootGone = pidGone

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
// The reading comes from the session's active pane, but under remain-on-exit a
// user who attaches can split the window and end up focused on a pane af did
// not launch. Only a pane whose recorded start command carries af's launch
// marker — and whose stamp, when it exists, still names the current
// incarnation — is the recorded command's pane; a foreign pane's death is not
// the command's exit and reports not-dead, so a split pane the user killed can
// never be stamped as how the command ended (readPaneExit).
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
	// A dead pane whose root is still running is either a root that closed its
	// terminal and carries on, or one that is exiting right now: tmux marks the pane
	// dead when the pty hits EOF, and the kernel closes a dying process's
	// descriptors before it becomes a zombie. One read cannot tell them apart, so
	// give the second kind a short grace to become collectable (#6439) before
	// answering that the root is alive.
	if exit.rootRunning {
		graceEnd := time.Now().Add(paneRootGrace)
		for pause := 5 * time.Millisecond; exit.rootRunning && time.Now().Before(graceEnd); pause = min(2*pause, 25*time.Millisecond) {
			time.Sleep(pause)
			budget := time.Until(graceEnd)
			if budget <= 0 {
				break
			}
			next, nextUncollected := t.readPaneExit(budget)
			if !next.known {
				// A re-read that cannot answer, including one cut off by the end of
				// the grace, must not undo the reading that did.
				break
			}
			exit, uncollected = next, nextUncollected
		}
	}
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
	// The status fields and the root's liveness are two reads, not one: tmux can
	// collect the root between the format read (status still empty) and the
	// pidGone check, which then answers "gone" with a status the next read has. A
	// pane that took that path settles for a short bounded time rather than
	// reporting a finished command whose status is unknown (#6439).
	if exit.reapedNoStatus {
		settleEnd := time.Now().Add(paneRootGrace)
		for pause := 5 * time.Millisecond; exit.reapedNoStatus && time.Now().Before(settleEnd); pause = min(2*pause, 25*time.Millisecond) {
			time.Sleep(pause)
			budget := time.Until(settleEnd)
			if budget <= 0 {
				break
			}
			next, _ := t.readPaneExit(budget)
			if !next.known || !next.dead {
				break
			}
			exit = next
		}
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

// paneRootGrace bounds how long ProbePaneExit waits for the running root of a dead
// pane to finish exiting before it concludes the root is alive. It is paid only by
// a pane that is held dead with its root running, which is rare. var, not const,
// so tests can widen it.
var paneRootGrace = 200 * time.Millisecond

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
	// rootRunning marks a dead pane whose root is neither collected nor a zombie.
	rootRunning bool
	// reapedNoStatus marks a dead pane whose root is gone but whose status the read
	// did not carry: tmux may have recorded it after the read.
	reapedNoStatus bool
	status         int
	at             time.Time
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
	// The reading comes from the session's ACTIVE pane — what `=name:`
	// resolves to (see exactTarget) — and under remain-on-exit a user who
	// attaches can split the window and end up focused on a pane af did not
	// launch. That pane inherits remain-on-exit, exits, and is held dead as
	// the active pane, so tmux reports its death — collected status or bare
	// pid — on the very target the recorded command never ran on. What proves
	// the recorded command ran there is pane_start_command (the last field):
	// af's launch wrapper leaves a sessionenv exec marker AND the recorded
	// command as its last argument, immutable across respawn and older than
	// the stamp. The marker alone does not suffice — an account-scoped
	// session's default-command wraps an interactive account shell through
	// the same sessionenv marker, so a user's split pane can carry a genuine
	// af marker without ever having run the recorded command — and the stamp
	// alone cannot prove launch either, because restoreProcessTab's
	// remain-on-exit heal (ApplyRemainOnExit) stamps whatever pane is ACTIVE,
	// which under a user split is the foreign pane, and once the hook counts
	// that death the healed stamp reads stampFresh. The first-wins guard on
	// Tab.Exit makes a wrongly attributed exit permanent — the same
	// active-pane hazard the session-wide scan PaneRootProcessMatches exists
	// to reject (#5066 review: "the user can split the window or focus a
	// shell tab, moving the active-pane pointer") — so a pane whose recorded
	// launch is not this session's command never supplies a Tab.Exit at all.
	if !t.paneLaunchIsRecorded(fields[10:]) {
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
		if paneRootGone(pid) {
			exit.reapedNoStatus = true
			return exit, false
		}
		if !exitedUncollected(pid) {
			return paneExit{known: true, rootRunning: true}, false
		}
		return exit, true
	}
	// pane_start_command already proved this pane ran the recorded command,
	// so tmux's collected status is the recorded command's exit — it is this
	// pane's own last death — UNLESS the stamp positively names an earlier
	// incarnation: a respawn that moved past the stamp's pins died under a
	// command the row cannot attribute. A fresh stamp, a pending counter
	// (#5335), no stamp at all, or a bare-pid stamp written before the
	// incarnation fields existed (stampLegacy — unprovable, but contradicting
	// nothing) all keep the exit tmux already collected, and a pane that died
	// before its heal must not wait out a deaths hook that can never land.
	if _, verdict := paneStampPID(field(6), field(7), field(8), field(9)); verdict == stampStale {
		return paneExit{known: true}, false
	}
	return exit, false
}

// paneLaunchIsRecorded reports whether the pane's recorded pane_start_command
// is THIS session's recorded launch — the pane the recorded process belongs
// to, not merely a pane running under an af environment wrapper. af wraps
// every pane it launches in a sessionenv exec marker and leaves the recorded
// command as the wrapper's last argument, so the binding is marker AND the
// recorded program's quoted form at the tail. The marker alone is not launch
// provenance: an account-scoped session's default-command wraps an
// interactive account shell through the same marker (prepareLaunchEnvironment),
// so a split pane created from that default carries it without ever having
// run the recorded command. startFields is the raw tail of the probe's reply
// split on the field separator, joined so a command containing the separator
// still reads whole.
// unquotePaneStartCommand undoes the quoting tmux applies when it records
// pane_start_command: a command containing spaces or quote characters is
// stored as "…" with \, ", $ and ` escaped. The launch marker and the
// program tail must be matched on the command as af wrote it, so the wrap
// comes off first; a verbatim recording passes through unchanged.
func unquotePaneStartCommand(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) - 2)
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '\\' && i+1 < len(s)-1 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (t *TmuxSession) paneLaunchIsRecorded(startFields []string) bool {
	start := unquotePaneStartCommand(strings.Join(startFields, paneFieldSeparator))
	program := t.Program()
	if program == "" {
		return false
	}
	for _, marker := range []string{
		sessionenv.ExecMarker,
		sessionenv.AccountExecMarker,
		sessionenv.AccountEnvironmentExecMarker,
		sessionenv.AgentServerExecMarker,
		sessionenv.AgentServerAccountExecMarker,
	} {
		if strings.Contains(start, marker) {
			return strings.HasSuffix(start, " "+shellquote.Quote(program))
		}
	}
	return false
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
	paneFieldSeparator + paneCommandMatchField + paneFieldSeparator + "#{pane_start_command}"

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

// PaneDead reports tmux's pane_dead flag: the pane's terminal is gone, which
// is the condition pipe-pane refuses — whether or not the pane's root still
// runs (#5330). ProbePaneExit's stricter "the root is gone" answer would miss
// the closed-terminal-but-alive shape, so this reads the flag directly and
// cheaply: one bounded display-message, no reap wait or nudge — a stream
// subscribe only needs to know whether the pane can still produce. Any
// unreadable answer is false: the ordinary capture path then gets its usual
// error instead of a guessed dead verdict.
func (t *TmuxSession) PaneDead() bool {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), "#{pane_dead}")
	return err == nil && strings.TrimSpace(string(out)) == "1"
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
