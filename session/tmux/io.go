package tmux

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/creack/pty"
	"github.com/sachiniyer/agent-factory/log"
)

type statusMonitor struct {
	// Store hashes to save memory.
	prevOutputHash []byte
	// dead is set once a capture-pane failure has been confirmed by
	// DoesSessionExist() reporting the tmux session is gone. While true,
	// HasUpdated short-circuits and emits no further logs so a stale
	// instance can't flood agent-factory.log (#489). A successful Start or
	// Restore replaces the monitor with a fresh one, which naturally clears
	// this state on respawn.
	dead bool
}

func newStatusMonitor() *statusMonitor {
	return &statusMonitor{}
}

// hash hashes the string.
func (m *statusMonitor) hash(s string) []byte {
	h := sha256.New()
	h.Write([]byte(s))
	return h.Sum(nil)
}

// TapEnter sends an enter keystroke to the tmux pane.
func (t *TmuxSession) TapEnter() error {
	// Detach failure (or Close) clears t.ptmx (#474), so callers that fire
	// keystrokes against a detached session must surface ErrSessionGone
	// instead of panicking on a nil Write (#510).
	if t.ptmx == nil {
		return ErrSessionGone
	}
	_, err := t.ptmx.Write([]byte{0x0D})
	if err != nil {
		return fmt.Errorf("error sending enter keystroke to PTY: %w", err)
	}
	return nil
}

// TapDAndEnter sends 'D' followed by an enter keystroke to the tmux pane.
func (t *TmuxSession) TapDAndEnter() error {
	if t.ptmx == nil {
		return ErrSessionGone
	}
	_, err := t.ptmx.Write([]byte{0x44, 0x0D})
	if err != nil {
		return fmt.Errorf("error sending enter keystroke to PTY: %w", err)
	}
	return nil
}

func (t *TmuxSession) SendKeys(keys string) error {
	if t.ptmx == nil {
		return ErrSessionGone
	}
	_, err := t.ptmx.Write([]byte(keys))
	return err
}

// HasUpdated checks if the tmux pane content has changed since the last tick. It also returns true if the tmux
// pane has a prompt for aider or claude code, plus the raw captured content so the daemon's usage-limit detector (#1146) can inspect it without a second capture ("" on early return).
func (t *TmuxSession) HasUpdated() (updated bool, hasPrompt bool, content string) {
	// A nil monitor means Restore never ran for this session: a persisted Dead
	// instance is loaded with started=true but LocalBackend.Start returns before
	// Restore (which is the only place monitor is initialized) so the corpse is
	// not re-spawned (#970). The daemon's refreshInstanceStatus still polls every
	// started instance, so HasUpdated must treat "no live monitor" as
	// nothing-to-report rather than panic on a nil deref and kill the refresh
	// goroutine, zombifying the daemon (#999).
	//
	// Once the underlying tmux session has been confirmed gone, stay silent
	// instead of relogging capture-pane failures every daemon tick (#489).
	if t.monitor == nil || t.monitor.dead {
		return false, false, ""
	}

	content, err := t.CapturePaneContent()
	if err != nil {
		// If the tmux session no longer exists, log once and latch the
		// monitor as dead so the daemon's per-second poll doesn't spam
		// the log (#489). Transient capture-pane failures while the
		// session is still alive are rare and still surface every tick.
		// CapturePaneContent has already probed DoesSessionExist on the
		// error path, so use the wrapped sentinel rather than re-probing.
		if errors.Is(err, ErrSessionGone) {
			log.ErrorLog.Printf("tmux session %s is gone; status monitor going silent (capture-pane error: %v)", t.sanitizedName, err)
			t.monitor.dead = true
			return false, false, ""
		}
		log.ErrorLog.Printf("error capturing pane content in status monitor: %v", err)
		return false, false, ""
	}

	// Only set hasPrompt for agents with a known confirmation dialog, keyed
	// off the agent actually running in the pane (a non-agent override or a
	// substring-matching path must not get an agent's prompt heuristic).
	switch DetectAgentFromCommand(t.programCmd()) {
	case ProgramClaude:
		hasPrompt = strings.Contains(content, "No, and tell Claude what to do differently")
	case ProgramAider:
		hasPrompt = strings.Contains(content, "(Y)es/(N)o/(D)on't ask again")
	case ProgramGemini:
		hasPrompt = strings.Contains(content, "Yes, allow once")
	}

	newHash := t.monitor.hash(content)
	if !bytes.Equal(newHash, t.monitor.prevOutputHash) {
		t.monitor.prevOutputHash = newHash
		return true, hasPrompt, content
	}
	return false, hasPrompt, content
}

// SetDetachedSize set the width and height of the session while detached. This makes the
// tmux output conform to the specified shape.
func (t *TmuxSession) SetDetachedSize(width, height int) error {
	// Detach failure (or Close) clears t.ptmx (#474), and the tmux session
	// may have been killed externally. Guard the ioctl so a missing PTY
	// surfaces as ErrSessionGone instead of panicking on nil.Fd() or
	// logging "bad file descriptor" at ERROR (#496).
	if t.ptmx == nil {
		return ErrSessionGone
	}
	if err := t.updateWindowSize(width, height); err != nil {
		if !t.DoesSessionExist() {
			return fmt.Errorf("%w: %v", ErrSessionGone, err)
		}
		return err
	}
	return nil
}

// updateWindowSize updates the window size of the PTY.
func (t *TmuxSession) updateWindowSize(cols, rows int) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
		X:    0,
		Y:    0,
	})
}

// CapturePaneContent captures the content of the tmux pane. When the
// capture fails and DoesSessionExist confirms the session is gone, the
// returned error wraps ErrSessionGone so non-daemon callers can degrade
// gracefully instead of logging at ERROR (#496).
func (t *TmuxSession) CapturePaneContent() (string, error) {
	// Add -e flag to preserve escape sequences (ANSI color codes). `=` forces
	// an exact session match: without it tmux would prefix-match a surviving
	// sibling session (e.g. the `__shell` tab) when the agent session has
	// died, capturing the wrong pane and masking the dead agent (#1006).
	cmd := exec.Command("tmux", "capture-pane", "-p", "-e", "-J", "-t", exactTarget(t.sanitizedName))
	output, err := t.cmdExec.Output(cmd)
	if err != nil {
		if !t.DoesSessionExist() {
			return "", fmt.Errorf("%w: capture-pane: %v", ErrSessionGone, err)
		}
		return "", fmt.Errorf("error capturing pane content: %v", err)
	}
	return string(output), nil
}

// CapturePaneContentWithOptions captures the pane content with additional options
// start and end specify the starting and ending line numbers (use "-" for the start/end of history).
// Wraps ErrSessionGone when the session has vanished, mirroring CapturePaneContent.
func (t *TmuxSession) CapturePaneContentWithOptions(start, end string) (string, error) {
	// Add -e flag to preserve escape sequences (ANSI color codes). `=` forces
	// an exact session match, mirroring CapturePaneContent (#1006).
	cmd := exec.Command("tmux", "capture-pane", "-p", "-e", "-J", "-S", start, "-E", end, "-t", exactTarget(t.sanitizedName))
	output, err := t.cmdExec.Output(cmd)
	if err != nil {
		if !t.DoesSessionExist() {
			return "", fmt.Errorf("%w: capture-pane: %v", ErrSessionGone, err)
		}
		return "", fmt.Errorf("failed to capture tmux pane content with options: %v", err)
	}
	return string(output), nil
}
