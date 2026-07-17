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

func (t *TmuxSession) DoesSessionExist() bool {
	return sessionExists(t.cmdExec, t.sanitizedName)
}

// sessionExists reports whether a tmux session with the exact name `name`
// currently exists. Shared by DoesSessionExist and the receiver-less
// CleanupSessions path so both probe identically.
func sessionExists(cmdExec cmd.Executor, name string) bool {
	// Using "-t name" does a prefix match, which is wrong. `-t=` does an exact match.
	existsCmd := exec.Command("tmux", "has-session", fmt.Sprintf("-t=%s", name))
	return cmdExec.Run(existsCmd) == nil
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
