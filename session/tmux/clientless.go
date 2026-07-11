package tmux

import (
	"fmt"
	"os/exec"
)

// Clientless control channel (#1592 Phase 2 PR5). These primitives capture pane
// output and drive input/size WITHOUT a `tmux attach-session` render client:
//
//   - EnablePipePane/DisablePipePane stream the pane's raw output to a shell
//     command via `tmux pipe-pane` — the WS PTY broker points it at a FIFO it
//     drains, so producing the byte stream no longer needs a tmux client.
//   - SendRawKeys writes verbatim input bytes with `send-keys -H` (hex), the
//     multi-writer interactive-input path (distinct from the SendKeysCommand
//     paste-buffer path automated deliveries use).
//   - ResizeWindow sets the window size with `resize-window`, which pins the
//     window to a manual size independent of any attached client.
//
// This is the structural move the epic set up: once the daemon produces the
// stream clientlessly, PR6 can delete the attach-session render client the TUI
// still uses today. Every command uses the exact `=name:` target so a
// prefix-matched sibling session (e.g. a `__shell` tab) can never be driven by
// mistake (#1006).

// EnablePipePane starts piping the pane's raw output to shellCommand via
// `tmux pipe-pane -O` (output only). tmux runs shellCommand through /bin/sh, so
// the broker passes e.g. `cat >> <fifo>`. A pane may hold only one pipe at a
// time; DisablePipePane closes it.
func (t *TmuxSession) EnablePipePane(shellCommand string) error {
	cmd := exec.Command("tmux", "pipe-pane", "-O", "-t", exactTarget(t.sanitizedName), shellCommand)
	if err := t.cmdExec.Run(cmd); err != nil {
		if !t.DoesSessionExist() {
			return fmt.Errorf("%w: pipe-pane", ErrSessionGone)
		}
		return fmt.Errorf("error enabling pipe-pane: %w", err)
	}
	return nil
}

// DisablePipePane closes the pane's current output pipe. `tmux pipe-pane` with no
// shell-command closes the active pipe (tmux man page); a missing session is a
// no-op success since the pipe is already gone with it.
func (t *TmuxSession) DisablePipePane() error {
	cmd := exec.Command("tmux", "pipe-pane", "-t", exactTarget(t.sanitizedName))
	if err := t.cmdExec.Run(cmd); err != nil {
		if !t.DoesSessionExist() {
			return nil
		}
		return fmt.Errorf("error disabling pipe-pane: %w", err)
	}
	return nil
}

// SendRawKeys writes verbatim input bytes to the pane using `send-keys -H`
// (hex-encoded), so arbitrary control bytes (arrow keys, Ctrl sequences) land
// exactly as typed — the interactive multi-writer input path. Empty input is a
// no-op.
func (t *TmuxSession) SendRawKeys(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	args := make([]string, 0, len(b)+4)
	args = append(args, "send-keys", "-t", exactTarget(t.sanitizedName), "-H")
	for _, c := range b {
		args = append(args, fmt.Sprintf("%02x", c))
	}
	cmd := exec.Command("tmux", args...)
	if err := t.cmdExec.Run(cmd); err != nil {
		if !t.DoesSessionExist() {
			return fmt.Errorf("%w: send-keys", ErrSessionGone)
		}
		return fmt.Errorf("error sending raw keys: %w", err)
	}
	return nil
}

// ResizeWindow sets the window to cols×rows with `resize-window -x -y`, which
// switches the window to a manual size so it no longer tracks an attached
// client's dimensions — the clientless last-resize-wins size the broker applies.
func (t *TmuxSession) ResizeWindow(cols, rows int) error {
	cmd := exec.Command("tmux", "resize-window", "-t", exactTarget(t.sanitizedName),
		"-x", fmt.Sprintf("%d", cols), "-y", fmt.Sprintf("%d", rows))
	if err := t.cmdExec.Run(cmd); err != nil {
		if !t.DoesSessionExist() {
			return fmt.Errorf("%w: resize-window", ErrSessionGone)
		}
		// resize-window fails on tmux servers older than 2.9; surface the error so
		// the caller can log it rather than silently mis-sizing the pane.
		return fmt.Errorf("error resizing window: %w", err)
	}
	return nil
}
