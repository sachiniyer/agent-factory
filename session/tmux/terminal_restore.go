package tmux

// NeutralTerminalRestore is written to stdout after an interactive raw-PTY
// stream ends, returning the terminal to the neutral state a well-behaved
// full-screen program leaves behind on exit: main screen, cursor visible, no
// scroll region, no mouse/focus/paste reporting, and legacy keyboard encoding.
//
// A raw attach stream — the remote hook's attach_cmd PTY, or (since #1592 Phase
// 2 PR7) the local session's clientless WS PTY stream — is copied byte-for-byte
// to the local terminal, so whatever modes the streamed program set (tmux/agents
// enter the alt screen, set a scroll region, enable mouse and focus reporting)
// are set on the local terminal too. On a graceful exit the program emits its own
// restore sequences, but the detach key ends the stream mid-flight and nothing
// resets those modes — a caller that then repaints into the terminal inherits a
// stale scroll region and screen buffer, the "messed up UI until I resize" of
// #845. Both drivers write this on every exit path.
//
// The keyboard-encoding modes matter for the same reason and were missed until
// #1833 made them reachable: a modern agent CLI negotiates the kitty keyboard
// protocol or xterm modifyOtherKeys at startup (see apiclient/detachkey.go), and
// the raw stream sets those on the REAL terminal. Until #1833 the detach key was
// unrecognizable in those modes, so this path was never reached from an upgraded
// terminal; now that detach works there, leaving the modes set would hand the
// user back a terminal that still reports Ctrl keys as escape sequences — host
// shortcuts like Ctrl-] break until a manual reset.
//
// The kitty flags are forced to zero rather than popped: the pane program may
// have pushed any number of stack entries, or used the set form and pushed none,
// so there is no pop depth that is right in every case, and an over-pop would
// discard a saved entry we never pushed. Zeroing the flags is depth-independent
// and idempotent, and it clobbers nothing — bubbletea v1 does not speak the
// kitty protocol, and the TUI re-asserts its own modes afterwards regardless.
//
// It is deliberately caller-agnostic: it serves the TUI (which re-asserts its own
// bubbletea modes afterwards — see app.attachOverlayCallback) and the plain
// `af sessions attach` CLI (for which this neutral state is exactly right).
// Hand-rolled escapes are the only option — there is no bubbletea program at this
// layer.
//
// Order matters: reporting modes off first (so nothing new arrives while we
// restore), then keyboard modes, then geometry, then the buffer switch, and
// finally cosmetics on the buffer we land on. The scroll region is reset on both
// sides of the 1049l switch because emulators disagree on whether DECSTBM margins
// are shared or per-buffer.
const NeutralTerminalRestore = "" +
	"\x1b[?1003l\x1b[?1002l\x1b[?1000l" + // all mouse tracking variants off
	"\x1b[?1015l\x1b[?1006l\x1b[?1005l" + // all mouse encoding extensions off
	"\x1b[?1004l" + // focus reporting off
	"\x1b[?2004l" + // bracketed paste off
	"\x1b[=0;1u" + // kitty keyboard protocol: all enhancement flags off
	"\x1b[>4;0m" + // xterm modifyOtherKeys off
	"\x1b[?1l\x1b>" + // cursor keys and keypad back to normal mode
	"\x1b[?7h" + // autowrap back on (xterm default)
	"\x1b[r" + // scroll region = full screen (current buffer)
	"\x1b[?1049l" + // back to the main screen buffer (no-op if already there)
	"\x1b[r" + // scroll region again on the main buffer
	"\x1b[0m" + // SGR attributes reset
	"\x1b[?25h" // cursor visible
