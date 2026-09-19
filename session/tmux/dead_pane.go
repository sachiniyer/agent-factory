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
// nothing, so a whitespace split would shift later fields left. Every value is
// numeric, so the separator cannot occur inside one.
const paneFieldSeparator = "|"

// paneRowFormat is what the teardown asks list-panes and display-message for.
// The third field concatenates every value tmux reports only after reaping the
// root, so it is non-empty exactly when the root is known to be gone.
//
// A reply without the separator is a bare pane_pid, which is what a tmux that
// cannot expand the later fields, or a test executor, returns. It reads as a
// live pane, the behaviour from before #4479.
const paneRowFormat = "#{pane_pid}" + paneFieldSeparator + "#{pane_dead}" + paneFieldSeparator +
	"#{pane_dead_status}#{pane_dead_signal}#{pane_dead_time}"

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
	pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return paneRow{}, fmt.Errorf("invalid pane pid %q", field)
	}
	row := paneRow{pid: pid}
	if len(parts) > 1 {
		row.dead = strings.TrimSpace(parts[1]) == "1"
	}
	if len(parts) > 2 {
		row.reaped = row.dead && strings.TrimSpace(parts[2]) != ""
	}
	return row, nil
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

// panePID reads the session's pane pid and whether that pane is held dead. It
// must run before kill-session, because afterwards there is nothing to ask.
func (t *TmuxSession) panePID() (paneRow, error) {
	// exactTarget forces an exact session match, mirroring ExistsOrUnknown.
	// (The bare `=name` form returns an empty pane_pid for display-message —
	// the trailing `:` in exactTarget is what makes the pid resolve. See #1006.)
	//
	// Bounded by tmuxCommandTimeout (#1917): this is the FIRST tmux command on
	// the kill teardown, so an unbounded stall here wedges the kill before
	// kill-session is even attempted.
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), paneRowFormat)
	if err != nil {
		if ctx.Err() != nil {
			return paneRow{}, fmt.Errorf("%w: display-message pane_pid after %s", ErrTmuxTimeout, tmuxCommandTimeout)
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
	if trimmed == "" || strings.Trim(trimmed, paneFieldSeparator) == "" {
		// tmux answered and named no pane. Measured: this is what a missing
		// session produces — exit 0, empty output — so it is the one pane-query
		// failure that is evidence of ABSENCE rather than of an unreadable answer.
		// With this multi-field format that answer is the bare separators: `||`,
		// exit 0 (measured, tmux 3.4).
		return paneRow{}, errPaneQueryFoundNoPane
	}
	row, err := parsePaneRow(trimmed)
	if err != nil || row.pid <= 0 {
		return paneRow{}, fmt.Errorf("unexpected pane pid output %q", string(output))
	}
	return row, nil
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
// The known contract matches ProbeSession: known=false means tmux could not
// answer (timeout, socket policy), never that the pane is alive. status is
// meaningful only when statusKnown: a signal death, or a tmux too old to report
// the field, leaves it unknown.
func (t *TmuxSession) ProbePaneExit() (dead bool, status int, statusKnown bool, at time.Time, known bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), paneExitFormat)
	if err != nil {
		return false, 0, false, time.Time{}, false
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
		return false, 0, false, time.Time{}, true
	}
	if code, perr := strconv.Atoi(field(1)); perr == nil {
		status, statusKnown = code, true
	}
	if unix, perr := strconv.ParseInt(field(2), 10, 64); perr == nil && unix > 0 {
		at = time.Unix(unix, 0)
	}
	if field(1) == "" && field(2) == "" && field(3) == "" {
		// tmux has not reaped the root, or is too old to say it did.
		pid, perr := strconv.Atoi(field(4))
		if perr != nil || pid <= 0 {
			return false, 0, false, time.Time{}, false
		}
		if !pidGone(pid) && !exitedUncollected(pid) {
			return false, 0, false, time.Time{}, true
		}
	}
	return true, status, statusKnown, at, true
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
// time as the exit status (#4506 review).
const paneExitFormat = "#{pane_dead}" + paneFieldSeparator + "#{pane_dead_status}" + paneFieldSeparator +
	"#{pane_dead_time}" + paneFieldSeparator + "#{pane_dead_signal}" + paneFieldSeparator + "#{pane_pid}"

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
