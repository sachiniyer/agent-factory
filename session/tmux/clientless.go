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
// probing ExistsOrUnknown — see tmuxTimeoutContext.
func (t *TmuxSession) EnablePipePane(shellCommand string) error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "pipe-pane", "-O", "-t", exactTarget(t.sanitizedName), shellCommand); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: pipe-pane after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
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
//
// The !ExistsOrUnknown no-op success below is gated on the DEFINITIVELY-absent
// branch only, and safely (#1962): the timeout is already handled above, so this
// runs only when pipe-pane answered with a fast error, and a wedged→"exists"
// keeps that a real error rather than a false no-op — never the other way.
func (t *TmuxSession) DisablePipePane() error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "pipe-pane", "-t", exactTarget(t.sanitizedName)); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: pipe-pane after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
			return nil
		}
		return fmt.Errorf("error disabling pipe-pane: %w", err)
	}
	return nil
}

// sendRawKeysMaxChunk bounds how many INPUT bytes go into one `send-keys -H`
// command, because tmux bounds the command itself. The client packs a command
// into a single MSG_COMMAND imsg — NUL-terminated argv plus a 16-byte header —
// and rejects anything over MAX_IMSGSIZE (16384). `-H` spends one 2-char
// argument per input byte, so a command costs ~3 bytes per byte sent and the
// limit lands at ~5,445 bytes of input; past that tmux answers "failed to send
// command" and the input is silently dropped (#2414).
//
// 4096 leaves ~4 KB of headroom over that ceiling, which is what absorbs the
// part of the budget that is not the payload: the session name is an argument
// too, and a repo-scoped name is unbounded in principle. Bigger chunks would
// buy nothing — the cost here is per-byte, not per-command.
const sendRawKeysMaxChunk = 4096

// SendRawKeys writes verbatim input bytes to the pane using `send-keys -H`
// (hex-encoded), so arbitrary control bytes (arrow keys, Ctrl sequences) land
// exactly as typed — the interactive multi-writer input path. Empty input is a
// no-op.
//
// Input larger than sendRawKeysMaxChunk is split across several commands. A web
// terminal delivers a paste as ONE frame (xterm.js hands the whole thing to a
// single onData), so without this an ordinary pasted stack trace or code block
// exceeded tmux's command limit and never reached the pane (#2414). Splitting is
// safe because this is a byte STREAM, not a message: the chunks preserve order
// and content exactly, so a receiving application's parser cannot tell where the
// boundaries fell — including one landing inside a bracketed-paste marker. The
// TUI attach path already relied on exactly this, reading stdin in 32-byte
// chunks (apiclient/attach.go), which is the only reason it never hit the limit.
//
// A chunked send is NOT atomic, and deliberately fails loudly rather than
// partially: on a chunk error it stops and propagates, so the caller sees a
// failed send instead of a success that delivered a truncated paste (the silent
// prompt-corruption class of #1982/#2099). Concurrent writers were already
// interleaved at frame granularity — the broker's input path takes no lock and
// the TUI path has always sent many small frames — so this adds no ordering
// guarantee that callers previously had.
//
// Each chunk is bounded by tmuxCommandTimeout (#1787). This runs on the WS reader
// goroutine, which holds no broker lock, so a stall here is milder than the
// pipe-pane pair above — it strands one connection's input loop rather than the
// session's capture transitions. It is bounded anyway to keep ONE invariant over
// the whole clientless channel — no tmux command on the WS data path is unbounded
// — rather than leaving a second, subtler way for a wedged server to park a
// goroutine.
func (t *TmuxSession) SendRawKeys(b []byte) error {
	for len(b) > 0 {
		n := min(len(b), sendRawKeysMaxChunk)
		if err := t.sendRawKeysChunk(b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// sendRawKeysChunk issues one `send-keys -H` for at most sendRawKeysMaxChunk
// bytes. The caller guarantees the size bound; this owns the argv shape, the
// deadline, and the error classification for a single command.
func (t *TmuxSession) sendRawKeysChunk(b []byte) error {
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
		if !t.ExistsOrUnknown() {
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
		if !t.ExistsOrUnknown() {
			return fmt.Errorf("%w: resize-window", ErrSessionGone)
		}
		// resize-window fails on tmux servers older than 2.9; surface the error so
		// the caller can log it rather than silently mis-sizing the pane.
		return fmt.Errorf("error resizing window: %w", err)
	}
	return nil
}
