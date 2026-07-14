package tmux

import "fmt"

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
//
// Bounded by tmuxCommandTimeout (#1787). The broker calls this while holding
// captureMu, so an unbounded stall would strand that mutex and deadlock every
// LATER capture start/stop for the session — the deadline is what keeps the
// lock hold finite. On a tripped deadline it returns ErrTmuxTimeout without
// probing DoesSessionExist — see tmuxTimeoutContext.
func (t *TmuxSession) EnablePipePane(shellCommand string) error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "pipe-pane", "-O", "-t", exactTarget(t.sanitizedName), shellCommand); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: pipe-pane after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
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
//
// Bounded by tmuxCommandTimeout (#1787), for the same captureMu reason as
// EnablePipePane — this runs on the teardown half of the transition. A tripped
// deadline reports ErrTmuxTimeout rather than the missing-session no-op success:
// the server is wedged, so whether the pipe is actually closed is UNKNOWN, and
// silently claiming success would let the broker believe it had torn down a pipe
// that may still be writing.
func (t *TmuxSession) DisablePipePane() error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "pipe-pane", "-t", exactTarget(t.sanitizedName)); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: pipe-pane after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
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
//
// Bounded by tmuxCommandTimeout (#1787). This runs on the WS reader goroutine,
// which holds no broker lock, so a stall here is milder than the pipe-pane pair
// above — it strands one connection's input loop rather than the session's
// capture transitions. It is bounded anyway to keep ONE invariant over the whole
// clientless channel — no tmux command on the WS data path is unbounded — rather
// than leaving a second, subtler way for a wedged server to park a goroutine.
func (t *TmuxSession) SendRawKeys(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	args := make([]string, 0, len(b)+4)
	args = append(args, "send-keys", "-t", exactTarget(t.sanitizedName), "-H")
	for _, c := range b {
		args = append(args, fmt.Sprintf("%02x", c))
	}
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, args...); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: send-keys after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
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
//
// Bounded by tmuxCommandTimeout (#1787), for the same whole-channel invariant as
// SendRawKeys. The broker already applies the size best-effort (it broadcasts its
// authoritative resize echo regardless of whether this succeeds), so a wedged
// server costs the pane resize, never the echo clients reflow on.
func (t *TmuxSession) ResizeWindow(cols, rows int) error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "resize-window", "-t", exactTarget(t.sanitizedName),
		"-x", fmt.Sprintf("%d", cols), "-y", fmt.Sprintf("%d", rows)); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: resize-window after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.DoesSessionExist() {
			return fmt.Errorf("%w: resize-window", ErrSessionGone)
		}
		// resize-window fails on tmux servers older than 2.9; surface the error so
		// the caller can log it rather than silently mis-sizing the pane.
		return fmt.Errorf("error resizing window: %w", err)
	}
	return nil
}
