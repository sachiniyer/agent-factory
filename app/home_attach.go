package app

import (
	"context"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// remoteDetachTerminalReassert re-establishes the terminal modes bubbletea
// set at startup (see Run: WithAltScreen + WithMouseCellMotion, plus the
// hidden cursor and the bracketed paste bubbletea enables by default) after a
// remote attach stream has scribbled over them. The attach driver's neutral
// restore hands the terminal back on the MAIN screen with the cursor visible
// and all reporting modes off — correct for the CLI attach path, but not the
// state this TUI runs in.
//
// Hand-rolled rather than bubbletea-native, for two reasons (#845):
//   - bubbletea cannot re-assert state it believes is already set: the
//     renderer's enterAltScreen() is a no-op while its altScreenActive
//     bookkeeping is true, and that bookkeeping never saw the remote PTY's
//     writes. An ExitAltScreen/EnterAltScreen dance defeats the guard, but
//     runs as queued program msgs racing the post-detach msg backlog — diff
//     frames could land on the main screen first, leaking TUI content into
//     the shell's scrollback.
//   - Writing synchronously here, while the Update goroutine is still blocked
//     inside the onDismiss callback, guarantees the terminal is back in the
//     state the renderer assumes before it can emit a single frame.
//
// The renderer's diff cache is still stale after this (it thinks the
// pre-attach frame is on screen; the 1049h re-entry cleared it), so the
// caller follows up with tea.ClearScreen — the native lever for "invalidate
// the cache and repaint everything".
//
// Since #2157 the attach is bracketed by ReleaseTerminal/RestoreTerminal, which
// re-asserts most of these itself from bookkeeping that is no longer stale. This
// constant stays load-bearing anyway for the MOUSE modes: bubbletea enables them
// once at startup from the WithMouseCellMotion option and RestoreTerminal does
// not, so without this write mouse scroll and click would be dead for the rest of
// the session after the first attach.
const remoteDetachTerminalReassert = "" +
	"\x1b[?1049h" + // re-enter the alt screen (terminal clears it)
	"\x1b[?25l" + // bubbletea hid the cursor at startup; re-hide it
	"\x1b[?1002h\x1b[?1006h" + // WithMouseCellMotion + SGR encoding
	"\x1b[?2004h" // bracketed paste (bubbletea default-on)

// remoteDetachResetWriter is where remoteDetachTerminalReassert is written —
// the real terminal in production, swappable so tests can capture it.
var remoteDetachResetWriter io.Writer = os.Stdout

// attachAltScreenEnter puts the terminal back on the alt screen for the
// duration of an attach, immediately after releaseTerminalToAttach.
//
// "Released" means Bubble Tea has handed the terminal back the way it found it,
// which includes leaving the alt screen. Without this the raw attach proxy would
// paint the pane onto the MAIN screen — straight into the user's scrollback,
// where it stays after detach. Every full-screen attach has always run on the alt
// screen (Bubble Tea's own, until #2157 made the release necessary), so this just
// keeps that true: the pane gets a scratch screen, and detach hands the user back
// the terminal they attached from.
//
// It is written directly rather than through the renderer because the renderer is
// stopped and its bookkeeping must stay as ReleaseTerminal left it: the attach's
// NeutralTerminalRestore leaves the terminal on the main screen again, which is
// exactly the state restoreTerminal then re-enters the alt screen from.
const attachAltScreenEnter = "\x1b[?1049h"

type beginAttachMsg struct {
	run func() tea.Cmd
}

// beginAttachTransition gives Bubble Tea one explicit blank/clear frame before
// the existing blocking attach callback hands stdout to tmux. Direct writes from
// the callback are too late for the renderer-owned status/footer diff; the final
// TUI frame itself must contain no AF chrome (#1448).
func (m *home) beginAttachTransition(run func() tea.Cmd) tea.Cmd {
	// Re-entry guard (#1530): every full-screen attach entry point funnels
	// through here, so this is the reliable place to block a second attach from
	// starting while one is already in flight. Without it, repeated Enter/`o`
	// presses during the transition window each schedule an independent attach
	// flow — duplicate heartbeat/resume goroutines, racing m.attached.Store,
	// doubled post-detach side effects, and possible terminal corruption. The
	// flags clear on detach (attachTransitioning here, attached via its defer in
	// attachOverlayCallback), so this only ignores presses until then.
	if m.attachTransitioning || m.attached.Load() {
		return nil
	}
	m.attachTransitioning = true
	return tea.Tick(20*time.Millisecond, func(time.Time) tea.Msg {
		return beginAttachMsg{run: run}
	})
}

// attachOverlayCallbackFn is the indirection handleEnter reaches
// attachOverlayCallback through. Production points it at the method; tests swap
// it to substitute a hermetic attach func (no real WS PTY stream or remote
// terminal_cmd PTY) while preserving the call-site behaviour.
var attachOverlayCallbackFn = (*home).attachOverlayCallback

// attachStreamFn is the indirection attachInstanceTab dials the daemon's WS PTY
// stream through — the SOLE full-screen attach byte source for every session,
// local or remote (#1837). Production points it at apiclient; tests swap it to
// observe the routing without standing up a daemon.
var attachStreamFn = func(ctx context.Context, title, repoID, tabID string, tabIdx int) (chan struct{}, error) {
	c, err := apiclient.NewTargeted()
	if err != nil {
		return nil, err
	}
	return c.AttachStream(ctx, title, repoID, tabID, tabIdx)
}

// SetAttachStreamFnForTest swaps the WS PTY stream dial and returns a restore
// func, so a test can pin which attach source attachInstanceTab routes to.
func SetAttachStreamFnForTest(fn func(context.Context, string, string, string, int) (chan struct{}, error)) func() {
	prev := attachStreamFn
	attachStreamFn = fn
	return func() { attachStreamFn = prev }
}

// attachOverlayCallback runs the attach-overlay onDismiss lifecycle: emits
// the detach-trace markers, invokes attach, arms the attached flag for the
// duration of `<-ch`, then returns the tea.Cmd that re-asserts the terminal and
// emits repaintAfterDetachMsg{}. Returns nil when attach itself fails so the
// callback can be passed directly to showHelpScreen's onDismiss.
//
// Post-detach terminal handling is now uniform (#1592 Phase 2 PR7): every
// full-screen attach — a local WS PTY subscriber (apiclient.AttachStream) or a
// remote hook attach_cmd — is a RAW byte proxy that scribbles the pane program's
// alt-screen/mouse/scroll modes onto the real terminal and hands it back neutral
// (main screen, cursor visible, reporting off) on detach. Local attach used to be
// exempt only because a long-lived `tmux attach-session` client replayed clean
// restores across attach cycles; the clientless WS proxy has no such client, so
// the local path now hits the same #845 problem as remote. So both re-assert
// bubbletea's startup modes synchronously — while the Update goroutine is still
// blocked here, before the renderer can emit a frame — and precede the repaint
// with tea.ClearScreen to invalidate the stale diff cache (#845).
//
// The attach is also bracketed by the terminal handover (#2157): every
// full-screen attach reads the real stdin itself, so the Program must stop
// reading it for the attach's duration or the two readers split the user's
// keystrokes between them. See releaseTerminalToAttach.
//
// The defer on m.attached.Store(false) is load-bearing: it guarantees the
// flag clears even if `<-ch` is woken by an abnormal close or a panic
// further down the stack. Leaving the flag stuck at true would silently
// stall the metadata tick, preview refresh, and PR info fetcher until the
// next process restart — exactly the kind of regression #598 wants to
// avoid creating while fixing the original hang.
//
// Extracted so the attach call-sites (handleEnter sidebar, handleEnter
// terminal-tab) all funnel through one place — and so the pause-while-attached
// gating + the flag-clears-on-error path are testable without spinning up
// a real WS stream.
func (m *home) attachOverlayCallback(target sessionActionTarget, label, traceSuffix string, attach func() (chan struct{}, error)) tea.Cmd {
	detachTraceMark(label + "-onDismiss-entry" + traceSuffix)
	// Snapshot the terminal-write seam ONCE, here, before the attach starts —
	// same rule as the per-home seams captured below and as apiclient's attach
	// driver applies to its own package-level seams. remoteDetachResetWriter is a
	// mutable package global, and this callback runs for the whole length of an
	// attach; re-reading it on the detach path is a live read of shared mutable
	// state from a long-running call, which a test that swaps and restores it
	// races (the #1970/#2079 shared-global class). Every write below goes to this
	// local, so the writer this attach releases into is the one it reclaims into.
	resetWriter := remoteDetachResetWriter
	// Hand the terminal to the attach BEFORE it starts reading stdin (#2157).
	released := m.releaseTerminalToAttach(resetWriter)
	ch, err := attach()
	if err != nil {
		// Only when we released: an attach that never started scribbled nothing,
		// so there is no terminal state to take back and re-asserting modes would
		// pointlessly wipe the current frame. When we DID release, the reclaim is
		// mandatory — see reclaimTerminalFromAttach on the mouse.
		if released {
			m.reclaimTerminalFromAttach(resetWriter, true)
		}
		m.attachTransitioning = false
		log.ErrorLog.Printf("failed to attach (%s): %v", label+traceSuffix, err)
		return nil
	}

	// While we hold the shared tmux server full-screen, ask the daemon to pause
	// its per-instance capture-pane liveness poll for THIS instance so it stops
	// contending with the live attach (#1160, Fix A follow-up to #1157). A
	// heartbeat renews the daemon's lease-bounded pause until detach; the pause
	// is best-effort so a down/slow daemon never disturbs the attach.
	//
	// Capture the seams off the home HERE, on the event loop, before
	// any goroutine spawns: the seams are per-home fields (not shared globals)
	// precisely so the goroutines never read home state a sibling test could
	// reassign under `go test -parallel -race` (the #964 / #960-PR4 race class).
	pause := m.pauseStatusPoll
	resume := m.resumeStatusPoll
	pauseDone := make(chan struct{})
	heartbeatExited := make(chan struct{})
	go runStatusPollPauseHeartbeat(pause, target.pauseStatusPollRequest(), pauseDone, heartbeatExited)

	m.attached.Store(true)
	defer m.attached.Store(false)
	// <-ch blocks for as long as the user is attached. Mark the boundary so
	// post-detach elapsed times in the trace are measured from when the user
	// actually returned to the UI, not from when the attach started.
	detachTraceMark(label + "-blocking-on-<-ch" + traceSuffix)
	<-ch
	// Stop the heartbeat and resume the daemon's poll immediately on this clean
	// detach — don't wait out the lease. The resume must WIN over any in-flight
	// pause: both were fire-and-forget, so a naive resume could land on the wire
	// before the heartbeat's last pause() and leave the instance paused until the
	// lease expires (Greptile P). runStatusPollResume waits for heartbeatExited
	// (the heartbeat closes it after its final synchronous pause() returns — and
	// callDaemon blocks until the daemon has applied that pause) so the resume
	// strictly follows it. This runs on its OWN goroutine so the detach hot path
	// never blocks on the wait or the RPC — attach/detach responsiveness is the
	// whole point of #1160.
	close(pauseDone)
	go runStatusPollResume(resume, target.resumeStatusPollRequest(), heartbeatExited)
	detachStart := time.Now()
	detachTraceMark(label + "-<-ch-unblocked" + traceSuffix)
	m.attachTransitioning = false
	m.state = stateDefault
	// Arm the slow-detach watchdog: if the post-detach paint
	// (panesRefreshedMsg) does not arrive within slowDetachThreshold, a
	// goroutine dump is appended to detach-slow.log so we can see which
	// goroutine is blocked.
	beginDetachWatchdog(label + traceSuffix)
	repaintCmd := func() tea.Msg {
		detachTrace(detachStart, label+"-repaintAfterDetachMsg-emitted")
		return repaintAfterDetachMsg{}
	}
	// The attach driver (WS or hook) wrote its neutral restore before closing ch,
	// so the reclaim lands strictly after it. The Update goroutine is still
	// blocked in this callback, so no renderer write can interleave (#845).
	//
	// ClearScreen then invalidates the renderer's stale diff cache before the
	// repaint flow runs; then the usual repaintAfterDetachMsg path, watchdog
	// semantics (#683) included.
	m.reclaimTerminalFromAttach(resetWriter, released)
	return tea.Sequence(tea.ClearScreen, repaintCmd)
}

// releaseTerminalToAttach hands the real terminal to the full-screen attach that
// is about to start, and reports whether it did (so the caller pairs it with
// exactly one reclaimTerminalFromAttach).
//
// This is the #2157 fix. A full-screen attach is a RAW byte proxy: it reads the
// real stdin itself (apiclient.driveAttachStream's stdin pump) and forwards every
// byte to the pane. But Bubble Tea's input reader is blocked on that SAME file
// descriptor for the whole attach — the Update goroutine is parked in this
// callback, yet the read loop it started at boot is a separate goroutine and
// nothing here ever stopped it. Two readers on one tty do not each get a copy:
// they SPLIT the stream, and every byte the TUI's reader wins is gone from the
// pane's input.
//
// A single keystroke rarely loses that race and would be invisible if it did (the
// stolen key is just queued for a blocked Update). A PASTE loses it constantly:
// the pump forwards its 32-byte read over the websocket while the TUI's 256-byte
// read takes everything else the terminal has buffered. That is the reported
// symptom exactly — a 128-character line landing as its first 32 bytes, whole
// interior chunks missing, worst on the first paste after attaching — and it is
// silent, so a partially-pasted quoted command reads to the user as a typo.
//
// ReleaseTerminal is Bubble Tea's own answer: it cancels the input reader, stops
// the renderer, and restores the terminal — the same handover tea.Exec performs
// around a child process that takes the terminal, and the reason the config-agent
// attach (which goes through tea.ExecProcess) never had this bug. After it, the
// attach's pump is the only reader of stdin, which is the invariant a raw proxy
// needs.
//
// A failure to release is logged and the attach proceeds: a live attach with a
// racy paste is a far better outcome than refusing to attach at all. The false
// return then keeps the terminal from being "restored" from a state it was never
// released into.
//
// out is the terminal, captured by the caller before the attach starts, never
// re-read from the package seam mid-attach.
func (m *home) releaseTerminalToAttach(out io.Writer) bool {
	if m.releaseTerminal == nil {
		return false // no Program wired (tests): nothing owns the terminal
	}
	if err := m.releaseTerminal(); err != nil {
		log.ErrorLog.Printf("failed to release the terminal to the attach: %v", err)
		return false
	}
	_, _ = io.WriteString(out, attachAltScreenEnter)
	return true
}

// reclaimTerminalFromAttach takes the terminal back for the TUI once the attach
// is over — on a clean detach and on an attach that failed after the release.
// released says whether the terminal was actually handed to Bubble Tea's
// ReleaseTerminal; out is the terminal, captured by the caller.
//
// This is the ONLY place the mode re-assert is written, and that is deliberate.
// Both halves of "take the terminal back" have to happen together on EVERY path:
//
//   - RestoreTerminal restarts the input reader the attach was given exclusive
//     use of, resumes the renderer, and re-enters the alt screen with the
//     renderer's bookkeeping and the terminal agreeing again;
//   - remoteDetachTerminalReassert covers what it does not — above all MOUSE
//     reporting, which the release disabled and which bubbletea only ever writes
//     once, at startup, from the WithMouseCellMotion option.
//
// Splitting them is how a failed attach came to leave a TUI whose mouse was dead
// until the process restarted: attach errors are ordinary (a daemon down or
// restarting, an unresolved socket, a failed WS dial), that path returned early,
// and it skipped a re-assert that was written further down. One function, both
// halves, and the error path cannot drift from the detach path again.
//
// The re-assert is written even when the release did not happen (a headless
// caller, or a release that errored): the attach itself is a raw byte proxy that
// scribbled the pane program's modes onto the terminal regardless, which is the
// #845 reason this constant exists at all. It is also written even if
// RestoreTerminal fails, since a mouse-dead terminal helps nobody.
func (m *home) reclaimTerminalFromAttach(out io.Writer, released bool) {
	if released && m.restoreTerminal != nil {
		if err := m.restoreTerminal(); err != nil {
			// The terminal is now in whatever state the attach left it and the input
			// reader may be down. There is no second lever to pull — surface it in the
			// log; the mode re-assert below and the repaint the caller schedules are
			// the best remaining recovery.
			log.ErrorLog.Printf("failed to reclaim the terminal after attach: %v", err)
		}
	}
	_, _ = io.WriteString(out, remoteDetachTerminalReassert)
}

// statusPollRenewInterval is how often an attached TUI re-sends PauseStatusPoll
// to renew the daemon's lease-bounded pause (#1160). It MUST stay below the
// daemon's statusPollLease (3s) so a live attach never lets the lease lapse and
// let the daemon's capture-pane poll resume mid-attach; 1s against a 3s lease
// leaves two missed renews of slack for a hiccuping daemon.
const statusPollRenewInterval = 1 * time.Second

// runStatusPollPauseHeartbeat pauses the daemon's capture-pane poll for the
// attached instance and renews that lease every statusPollRenewInterval until
// done closes (detach), then closes exited. pause + request are captured off the
// event loop by the caller so this goroutine never touches shared home state
// (#964 race class). Every RPC is best-effort — a down/slow daemon logs and
// continues, never disturbing the attach (worst case the daemon keeps polling,
// the pre-#1160 behavior). Because pause() is called SYNCHRONOUSLY in the loop,
// once this goroutine returns no pause RPC is in-flight or pending, so exited
// firing is the signal a following resume can safely win the wire.
func runStatusPollPauseHeartbeat(pause func(daemon.PauseStatusPollRequest) error, request daemon.PauseStatusPollRequest, done <-chan struct{}, exited chan<- struct{}) {
	defer close(exited)
	send := func() {
		if err := pause(request); err != nil {
			log.ErrorLog.Printf("failed to pause daemon status poll for %q: %v", request.Title, err)
		}
	}
	send() // pause immediately on attach, before the first renew tick
	ticker := time.NewTicker(statusPollRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			send()
		}
	}
}

// runStatusPollResume resumes the daemon's poll on a clean detach so the poll
// resumes immediately rather than after the lease expires (#1160). It waits for
// heartbeatExited FIRST so the resume RPC strictly follows the heartbeat's final
// pause() — guaranteeing resume wins over any in-flight pause instead of racing
// it (Greptile P). resume + request are captured off the event loop by the
// caller (#964 race class). Best-effort; the caller runs this on its own
// goroutine so the detach hot path never blocks on the wait or the RPC.
func runStatusPollResume(resume func(daemon.ResumeStatusPollRequest) error, request daemon.ResumeStatusPollRequest, heartbeatExited <-chan struct{}) {
	<-heartbeatExited
	if err := resume(request); err != nil {
		log.ErrorLog.Printf("failed to resume daemon status poll for %q: %v", request.Title, err)
	}
}
