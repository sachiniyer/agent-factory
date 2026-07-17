package tmux

import (
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
	// First try to list sessions
	cmd := exec.Command("tmux", "ls")
	output, err := cmdExec.Output(cmd)

	// If there's an error and it's because no server is running, that's fine
	// Exit code 1 typically means no sessions exist
	if err != nil {
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

	// Home-scope the sweep (#1122): the af_ prefix alone does not prove this
	// home owns the session — another install or an escaped test process can
	// see the same server. Only the AF_HOME marker match does.
	ownHome, err := afHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve this agent-factory home; refusing to sweep tmux sessions: %w", err)
	}
	matches := make([]string, 0, len(prefixed))
	for _, match := range prefixed {
		home, ok := sessionHomeMarker(cmdExec, match)
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
		if err := cmdExec.Run(exec.Command("tmux", "kill-session", "-t", exactTarget(match))); err != nil {
			// Idempotent teardown (#967): a session can vanish between the
			// `tmux ls` above and this kill (TOCTOU). A gone session is the
			// goal of cleanup, so only a survivor is a real failure.
			if sessionExists(cmdExec, match) {
				killErr = fmt.Errorf("failed to kill tmux session %s: %v", match, err)
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
