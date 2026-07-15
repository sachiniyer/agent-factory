package app

import (
	"context"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
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
const remoteDetachTerminalReassert = "" +
	"\x1b[?1049h" + // re-enter the alt screen (terminal clears it)
	"\x1b[?25l" + // bubbletea hid the cursor at startup; re-hide it
	"\x1b[?1002h\x1b[?1006h" + // WithMouseCellMotion + SGR encoding
	"\x1b[?2004h" // bracketed paste (bubbletea default-on)

// remoteDetachResetWriter is where remoteDetachTerminalReassert is written —
// the real terminal in production, swappable so tests can capture it.
var remoteDetachResetWriter io.Writer = os.Stdout

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
func (m *home) attachOverlayCallback(title, label, traceSuffix string, attach func() (chan struct{}, error)) tea.Cmd {
	detachTraceMark(label + "-onDismiss-entry" + traceSuffix)
	ch, err := attach()
	if err != nil {
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
	// Capture the seams + repoID off the home HERE, on the event loop, before
	// any goroutine spawns: the seams are per-home fields (not shared globals)
	// precisely so the goroutines never read home state a sibling test could
	// reassign under `go test -parallel -race` (the #964 / #960-PR4 race class).
	pause := m.pauseStatusPoll
	resume := m.resumeStatusPoll
	repoID := m.repoID
	pauseDone := make(chan struct{})
	heartbeatExited := make(chan struct{})
	go runStatusPollPauseHeartbeat(pause, title, repoID, pauseDone, heartbeatExited)

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
	go runStatusPollResume(resume, title, repoID, heartbeatExited)
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
	// so this lands strictly after it. The Update goroutine is still blocked in
	// this callback, so no renderer write can interleave (#845). ClearScreen first
	// so the renderer's stale diff cache is invalidated before the repaint flow
	// runs; then the usual repaintAfterDetachMsg path, watchdog semantics (#683)
	// included.
	_, _ = io.WriteString(remoteDetachResetWriter, remoteDetachTerminalReassert)
	return tea.Sequence(tea.ClearScreen, repaintCmd)
}

// statusPollRenewInterval is how often an attached TUI re-sends PauseStatusPoll
// to renew the daemon's lease-bounded pause (#1160). It MUST stay below the
// daemon's statusPollLease (3s) so a live attach never lets the lease lapse and
// let the daemon's capture-pane poll resume mid-attach; 1s against a 3s lease
// leaves two missed renews of slack for a hiccuping daemon.
const statusPollRenewInterval = 1 * time.Second

// runStatusPollPauseHeartbeat pauses the daemon's capture-pane poll for the
// attached instance and renews that lease every statusPollRenewInterval until
// done closes (detach), then closes exited. pause + repoID are captured off the
// event loop by the caller so this goroutine never touches shared home state
// (#964 race class). Every RPC is best-effort — a down/slow daemon logs and
// continues, never disturbing the attach (worst case the daemon keeps polling,
// the pre-#1160 behavior). Because pause() is called SYNCHRONOUSLY in the loop,
// once this goroutine returns no pause RPC is in-flight or pending, so exited
// firing is the signal a following resume can safely win the wire.
func runStatusPollPauseHeartbeat(pause func(title, repoID string) error, title, repoID string, done <-chan struct{}, exited chan<- struct{}) {
	defer close(exited)
	send := func() {
		if err := pause(title, repoID); err != nil {
			log.ErrorLog.Printf("failed to pause daemon status poll for %q: %v", title, err)
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
// it (Greptile P). resume + repoID are captured off the event loop by the
// caller (#964 race class). Best-effort; the caller runs this on its own
// goroutine so the detach hot path never blocks on the wait or the RPC.
func runStatusPollResume(resume func(title, repoID string) error, title, repoID string, heartbeatExited <-chan struct{}) {
	<-heartbeatExited
	if err := resume(title, repoID); err != nil {
		log.ErrorLog.Printf("failed to resume daemon status poll for %q: %v", title, err)
	}
}
