package session

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Capture recovery for the PTY broker: bringing a session's byte stream back
// after the thing producing it went away.
//
// Two causes, one body. A LOCAL session's tmux pane can be re-spawned under a
// live broker (#1682), and a REMOTE session's data-plane WebSocket can be
// dropped by a proxy while the sandbox stays healthy over REST (#2438/#2450).
// Both leave the broker holding a capture that will never produce another byte,
// and both want the same repair: release the dead capture, discard what the dead
// pane left in the ring, dial a fresh one, and re-seed every attached subscriber
// with a repaint so its emulator is not silently desynced.
//
// They differ only in who notices. The local case is driven externally by the
// respawn hook (resetCapture, from localAgentServer); the remote case has no
// such hook, so it is driven by the readLoop's own exit through redialLoop.
// Split out of ptybroker.go when that second driver pushed the file past the
// 1000-line limit (#1145).

// Bounds on the self-driven re-dial after an upstream death (#2450).
//
// The recovery is driven by the readLoop's own exit, which makes it a feedback
// loop: an endpoint that accepts a socket and immediately drops it produces
// death -> re-dial -> death as fast as the loop can turn. So each consecutive
// attempt waits longer, doubling from redialInitialBackoff up to
// redialMaxBackoff. The web client already backs off this way
// (web/src/terminal.ts); this is the daemon-side equivalent, and without it the
// daemon would hammer a down sandbox on the user's behalf with nobody watching.
//
// The ladder resets once a capture has survived redialHealthySpan, so an
// endpoint that drops once an hour is treated as a fresh incident rather than
// inheriting the previous one's delay.
//
// vars, not consts, so tests can compress them — see setRedialTimingForTest.
var (
	redialInitialBackoff = 500 * time.Millisecond
	redialMaxBackoff     = 30 * time.Second
	redialHealthySpan    = 60 * time.Second
)

// setRedialTimingForTest compresses the re-dial ladder so a test can exercise
// several rungs without sleeping through the real one. Returns a restore func,
// matching the SetTrustPromptTimingForTest seam in task. Test-only.
func setRedialTimingForTest(unit time.Duration) func() {
	prevInitial, prevMax, prevHealthy := redialInitialBackoff, redialMaxBackoff, redialHealthySpan
	redialInitialBackoff, redialMaxBackoff, redialHealthySpan = unit, 4*unit, 50*unit
	return func() {
		redialInitialBackoff, redialMaxBackoff, redialHealthySpan = prevInitial, prevMax, prevHealthy
	}
}

// resetCapture recovers the broker onto a re-spawned tmux pane WITHOUT closing it or
// dropping its subscribers (#1682). On session recovery the previous tmux — and with
// it the `pipe-pane` writer feeding this broker's FIFO — died, but the broker's
// readLoop is parked on the O_RDWR FIFO (which never sees EOF) with `capturing` still
// true, so the cached broker keeps short-circuiting ensureCaptureStarted and no bytes
// ever flow again. resetCapture, holding captureMu across the whole transition so no
// concurrent bring-up/teardown can interleave (#1661):
//
//  1. Stops the stale capture — unblocking and JOINING the parked readLoop (no
//     goroutine leak) — and clears the capturing latch.
//  2. Discards the dead pane's still-buffered ring bytes, so a subscriber that was
//     lagging at recovery cannot be handed them after the repaint (#1840).
//  3. If subscribers are STILL attached (a web/TUI client that stayed connected
//     across the respawn — the common case), restarts the capture against the fresh
//     pane and re-seeds each subscriber with a repaint of the recovered screen, so it
//     resumes output on its OWN rather than hanging until some unrelated later
//     Subscribe happens to bring the capture back up (the #1682 residual T-Rex hit).
//     With no subscribers left, the lazy lifecycle is simply re-armed for the next
//     Subscribe.
//
// Recovery is ATOMIC as each attached subscriber sees it (#1975): the barrier armed
// below holds every subscriber's content stream from the moment recovery starts until
// the repaint is installed, so the repaint is the FIRST thing a subscriber renders
// after the respawn. Without it, steps 1-3 each leak a frame the repaint then wipes —
// the dead pane's still-buffered bytes before the discard, and (the reported case) the
// re-spawned pane's live bytes during step 3's snapshot, which runs without b.mu while
// the freshly-started readLoop is already feeding and waking subscribers.
//
// The subscriber count is re-read AFTER the stop (still under captureMu) so a
// subscriber that left during the blocking teardown is not resurrected onto a capture
// nobody wants. A no-op when the broker is already closed.
func (b *ptyBroker) resetCapture() { b.recoverCapture(false) }

// recoverDeadUpstream is resetCapture's recovery, GUARDED: it runs only when
// there is no healthy capture installed. That guard is what lets the readLoop's
// own exit drive recovery (#2450).
//
// resetCapture cannot be called from there directly, for two independent
// reasons. It unconditionally stops whatever capture is live, so a scheduled
// recovery that raced a healthy re-dial (a subscribe got there first) would tear
// down the good capture and blip every attached client. And it is asynchronous
// by necessity: a readLoop cannot run the teardown inline, because stopCapture
// ends in `<-done` — a wait on the very goroutine that would be calling it.
//
// Routing recovery through the SAME body as the local-respawn case is the point,
// not an economy. A recovery that merely re-dialled would splice a silent gap
// into an already-attached subscriber's stream: remoteClientlessChannel dials
// from the LIVE TAIL, so every byte produced during the outage is gone, and the
// ring seq stays contiguous across the hole so nothing tells the client anything
// was lost. This body arms the #1975 barrier, performs the #1840 discard, and
// re-seeds a repaint of the recovered screen — which is exactly what that
// subscriber needs and what #2447's subscribe-only path did not do.
func (b *ptyBroker) recoverDeadUpstream() { b.recoverCapture(true) }

func (b *ptyBroker) recoverCapture(onlyIfNoHealthyCapture bool) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	// The guard, mirroring ensureCaptureStartedLocked's short-circuit: `capturing
	// && !captureEnded` is the one expression in this file that means "a healthy
	// capture is installed". A scheduled recovery whose death was already
	// reconciled by a racing subscribe must do NOTHING rather than restart a
	// capture that is working.
	if onlyIfNoHealthyCapture && b.capturing && !b.captureEnded {
		b.mu.Unlock()
		return
	}
	// Arm the barrier FIRST — before the teardown, the discard, and the restart — so no
	// subscriber can emit a frame at any point inside the recovery. Armed subscribers
	// are captured by identity, not re-read from b.subs at release time, so one that
	// detaches mid-recovery is still released rather than left parked (#1975).
	armed := b.armRecoveryRepaintLocked()
	var stop func()
	if b.capturing {
		stop = b.stopCapture
		b.capturing = false
		b.stopCapture = nil
	}
	b.mu.Unlock()

	// The barrier MUST be lifted on EVERY path out — a failed restart, a Snapshot
	// error, nobody left attached — or an armed subscriber parks in NextEvent forever
	// waiting for a repaint that never comes. rp is read when the deferred call runs,
	// so this single exit point both installs whatever repaint the recovery managed to
	// build and lifts the barrier, atomically under b.mu.
	var rp *repaintSnapshot
	defer func() { b.releaseRecoveryRepaint(armed, rp) }()

	if stop != nil {
		stop()
	}

	// Discard the dead pane's buffered output (#1840). This is the ONLY point where
	// that is race-free: the old readLoop has been joined by the stop above and the
	// fresh one is not started until below, so the ring provably holds dead-pane bytes
	// and nothing else, and no feed can interleave. Dropping the bytes while KEEPING
	// the seq monotonic (base jumps to head over an emptied ring) means a lagging
	// subscriber — one whose WS write blocked while the dying pane kept producing —
	// hits the existing `cursor < base` eviction clamp in NextEvent and fast-forwards
	// to the live tail. Without this it would take the recovery repaint and THEN the
	// pre-recovery bytes, overwriting the recovered screen with the dead pane's
	// content. Nothing needs to touch subscriber cursors directly; the clamp is the
	// same mechanism a ring overflow already uses.
	//
	// Unconditional, including when nobody is attached: a later Subscribe(since) would
	// otherwise replay the dead pane's tail into a fresh client. That no-subscriber path
	// is why subscribe() repaints a reconnect whose `since` lands below base — the
	// discard leaves it a cursor that can never be replayed, and with nobody attached
	// there is no re-seed here to cover it.
	b.mu.Lock()
	b.base = b.headLocked()
	b.buf = nil
	// Re-read the subscriber count AFTER the teardown drained: nobody left → just the
	// lazy re-arm, the next Subscribe restarts a fresh capture.
	resume := !b.closed && len(b.subs) != 0
	b.mu.Unlock()
	if !resume {
		return
	}

	// A subscriber stayed attached across the respawn. Restart the capture against the
	// re-spawned pane and re-seed every current subscriber so it repaints the recovered
	// screen and resumes live output without a new Subscribe. The restarted capture's
	// readLoop begins feeding and waking subscribers IMMEDIATELY — the barrier is what
	// keeps those bytes behind the repaint built below (#1975).
	if err := b.ensureCaptureStartedLocked(); err != nil {
		log.WarningLog.Printf("pty broker: restart capture after recovery: %v", err)
		return
	}
	rp = b.recoveryRepaint()
}

// armRecoveryRepaintLocked holds every currently-registered subscriber's content
// stream (PTYCursor/PTYData) until the recovery repaint is installed, and returns the
// subscribers it armed. Caller holds b.mu.
//
// The returned slice — NOT a later re-read of b.subs — is what releaseRecoveryRepaint
// walks: a subscriber that detaches mid-recovery is gone from the map but may still be
// parked in NextEvent, and it must be released too.
func (b *ptyBroker) armRecoveryRepaintLocked() []*ptySub {
	armed := make([]*ptySub, 0, len(b.subs))
	for _, s := range b.subs {
		s.repaintArmed = true
		armed = append(armed, s)
	}
	return armed
}

// recoveryRepaint captures the recovered pane's screen and builds the repaint bytes
// the re-seed installs, so a client that stayed attached across a tmux respawn
// repaints the current screen and resumes live output on its own (#1682). It mirrors
// subscribe()'s initial-repaint injection but builds ONE snapshot for all subscribers.
// The Snapshot exec runs without b.mu held (matching subscribe) — which is precisely
// the window the barrier exists to cover. Best-effort: a Snapshot error or an empty
// screen yields nil, and the release degrades to just lifting the barrier and waking
// the subscribers — the restarted capture's next live byte still reaches them — rather
// than failing the recovery. Caller holds captureMu.
func (b *ptyBroker) recoveryRepaint() *repaintSnapshot {
	snap, err := b.ch.Snapshot()
	if err != nil {
		log.WarningLog.Printf("pty broker: snapshot for recovery re-seed: %v", err)
		return nil
	}
	if !snapshotHasRepaintState(snap) {
		return nil
	}
	rp := buildRepaintSnapshot(snap)
	rp.provenance = PTYRepaintRecovery
	return &rp
}

// redialDelay is the backoff for the nth consecutive re-dial: initial, doubling,
// capped. Attempt 0 still waits — an upstream that just dropped is rarely ready
// the same millisecond, and the wait is also what keeps a racing subscribe (which
// recovers synchronously and for free) from colliding with this path.
func redialDelay(attempt int) time.Duration {
	d := redialInitialBackoff
	for i := 0; i < attempt && d < redialMaxBackoff; i++ {
		d *= 2
	}
	if d > redialMaxBackoff {
		d = redialMaxBackoff
	}
	return d
}

// redialLoop re-establishes a capture whose upstream died, without waiting for a
// new subscribe (#2450).
//
// #2447 made a dead capture recoverable but reachable only from subscribe(), and
// with one client attached over a healthy daemon-browser WebSocket nothing ever
// re-subscribes — the daemon pings, the browser pongs, and the pane stays dead
// until the user refreshes. This is the missing driver.
//
// It retries while there is no healthy capture and somebody is still attached,
// climbing redialDelay's ladder. Two distinct storms are bounded by the same
// counter, because to a down endpoint they look the same from here:
//
//   - the dial itself keeps failing (sandbox unreachable) — this loop retries;
//   - the dial succeeds and the socket drops immediately (a flapping proxy) —
//     this loop exits, the fresh readLoop dies, and its hand-off resumes the
//     ladder WITHOUT resetting it, because the capture did not survive
//     redialHealthySpan.
//
// It gives up on: the broker closing, the last subscriber leaving (the lazy
// lifecycle owns that case — the next subscribe brings the capture up), or a
// healthy capture appearing (a subscribe beat us to it).
func (b *ptyBroker) redialLoop() {
	defer func() {
		b.mu.Lock()
		b.redialing = false
		b.mu.Unlock()
	}()

	for {
		b.mu.Lock()
		healthy := b.capturing && !b.captureEnded
		if b.closed || healthy || len(b.subs) == 0 {
			b.mu.Unlock()
			return
		}
		delay := redialDelay(b.redialAttempts)
		b.redialAttempts++
		b.mu.Unlock()

		select {
		case <-b.closedCh:
			return
		case <-time.After(delay):
		}

		// Guarded: does nothing if a subscribe recovered the capture while we slept.
		b.recoverDeadUpstream()
	}
}

// releaseRecoveryRepaint installs rp on every armed subscriber and lifts the barrier —
// both under ONE b.mu hold, so a subscriber can never observe the lifted barrier
// without also seeing the repaint that was owed to it. Waking is what makes a parked
// subscriber re-read that state.
func (b *ptyBroker) releaseRecoveryRepaint(armed []*ptySub, rp *repaintSnapshot) {
	b.mu.Lock()
	for _, s := range armed {
		if rp != nil && len(rp.data) > 0 {
			s.pendingRepaint = rp
		}
		s.repaintArmed = false
	}
	b.wakeAllLocked()
	b.mu.Unlock()
	// wakeAllLocked only rings subscribers still in b.subs. An armed subscriber that
	// detached mid-recovery is not in the map but can still be parked on the barrier,
	// so ring its doorbell directly; wake() is a coalescing no-op when already signaled.
	for _, s := range armed {
		s.wake()
	}
}
