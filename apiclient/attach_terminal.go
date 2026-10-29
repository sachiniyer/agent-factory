package apiclient

import (
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// handbackSignals are the signals whose DEFAULT disposition terminates the
// process without running a single deferred function. During an attach that is
// a terminal leak: the process dies, the termios stays raw, and the shell the
// user is dropped back into has no echo and no line editing.
//
// SIGQUIT is deliberately absent. Its default disposition in a Go program is
// the runtime goroutine dump, and taking it over here would trade a debugging
// affordance (kill -QUIT on a wedged attach) for a terminal the runtime is
// about to scribble a traceback onto anyway.
//
// Keyboard-generated signals are not the case this covers: the attach holds the
// terminal in raw mode, so ISIG is off and ctrl+c is a byte the pane receives.
// What reaches an attached process is an external kill.
//
// This does not fight bubbletea for the TUI. Bubble Tea watches SIGINT/SIGTERM
// too, but a full-screen attach is bracketed by ReleaseTerminal (#2157), which
// sets its ignoreSignals flag — so for the whole attach it takes those signals
// off its channel and DISCARDS them, and an attached TUI outlives a SIGTERM
// today while leaving the user a raw terminal on any signal it does not catch.
// Restoring and then re-raising ends that: the terminal comes back and the
// signal means what it says.
var handbackSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// terminalHandback owns the one thing a full-screen attach borrows from the
// user: the terminal. AttachStream takes it with term.MakeRaw, the pane program
// then negotiates whatever modes it likes onto it (alt screen, mouse reporting,
// kitty keyboard), and every exit from the attach owes all of that back.
//
// "Every exit" is the whole point, and is what #2784 was about. The driver used
// to restore the terminal in its tail, which covers the exits somebody thought
// of — a detach, a server disconnect — and silently skips the ones nobody wrote
// a handler for. A panic anywhere in the driver, or a signal from outside, left
// the user in a raw shell where the recovery (reset, or stty sane) is the
// hardest thing there is to type. So the hand-back is deferred rather than
// tail-called, and armed against the terminating signals as well.
//
// It is NOT a panic handler: nothing here recovers. The terminal comes back and
// the panic still crashes the process with its stack trace, which is the only
// way a real bug in the driver stays visible.
type terminalHandback struct {
	out      io.Writer
	oldState *term.State
	// mu guards restored/finished. A plain Once is not enough: the signal path can
	// legitimately hand the terminal back and then take it BACK, so the state has
	// to be readable and reversible rather than write-once.
	mu       sync.Mutex
	restored bool
	// finished is set by release. It is what stops a signal watcher from retaking
	// a terminal the attach has already given up for good — that would hand the
	// user a raw shell, which is the bug, arrived at backwards.
	finished bool
	// stopWatch tears down the signal watch armed by beginTerminalHandback; nil
	// when there was no raw mode to guard.
	stopWatch func()
}

// restoreTermios is term.Restore; a var so tests can observe and gate the step.
var restoreTermios = term.Restore

// activeHandback is the hand-back of the attach in flight, if any. It exists so a
// caller that must die on a signal while an attach owns the terminal (the TUI's
// exit guard, #6072) restores termios through THIS owner instead of racing it: a
// second signal handler that re-raised first killed the process before the
// attach's own term.Restore ran, leaving the terminal raw.
var activeHandback atomic.Pointer[terminalHandback]

// RestoreAttachedTerminal hands the terminal back through the in-flight attach's
// own restore, and is a no-op when no attach is running or it already restored
// (restore is idempotent under h.mu, so racing the attach's watcher, its deferred
// release and any number of callers restores exactly once and never re-applies a
// stale state). Callers that are about to terminate the process call it BEFORE
// re-raising the signal.
func RestoreAttachedTerminal() {
	if h := activeHandback.Load(); h != nil {
		h.restore()
	}
}

// handbackRetakeGrace is how long the signal watcher waits after re-raising a
// signal before concluding that it could not terminate the process. A fatal
// disposition kills us well inside this window; anything still running after it
// was ignored.
var handbackRetakeGrace = 200 * time.Millisecond

// beginTerminalHandback arms the hand-back for an attach that has just put the
// terminal into raw mode, and starts watching for the signals that would
// otherwise kill the process with the terminal still borrowed. out is the
// caller's already-snapshotted stdout seam; oldState is the termios MakeRaw
// captured, and is nil when no raw mode was taken (a non-TTY, or a test driving
// the proxy over pipes) — in which case there is nothing borrowed and nothing
// to guard.
func beginTerminalHandback(out io.Writer, oldState *term.State) *terminalHandback {
	h := &terminalHandback{out: out, oldState: oldState}
	activeHandback.Store(h)
	h.armWatch()
	return h
}

// beginRawTerminalHandback takes the terminal (makeRaw) AND arms the hand-back as
// one step, with the owner reachable BEFORE the terminal is borrowed (#6083): the
// handback is published and h.mu is held across makeRaw, so a signal arriving
// while the terminal is being taken (or at any instant after) reaches an owner.
// Its restore blocks on h.mu until the raw mode and the termios to restore are
// both recorded, then restores them — there is no instant between "raw" and
// "someone can restore it" for a signal to fall into. A failed makeRaw borrowed
// nothing, so the owner is retired and the error returned.
func beginRawTerminalHandback(out io.Writer, makeRaw func() (*term.State, error)) (*terminalHandback, error) {
	h := &terminalHandback{out: out}
	h.mu.Lock()
	activeHandback.Store(h)
	oldState, err := makeRaw()
	if err != nil {
		h.finished, h.restored = true, true // nothing was borrowed
		h.mu.Unlock()
		activeHandback.CompareAndSwap(h, nil)
		return nil, err
	}
	h.oldState = oldState
	h.mu.Unlock()
	h.armWatch()
	return h, nil
}

// armWatch starts watching for the signals that would otherwise kill the process
// with the terminal still borrowed. A hand-back with no termios (a non-TTY, or a
// test driving the proxy over pipes) borrowed nothing and guards nothing.
func (h *terminalHandback) armWatch() {
	if h.oldState == nil {
		return
	}
	// Watch only the signals that could actually kill this process. One inherited
	// as ignored (nohup, a disowned job, a shell trap that ignores HUP — SIG_IGN
	// survives execve) cannot, and notifying on it would be actively harmful:
	// os/signal takes the ignore away to deliver it, then Reset puts the SAME
	// ignore back, so the re-raise is discarded and the process lives on with a
	// terminal it has already handed back while the attach is still proxying it.
	// Nothing to guard against is the correct reading — leave those signals alone.
	// Ignored must be asked BEFORE Notify, which reports false once a watch is
	// armed.
	watched := make([]os.Signal, 0, len(handbackSignals))
	for _, sig := range handbackSignals {
		if signal.Ignored(sig) {
			continue
		}
		watched = append(watched, sig)
	}
	if len(watched) == 0 {
		return
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, watched...)
	done := make(chan struct{})
	var stopOnce sync.Once
	h.stopWatch = func() {
		stopOnce.Do(func() {
			signal.Stop(sigs)
			close(done)
		})
	}
	go func() {
		for {
			select {
			case <-done:
				return
			case sig := <-sigs:
				h.restore()
				// Then die exactly as we would have without the handler: reset the
				// signal to its default disposition and re-raise it, so the process
				// still terminates and still reports the signal in its wait status.
				// Restoring and CONTINUING would be worse than not handling it —
				// the attach would go on proxying a terminal it no longer owns.
				signal.Reset(sig)
				if s, ok := sig.(syscall.Signal); ok {
					_ = syscall.Kill(os.Getpid(), s)
				}
				// Still here after the grace: that signal could not kill us, so the
				// hand-back was premature and the attach still owns the terminal.
				// TAKE IT BACK rather than proxying a raw byte stream to a cooked
				// terminal. Without this the handler is WORSE than no handler at all
				// for these signals, which ignored them and left the attach intact.
				//
				// Two ways to get here, and the filter above catches neither:
				//   - the process inherited the signal as ignored, but something else
				//     had already called Notify for it, so signal.Ignored could not
				//     see it. Bubble Tea does exactly that for SIGINT/SIGTERM at
				//     startup, which makes this the TUI's normal state, and Reset
				//     restores that inherited ignore;
				//   - the kernel suppresses the default action (pid 1 in a container).
				//
				// Looping also matters: returning would leave the remaining signals
				// armed but unread, swallowing them for the rest of the process life.
				time.Sleep(handbackRetakeGrace)
				h.retake()
			}
		}
	}()
}

// restore hands the terminal back, at most once however many exits race for it.
//
// Two halves, both required: the pane program set modes on the terminal that no
// termios restore touches (alt screen, mouse and paste reporting, keyboard
// encoding), so the neutral restore drops those back to a plain terminal (#845,
// local edition) before the termios itself goes back to whoever owned it before
// the attach — bubbletea for the TUI, the shell for the CLI.
func (h *terminalHandback) restore() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.restoreLocked()
}

func (h *terminalHandback) restoreLocked() {
	if h.restored {
		return
	}
	h.restored = true
	_, _ = io.WriteString(h.out, tmux.NeutralTerminalRestore)
	if h.oldState != nil {
		_ = restoreTermios(int(os.Stdin.Fd()), h.oldState)
	}
}

// retake puts the terminal back into raw mode after a hand-back that turned out
// to be premature — the signal it was made for could not end the process. The
// attach is still running and still owns the terminal, so this restores the
// invariant rather than leaving the two disagreeing.
//
// It deliberately does nothing once release has run: an attach that is over has
// given the terminal up for good, and re-raw-ing it then would hand the user the
// exact broken shell this file exists to prevent.
func (h *terminalHandback) retake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished || !h.restored || h.oldState == nil {
		return
	}
	if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
		return // could not re-raw: a cooked terminal beats a wedged one
	}
	h.restored = false
}

// release is the deferred hand-back: restore, then stop the signal watch. Safe
// to call after the signal path already restored — the terminal goes back
// exactly once either way.
//
// The order is load-bearing. Disarming first would put the default disposition
// back while the termios is still raw, so a signal landing in that window would
// kill the process on exactly the exit this type exists to close. Restoring
// first leaves only the harmless window: a signal after the terminal is already
// back.
func (h *terminalHandback) release() {
	h.mu.Lock()
	h.finished = true // no retake can follow the final hand-back
	h.restoreLocked()
	h.mu.Unlock()
	// Ownership is given up only AFTER the restore it guards has completed (#6083).
	// Unpublishing first left a window — a signal after the swap but before (or
	// while) the restore ran found no owner, and the exit guard re-raised with the
	// termios still raw. Published through the restore, a concurrent
	// RestoreAttachedTerminal either blocks on h.mu behind it or finds the
	// terminal already restored.
	activeHandback.CompareAndSwap(h, nil)
	if h.stopWatch != nil {
		h.stopWatch()
	}
}
