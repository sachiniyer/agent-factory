package app

import (
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sachiniyer/agent-factory/apiclient"
)

// terminalExitRestore is written once af is done with the terminal, on every exit
// path, when a full-screen attach ever had it (#6063). An attach is a raw byte
// proxy (or a nested tmux client) that drives the terminal itself — alt screen,
// scroll region, cursor, SGR, mouse and paste reporting — and bubbletea's own
// shutdown only undoes what bubbletea knows it set. If anything the attach did
// outlives the TUI the shell resumes drawing into it: a stale scroll region, a
// hidden cursor, a cursor parked mid-screen inside the attach's last frame.
//
// Order matters, and mirrors tmux.NeutralTerminalRestore: reporting modes off
// first, then geometry, then the buffer switch, then cosmetics on the buffer the
// shell lands on. The final "\r\x1b[J" erases from the shell's cursor down — never
// above it, so the launch line stays (the #4785 trap: clearing the whole main
// screen leaves a blank pane) — which is exactly where an attach frame that
// reached the main screen sits. DECSTBM (the scroll-region reset) homes the
// cursor as a side effect, so the cursor is saved and restored around it; without
// that the erase ran from the top-left and wiped the whole screen (#6071). Each reset is bracketed on its own: when af is already on the main screen (it is, after a normal quit) the FIRST reset homes the main cursor, and a 1049l after it is a no-op that cannot bring the cursor back.
const terminalExitRestore = "" +
	"\x1b[?1003l\x1b[?1002l\x1b[?1000l" + // mouse tracking off
	"\x1b[?1015l\x1b[?1006l\x1b[?1005l" + // mouse encodings off
	"\x1b[?1004l\x1b[?2004l" + // focus reporting and bracketed paste off
	"\x1b[?1l\x1b>" + // cursor keys and keypad normal
	"\x1b[?7h" + // autowrap on
	"\x1b7\x1b[r\x1b8" + // scroll region = full screen on the CURRENT buffer, cursor kept (DECSTBM homes it)
	"\x1b[?1049l" + // leave the alt screen (a no-op if already on the main one)
	"\x1b7\x1b[r\x1b8" + // and again on the main buffer, whichever one the first reset ran on (#6071)
	"\x1b[0m" + // SGR reset
	"\x1b[?25h" + // cursor visible
	"\r\x1b[J" // erase from the shell's cursor down

// terminalExitWriter is where terminalExitRestore goes; swappable for tests.
var terminalExitWriter io.Writer = os.Stdout

// restoreTerminalAtExit writes terminalExitRestore if a full-screen attach ever
// took the terminal. A run that never attached writes nothing, so quitting
// without attaching stays byte-for-byte what it was.
func (m *home) restoreTerminalAtExit(out io.Writer) {
	if m.tookTerminal.Load() {
		_, _ = io.WriteString(out, terminalExitRestore)
	}
}

// guardTerminalOnSignal restores the terminal on the signals bubbletea does not
// turn into a clean quit (it consumes SIGINT and SIGTERM, which end p.Run and so
// reach the deferred restore; SIGHUP and SIGQUIT would otherwise end the process
// with the attach's state on the terminal). It writes the restore and re-raises
// the signal with its default action, so the process still dies as it would have.
// The returned func stops the guard.
func (m *home) guardTerminalOnSignal(out io.Writer) func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			exitOnSignal(m, sig, out, apiclient.RestoreAttachedTerminal, func(s syscall.Signal) {
				signal.Reset(s)
				_ = syscall.Kill(os.Getpid(), s)
			})
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// exitOnSignal is the guard's reaction, with its two side effects injected so the
// ordering is testable. Exactly one owner restores termios: while an attach holds
// the terminal that is the attach's own hand-back (restoreAttached), which is
// idempotent and so safe against the attach's own signal watcher getting the same
// signal; this guard only adds the escape-sequence restore and re-raises. The
// termios restore must complete BEFORE the re-raise, or the process dies with the
// terminal raw (#6072).
func exitOnSignal(m *home, sig os.Signal, out io.Writer, restoreAttached func(), kill func(syscall.Signal)) {
	restoreAttached()
	m.restoreTerminalAtExit(out)
	if s, ok := sig.(syscall.Signal); ok {
		kill(s)
	}
}
