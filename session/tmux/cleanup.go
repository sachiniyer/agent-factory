package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
)

// ExistsOrUnknown reports session existence as a deliberately LOSSY bool: it
// returns true for "the session exists OR tmux could not be reached (a wedged /
// timed-out has-session)", and false ONLY for "definitively absent" — tmux
// answered and said no such session. A wedged has-session is laundered into true
// here (via sessionExists), the conservative lie a bool is forced to tell.
//
// The name states the lie so it is visible at every call site. Contract:
//
//   - A caller MAY act on !ExistsOrUnknown() ("definitively gone"): classify a
//     failed command as ErrSessionGone, or skip the idempotent teardown of an
//     already-absent session. A wedged server reads as "exists", so these callers
//     never falsely tear down or abandon a session that is merely slow — the safe
//     direction, and the reason this form is kept.
//   - A caller MUST NOT read a bare true as proof of life: "true" folds in "I
//     could not tell". Any site that treats existence as EVIDENCE or as a POSITIVE
//     gate must call ProbeSession() and handle !known explicitly (#1917/#1962).
func (t *TmuxSession) ExistsOrUnknown() bool {
	return sessionExists(t.cmdExec, t.sanitizedName)
}

// sessionExists reports whether a tmux session with the exact name `name`
// currently exists. Shared by ExistsOrUnknown and the receiver-less
// CleanupSessions path so both probe identically.
//
// Bounded by tmuxCommandTimeout (#1917): has-session against a wedged server
// parks forever, and this probe is the fallback on nearly every tmux error path
// in the package — including Close's, on the daemon's undeletable-session
// teardown.
//
// A tripped deadline reports TRUE (exists). The bool cannot express "unknown",
// so the answer has to be the one that is safe to be wrong about, and every
// caller that acts destructively acts on FALSE: Close reaps the session's
// process trees, and io.go/clientless.go raise ErrSessionGone, which the daemon
// reads as a confirmed death. A false "gone" against a server that is merely
// wedged would SIGKILL a live agent's process tree and tear down a session that
// is still running — the exact mistake tmuxTimeoutContext exists to prevent. A
// false "exists" only costs a best-effort skip, so it is the conservative lie.
//
// Callers that must not paper over the difference do not use this probe at all:
// they check ctx.Err() on their own bounded command and skip it entirely (see
// tmuxTimeoutContext, and Close's kill-session timeout branch).
//
// A non-timeout failure — the usual `has-session` exit 1 for "no such session",
// or any other tmux error — still reports false, preserving the pre-#1917
// conflation callers already relied on.
func sessionExists(cmdExec cmd.Executor, name string) bool {
	exists, known := probeSession(cmdExec, name)
	if !known {
		log.WarningLog.Printf("tmux has-session for %s timed out after %s; the server is wedged, so "+
			"reporting the session as still present rather than risk a false teardown", name, tmuxCommandTimeout)
		return true
	}
	return exists
}

// ProbeSession reports whether this session exists AND whether tmux actually
// ANSWERED — the tri-state a bool cannot express (#1917 round 8).
//
// ExistsOrUnknown has to pick yes or no, so a timed-out probe becomes "yes": the
// conservative lie, safe for the read-only callers that only ever act on "no". But
// it launders UNKNOWN into AFFIRMATIVE at the bottom of the stack, and every caller
// above is then downstream of a lie it cannot detect — which is how a wedged tmux
// server came to be reported as a live agent, and how a liveness counter built on
// affirmative evidence got fooled anyway. Callers that treat "alive" as EVIDENCE
// must take this form; callers that merely need a bool keep the lie, knowingly.
func (t *TmuxSession) ProbeSession() (exists bool, known bool) {
	return probeSession(t.cmdExec, t.sanitizedName)
}

// probeSession is sessionExists WITHOUT the lossy collapse: it reports whether
// the session exists AND whether tmux actually answered.
//
// The two-value form exists because the collapse above, while safe for the
// probe's many read-only callers, silently destroyed information for the one
// caller that tears sessions down: Close asked "does it still exist?", got back
// a `true` synthesized from a TIMEOUT, and reported an ordinary kill failure —
// so its caller deleted the workspace with the session's fate unknown (#1917).
// A caller that acts on the answer takes this form and handles !known; a caller
// that only reads takes the bool and gets the conservative lie.
func probeSession(cmdExec cmd.Executor, name string) (exists bool, known bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	// Using "-t name" does a prefix match, which is wrong. `-t=` does an exact match.
	err := runTmuxBoundedWith(ctx, cmdExec, "has-session", fmt.Sprintf("-t=%s", name))
	if err == nil {
		return true, true
	}
	if ctx.Err() != nil {
		return false, false
	}
	// tmux answered: the usual `has-session` exit 1 for "no such session", or any
	// other error, which this probe has always conflated with absence.
	return false, true
}

// probeSessionStrict is the non-lossy existence probe for CleanupSessions'
// ownership gate. Unlike probeSession, it does not treat every non-timeout
// execution failure as absence: only tmux's ordinary exit 1 paired with its
// exact "can't find session" diagnostic is a determinate answer. Any other
// failure remains unknown and cannot authorize reset to continue to worktree
// deletion.
func probeSessionStrict(cmdExec cmd.Executor, name string) (exists bool, known bool, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	_, err = outputTmuxBoundedWith(ctx, cmdExec, "has-session", fmt.Sprintf("-t=%s", name))
	if err == nil {
		return true, true, nil
	}
	if ctx.Err() != nil {
		return false, false, fmt.Errorf("%w: has-session %s after %s", ErrTmuxTimeout, name, tmuxCommandTimeout)
	}
	if missingTmuxSession(err, name) {
		return false, true, nil
	}
	return false, false, fmt.Errorf("has-session for %s did not return a usable answer: %w", name, err)
}

// missingTmuxSession recognizes tmux's explicit exact-target absence answer.
// Exit status 1 alone is ambiguous: wrapper, policy, and unclassified connection
// failures can return the same status while the named session remains unknown.
// An explicit no-server diagnostic is also definitive: no session can remain.
func missingTmuxSession(err error, name string) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	diagnostic := strings.TrimSpace(string(exitErr.Stderr))
	if diagnostic == "can't find session: "+name {
		return true
	}
	serverSocket, noServer := strings.CutPrefix(diagnostic, "no server running on ")
	return noServer && strings.TrimSpace(serverSocket) != ""
}

// exactTarget builds an exact-match `-t` target spec for the named session.
//
// tmux resolves a bare `-t name` by exact match first and then PREFIX match, so
// once an agent session dies a bare target silently resolves to a surviving
// sibling — e.g. the shell tab `<name>__shell` of which `<name>` is an exact
// prefix. Capturing or sending to that sibling masks the dead agent and skips
// the liveness check (#1006).
//
// The leading `=` forces an exact session match. The trailing `:` is required
// for the pane-target commands (capture-pane, send-keys, set-option): without
// it tmux parses `=name` as a bare pane spec and reports "can't find pane:
// =name" even when the session exists. Appending the (empty) window component
// makes tmux parse `=name` as the session and resolve to its active pane. The
// session-target commands (kill-session, attach-session) accept the same form,
// so every action command shares one exact-match spelling.
func exactTarget(name string) string {
	return fmt.Sprintf("=%s:", name)
}

// CleanupSessions kills every af_-prefixed tmux session owned by THIS
// agent-factory home, ownership proven by the AF_HOME session-environment
// marker stamped at creation (#1120). Sessions carrying another home's marker
// (a second install, a test's sandbox home) and sessions with no marker at
// all (pre-marker builds, tmux <3.2) are skipped and logged: killing a
// session this home cannot prove it owns is worse than leaving it, and a
// test sweep that escapes onto the developer's real server must be a no-op
// (#1122). `af doctor` lists unowned af_ sessions with a manual kill command.
func CleanupSessions(cmdExec cmd.Executor) error {
	// First try to list sessions. Bounded by tmuxCommandTimeout (#2099): this is
	// the first tmux command of the sweep, and `af reset` runs the sweep
	// synchronously in a short-lived CLI process, so an unbounded stall here is a
	// user-visible hang with no way out but ^C.
	listCtx, listCancel := tmuxTimeoutContext()
	output, err := outputTmuxBoundedWith(listCtx, cmdExec, "ls")
	listTimedOut := listCtx.Err() != nil
	listCancel()

	// If there's an error and it's because no server is running, that's fine
	// Exit code 1 typically means no sessions exist
	if err != nil {
		if listTimedOut {
			// A wedged server has told us NOTHING about what is running, so the
			// sweep must abort rather than proceed on an empty list — an empty
			// list here would silently read as "nothing to clean up" and report
			// success for a reset that swept nothing.
			return fmt.Errorf("%w: tmux ls after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil // No sessions to clean up
		}
		return fmt.Errorf("failed to list tmux sessions: %v", err)
	}

	// Anchor to start-of-line so `af_` embedded in a non-agent session name
	// (e.g. `my_af_project:`) is never matched and killed (#613).
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^%s[^:]*:`, regexp.QuoteMeta(TmuxPrefix)))
	prefixed := re.FindAllString(string(output), -1)
	for i, match := range prefixed {
		prefixed[i] = match[:strings.Index(match, ":")]
	}
	// Capture every listed session's verified pane process tree before marker
	// lookup. If the session vanishes during that lookup, tmux can no longer
	// recover its pane ancestry; this is the last authoritative opportunity to
	// bind any detached helper to the session. Capture errors are retained for
	// the vanished-session branch; a live owned session is captured again just
	// before kill, and a foreign session's unreadable panes never block this
	// home's sweep.
	preMarkerProcesses := make(map[string][]proctree.Process, len(prefixed))
	preMarkerCaptureErrs := make(map[string]error, len(prefixed))
	for _, match := range prefixed {
		preMarkerProcesses[match], preMarkerCaptureErrs[match] = captureSessionProcessTrees(cmdExec, match)
		if errors.Is(preMarkerCaptureErrs[match], ErrTmuxTimeout) {
			// The server is already known to be wedged. Do not launch the
			// ownership probe against it and pay another full timeout for an
			// answer that still could not authorize deletion.
			return fmt.Errorf("cannot capture tmux session %s processes before ownership lookup; refusing to continue cleanup: %w",
				match, preMarkerCaptureErrs[match])
		}
	}

	// Home-scope the sweep (#1122): the af_ prefix alone does not prove this
	// home owns the session — another install or an escaped test process can
	// see the same server. Only the AF_HOME marker match does.
	ownHome, err := afHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve this agent-factory home; refusing to sweep tmux sessions: %w", err)
	}
	matches := make([]string, 0, len(prefixed))
	for _, match := range prefixed {
		home, ok, markerErr := sessionHomeMarker(cmdExec, match)
		if markerErr != nil {
			if errors.Is(markerErr, ErrTmuxTimeout) {
				// The same server is already known to be wedged. A fallback
				// probe would wait on it for another full timeout and still
				// could not turn the ownership result into affirmative state.
				return fmt.Errorf("cannot determine tmux session %s ownership; refusing to continue cleanup: %w", match, markerErr)
			}
			// The session may have disappeared after `tmux ls`. Re-probe the
			// exact name after every non-timeout marker error: only an authoritative
			// has-session absence turns this race into a successful cleanup. Session
			// absence is not process absence: a detached AF_SESSION-marked helper may
			// have survived after tmux lost the pane ancestry used by the normal
			// reaper, so the absent branch performs an ownership-scoped process sweep.
			exists, known, probeErr := probeSessionStrict(cmdExec, match)
			if known && !exists {
				if reapErr := reapVanishedSessionProcesses(match, ownHome, preMarkerProcesses[match], preMarkerCaptureErrs[match]); reapErr != nil {
					return fmt.Errorf("tmux session %s vanished during ownership lookup, but its process cleanup is incomplete: %w",
						match, reapErr)
				}
				log.InfoLog.Printf("tmux session %s vanished during ownership lookup; marked orphan process sweep completed", match)
				continue
			}
			if !known {
				return fmt.Errorf("cannot determine tmux session %s ownership or whether it survived; refusing to continue cleanup: %w",
					match, errors.Join(markerErr, probeErr))
			}
			return fmt.Errorf("cannot determine tmux session %s ownership; refusing to continue cleanup: %w", match, markerErr)
		}
		switch {
		case !ok:
			log.InfoLog.Printf("leaving tmux session %s: no AF_HOME ownership marker (pre-marker build or tmux <3.2); kill manually with: %s", match,
				shellsuggest.Command("tmux", "kill-session", "-t", "="+match))
		case filepath.Clean(home) != filepath.Clean(ownHome):
			log.InfoLog.Printf("leaving tmux session %s: owned by another agent-factory home (%s)", match, home)
		default:
			matches = append(matches, match)
		}
	}

	// Capture every session's pane process trees before any kill (#1104);
	// reap synchronously at the end because `af reset` is a short-lived CLI
	// process — a goroutine would die with it before the sweep ran.
	leakedBySession := make(map[string][]proctree.Process, len(matches))
	for _, match := range matches {
		leakedBySession[match] = SessionProcessTrees(cmdExec, match)
	}

	// Only sessions that are actually gone get their captured trees reaped —
	// a session that survives its kill still owns its processes.
	killed := make([]string, 0, len(matches))
	var killErr error
	for _, match := range matches {
		log.InfoLog.Printf("cleaning up session: %s", match)
		// `=` forces an exact session match so a name extracted from `tmux ls`
		// kills exactly that session and never a prefix-matching sibling (#1006).
		//
		// Bounded by tmuxCommandTimeout (#2099) so one undeletable session cannot
		// wedge the whole sweep.
		killCtx, killCancel := tmuxTimeoutContext()
		killErrRaw := runTmuxBoundedWith(killCtx, cmdExec, "kill-session", "-t", exactTarget(match))
		killTimedOut := killCtx.Err() != nil
		killCancel()
		if killErrRaw != nil {
			if killTimedOut {
				// Do NOT fall back to the sessionExists probe on a tripped
				// deadline: it spawns another tmux command against the same
				// wedged server and would hang identically, buying nothing (see
				// tmuxTimeoutContext). Report the wedge and stop.
				killErr = fmt.Errorf("%w: kill-session %s after %s", ErrTmuxTimeout, match, tmuxCommandTimeout)
				break
			}
			// Idempotent teardown (#967): a session can vanish between the
			// `tmux ls` above and this kill (TOCTOU). A gone session is the
			// goal of cleanup, but only an authoritative absence may turn the
			// kill error into success. A failed re-probe leaves teardown unknown
			// and must stop before process trees are reaped.
			exists, known, probeErr := probeSessionStrict(cmdExec, match)
			if !known {
				killErr = fmt.Errorf("failed to kill tmux session %s and cannot determine whether it survived: %w",
					match, errors.Join(killErrRaw, probeErr))
				break
			}
			if exists {
				killErr = fmt.Errorf("failed to kill tmux session %s: %v", match, killErrRaw)
				break
			}
		}
		killed = append(killed, match)
	}

	// Sweep concurrently: the grace waits overlap instead of serializing,
	// and the whole reset still blocks until every sweep finishes.
	var wg sync.WaitGroup
	for _, match := range killed {
		leaked := leakedBySession[match]
		if len(leaked) == 0 {
			continue
		}
		wg.Add(1)
		go func(match string, leaked []proctree.Process) {
			defer wg.Done()
			reapLeakedProcesses(match, leaked, reapGraceWait, reapTermWait)
		}(match, leaked)
	}
	wg.Wait()
	return killErr
}

func reapVanishedSessionProcesses(match, ownHome string, candidates []proctree.Process, captureErr error) error {
	var sweepErr error
	if captureErr != nil {
		sweepErr = fmt.Errorf("could not establish the complete pane process tree before ownership lookup: %w", captureErr)
	}
	// Two refresh/reap passes cover a helper that appears after the pre-marker
	// snapshot and a child it forks during the first bounded grace period. A
	// final non-destructive refresh below is the evidence that cleanup finished.
	for range 2 {
		refreshed, refreshErr := refreshOrphanCandidates(candidates, match)
		sweepErr = errors.Join(sweepErr, refreshErr)
		marked, inspectErr := markedOrphanProcesses(refreshed, match, ownHome)
		sweepErr = errors.Join(sweepErr, inspectErr)
		remaining := reapLeakedProcesses(match, marked, reapGraceWait, reapTermWait)
		if len(remaining) > 0 {
			pids := make([]string, 0, len(remaining))
			for _, process := range remaining {
				pids = append(pids, fmt.Sprintf("%d", process.PID))
			}
			sweepErr = errors.Join(sweepErr, fmt.Errorf("marked processes %s are still alive after bounded teardown",
				strings.Join(pids, ", ")))
		}
		candidates = refreshed
	}
	finalCandidates, refreshErr := refreshOrphanCandidates(candidates, match)
	left, inspectErr := markedOrphanProcesses(finalCandidates, match, ownHome)
	sweepErr = errors.Join(sweepErr, refreshErr, inspectErr)
	if len(left) > 0 {
		pids := make([]string, 0, len(left))
		for _, process := range left {
			pids = append(pids, fmt.Sprintf("%d", process.PID))
		}
		sweepErr = errors.Join(sweepErr, fmt.Errorf("marked processes %s appeared or remained after the orphan sweep",
			strings.Join(pids, ", ")))
	}
	return sweepErr
}
