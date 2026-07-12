package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Start creates and starts a new tmux session, then attaches to it. Program is the command to run in
// the session (ex. claude). workdir is the git worktree directory.
func (t *TmuxSession) Start(workDir string) error {
	// Check if the session already exists
	if t.DoesSessionExist() {
		return fmt.Errorf("tmux session already exists: %s", t.sanitizedName)
	}

	// Create a new detached tmux session and start claude in it. The -e
	// markers (when supported) let `af doctor` trace any process the pane
	// spawns back to this session even after it is orphaned (#1104).
	args := []string{"new-session", "-d", "-s", t.sanitizedName, "-c", workDir}
	args = append(args, sessionEnvFlags(t.sanitizedName)...)
	args = append(args, t.programCmd())
	cmd := exec.Command("tmux", args...)

	ptmx, err := t.ptyFactory.Start(cmd)
	if err != nil {
		// Cleanup any partially created session if any exists.
		if t.DoesSessionExist() {
			leaked := SessionProcessTrees(t.cmdExec, t.sanitizedName)
			cleanupCmd := exec.Command("tmux", "kill-session", "-t", exactTarget(t.sanitizedName))
			if cleanupErr := t.cmdExec.Run(cleanupCmd); cleanupErr != nil {
				err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
			} else if len(leaked) > 0 {
				go reapLeakedProcesses(t.sanitizedName, leaked, reapGraceWait, reapTermWait)
			}
		}
		return fmt.Errorf("error starting tmux session: %w", err)
	}

	// Poll for session existence with exponential backoff
	timeout := time.After(2 * time.Second)
	sleepDuration := 5 * time.Millisecond
	for !t.DoesSessionExist() {
		select {
		case <-timeout:
			ptmx.Close()
			// The pane program exiting instantly (bad path, rejected flag)
			// takes the whole tmux session down before this existence poll
			// ever sees it — name the likely cause and the exact command so
			// the user isn't left with a bare timeout (#1116, #1131).
			timeoutErr := fmt.Errorf("timed out waiting for tmux session %s; the pane program may have exited immediately after launch — check that it runs and accepts its flags (program: %q)", t.sanitizedName, t.programCmd())
			if cleanupErr := t.Close(); cleanupErr != nil {
				timeoutErr = fmt.Errorf("%v (cleanup error: %v)", timeoutErr, cleanupErr)
			}
			return timeoutErr
		default:
			time.Sleep(sleepDuration)
			// Exponential backoff up to 50ms max
			if sleepDuration < 50*time.Millisecond {
				sleepDuration *= 2
			}
		}
	}
	ptmx.Close()

	// Set history limit to enable scrollback (default is 2000, we'll use 10000 for more history)
	historyCmd := exec.Command("tmux", "set-option", "-t", exactTarget(t.sanitizedName), "history-limit", "10000")
	if err := t.cmdExec.Run(historyCmd); err != nil {
		log.InfoLog.Printf("Warning: failed to set history-limit for session %s: %v", t.sanitizedName, err)
	}

	// Enable mouse scrolling for the session
	mouseCmd := exec.Command("tmux", "set-option", "-t", exactTarget(t.sanitizedName), "mouse", "on")
	if err := t.cmdExec.Run(mouseCmd); err != nil {
		log.InfoLog.Printf("Warning: failed to enable mouse scrolling for session %s: %v", t.sanitizedName, err)
	}

	// Attach to the session we just created. Pass empty workDir so a missing
	// session here surfaces as an error rather than recursively re-spawning.
	err = t.Restore("")
	if err != nil {
		// Probe BEFORE Close (which kills the session): the existence poll
		// above saw the session, so if it is gone again by attach time the
		// pane program exited within milliseconds of launch. Say so instead
		// of the misleading "session does not exist" (#1116, #1131).
		vanished := !t.DoesSessionExist()
		if cleanupErr := t.Close(); cleanupErr != nil {
			err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
		}
		if vanished {
			return fmt.Errorf("tmux session %s vanished before attach; the pane program likely exited immediately after launch — check that it runs and accepts its flags (program: %q): %w", t.sanitizedName, t.programCmd(), err)
		}
		return fmt.Errorf("error restoring tmux session: %w", err)
	}

	return nil
}

// CheckAndHandleTrustPrompt checks the pane content once for a trust prompt and dismisses it if found.
// Returns true if the prompt was found and handled.
func (t *TmuxSession) CheckAndHandleTrustPrompt() bool {
	content, err := t.CapturePaneContent()
	if err != nil {
		return false
	}

	// Key off the agent actually running in the pane, token-matched — a loose
	// substring check would route e.g. /opt/claude-wrapper/run through the
	// claude branch (#1116 defect class).
	if DetectAgentFromCommand(t.programCmd()) == ProgramClaude {
		if strings.Contains(content, "Do you trust the files in this folder?") ||
			strings.Contains(content, "new MCP server") {
			if err := t.TapEnter(); err != nil {
				log.ErrorLog.Printf("could not tap enter on trust/MCP screen: %v", err)
				return false
			}
			return true
		}
	} else {
		if strings.Contains(content, "Open documentation url for more info") {
			if err := t.TapDAndEnter(); err != nil {
				log.ErrorLog.Printf("could not tap enter on trust screen: %v", err)
				return false
			}
			return true
		}
	}
	return false
}

// Restore attaches to an existing tmux session. If the session is missing
// (e.g. the tmux server died after a machine reboot, see #386) and workDir is
// non-empty, a fresh session is spawned in workDir using the same program so
// persisted instances can resume across reboots. If the session is missing
// and workDir is empty, the missing-session condition is surfaced as an error;
// real failures (PTY open errors, Start failures such as missing binaries or
// vanished worktrees) are always surfaced.
//
// When re-spawning, the program string is rewritten via resumeProgram so
// agents that expose a "resume the most recent session in cwd" flag pick
// the prior conversation back up instead of starting fresh (#595). Agents
// without such a flag, or programs that already include one, are left
// untouched.
func (t *TmuxSession) Restore(workDir string) error {
	if !t.DoesSessionExist() {
		if workDir == "" {
			return fmt.Errorf("tmux session %q does not exist", t.sanitizedName)
		}
		log.InfoLog.Printf("tmux session %q missing on Restore; re-spawning in %s", t.sanitizedName, workDir)
		t.setProgramCmd(resumeProgram(t.programCmd()))
		return t.Start(workDir)
	}

	// The session is live. Restore is now a pure logical rebind (#1592 Phase 2
	// PR7): it opens NO `tmux attach-session` render client — the local runtime's
	// data plane is the daemon's clientless agent-server (pipe-pane → WS broker,
	// PR5/6), and interactive full-screen attach is a WS subscriber in the client
	// (apiclient.AttachStream). All Restore still owes is a fresh status monitor,
	// swapped under monitorMu because the daemon poll may be inside HasUpdated()
	// reading the old pointer and mutating its fields right now (#1528).
	t.setMonitor(newStatusMonitor())
	return nil
}
