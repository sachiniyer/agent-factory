package session

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/terminal"
)

// The WS PTY broker's server-side data plane (#1592 Phase 2 PR5). A ptyBroker
// owns one session's raw PTY output and fans it to N subscribers:
//
//   - A bounded ring buffer holds the recent output with a monotonic Seq cursor,
//     so a (re)connecting subscriber can Subscribe(since) and replay the tail it
//     missed rather than losing the gap.
//   - Bytes arrive from a CLIENTLESS tmux channel (pipe-pane capture, see
//     clientlessChannel) — producing the stream no longer requires a
//     `tmux attach-session` render client, which is the structural move that lets
//     PR6 delete that render client entirely.
//   - INPUT and RESIZE are accepted from ANY subscriber (multi-writer, no lease):
//     Input is a clientless send-keys, Resize is a clientless resize-window with
//     last-resize-wins plus an authoritative echo broadcast to every subscriber.
//   - A dead subscriber (its WS keepalive lapsed, or its consumer went away) is
//     dropped WITHOUT touching the PTY/session — the reliability win over a shared
//     tmux client, whose death used to take output down with it (§6).
//
// The broker is lazy: the clientless capture starts on the first Subscribe and
// stops when the last subscriber leaves, so a session nobody is watching runs no
// extra tmux pipe or reader goroutine.

// ptyRingMaxBytes bounds one session's output ring buffer. A subscriber that
// falls further behind than this loses the intervening bytes (its cursor is
// fast-forwarded to the oldest retained byte) — acceptable for a terminal, whose
// next full repaint re-synchronises the emulator, and the keepalive drops a
// truly-dead subscriber rather than letting it grow the buffer unbounded. var,
// not const, so tests can shrink it.
var ptyRingMaxBytes = 256 * 1024

// clientlessChannel is the local runtime's clientless tmux binding (#1592 PR5):
// output capture and input/size WITHOUT a tmux client. The real implementation
// (tmuxClientlessChannel) drives `tmux pipe-pane`/`send-keys`/`resize-window`;
// tests substitute an in-memory fake so the ring/fan-out logic is exercised with
// no real tmux server.
type clientlessChannel interface {
	// StartCapture enables output capture and returns a reader over the raw PTY
	// byte stream. The broker reads it on a goroutine until StopCapture.
	StartCapture() (io.ReadCloser, error)
	// StopCapture disables capture and releases its resources (closing the reader
	// StartCapture returned).
	StopCapture() error
	// SendRaw writes raw input bytes to the pane (clientless send-keys).
	SendRaw(b []byte) error
	// Resize sets the pane/window size (clientless resize-window). Last-resize-wins
	// is enforced by the broker; this just applies the winning size.
	Resize(rows, cols uint16) error
	// Snapshot returns the pane's current visible screen (GRID-form, one line per pane
	// row — see PaneSnapshot) AND the pane cursor position — the repaint the broker
	// injects on subscribe, since pipe-pane never delivers tmux's screen redraw (#1592
	// Phase 2 PR6). The cursor position lets the
	// repaint leave the emulator cursor where the pane program's cursor actually is;
	// without it the snapshot's trailing blank lines strand the emulator cursor at the
	// bottom, so the pane's next relative-positioned redraw (a shell's SIGWINCH prompt
	// redraw) lands there and orphans a stale copy at the top (the duplicated-prompt
	// artifact). HasCursor is false when the channel cannot report a position (the
	// remote REST-preview snapshot), in which case the repaint omits cursor restore.
	Snapshot() (PaneSnapshot, error)
}

// PaneSnapshot is a fresh-subscriber repaint source: the pane's current visible
// screen (with escapes) plus the pane cursor position. CursorRow/CursorCol are
// 0-based; they are meaningful only when HasCursor is true.
//
// Screen MUST be GRID-form — one line per PHYSICAL pane row, NOT -J-joined logical
// lines. buildRepaint places each line at its own absolute row, so line index i is
// taken to be pane row i; feeding it -J-joined lines (where one logical line spans
// several pane rows) would mis-map the rows. The local tmux channel captures grid
// form (CaptureVisiblePaneGrid). The remote channel's REST preview is -J-joined and
// carries no cursor (HasCursor=false) — a known screen-only best-effort limitation,
// see remoteClientlessChannel.Snapshot.
type PaneSnapshot struct {
	Screen    []byte
	CursorRow int
	CursorCol int
	HasCursor bool
	// Modes are the ownership-affecting terminal modes that were already active
	// before this subscriber existed. HasModes distinguishes a truthful all-off
	// primary-screen snapshot from a source that cannot report modes.
	Modes    terminal.Modes
	HasModes bool
}

// repaintSnapshot is one atomic broker event: the grid repaint plus the terminal
// modes captured with it. The daemon emits the modes immediately before Data,
// and Data also restores them as DEC sequences for terminal-only clients.
type repaintSnapshot struct {
	data     []byte
	modes    terminal.Modes
	hasModes bool
}

// ptyBroker is the per-session data plane. Guarded by mu; the ring buffer, the
// subscriber set, and the authoritative size all live under it.
type ptyBroker struct {
	ch       clientlessChannel
	maxBytes int

	// captureMu serializes the clientless capture's bring-up and tear-down so a
	// teardown can NEVER clobber a concurrently-(re)started capture (#1661). The
	// clientless channel drives a SINGLE pane pipe (`tmux pipe-pane`), so an
	// out-of-order StopCapture would `pipe-pane`-disable whatever pipe is current —
	// including a fresh one a new subscriber just enabled — leaving the new
	// subscriber with a live ring/readLoop but no bytes ever arriving (a stale pane
	// after a full-screen attach+detach cycle re-binds the embedded pane). Every
	// start/stop runs through a captureMu-serialized reconcile that re-reads the
	// live subscriber count, so the last operation to run always converges the
	// pipe to the true desired state. Lock ordering: captureMu THEN mu, never the
	// reverse.
	captureMu sync.Mutex

	mu   sync.Mutex
	buf  []byte // ring: recent output bytes, buf[0] is at seq `base`
	base Seq    // seq of buf[0]; head == base + len(buf)

	subs      map[uint64]*ptySub
	nextSubID uint64

	rows, cols uint16
	hasSize    bool
	resizeGen  uint64 // bumped on each resize; a subscriber echoes when it lags

	capturing   bool
	stopCapture func() // tears down the capture goroutine + clientless channel
	closed      bool
	// tabClosed records that this broker was shut down because ITS TAB was closed
	// (#2136) rather than because the whole session was torn down. It only selects
	// which end-of-stream error NextEvent reports (ErrTabClosed vs bare io.EOF);
	// the teardown itself is identical. Set under mu in the same section that
	// latches closed, so a subscriber can never observe one without the other.
	tabClosed bool
}

func newPTYBroker(ch clientlessChannel) *ptyBroker {
	return &ptyBroker{
		ch:       ch,
		maxBytes: ptyRingMaxBytes,
		subs:     make(map[uint64]*ptySub),
	}
}

// head returns the seq just past the last buffered byte. Caller holds mu.
func (b *ptyBroker) headLocked() Seq { return b.base + Seq(len(b.buf)) }

// subscribe registers a subscriber whose cursor starts at `since` (0 = the live
// tail). It starts the clientless capture on the first subscriber. Read-write:
// the returned subscription is also the identity Input/Resize fan out to.
func (b *ptyBroker) subscribe(since Seq) (*ptySub, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("pty broker closed")
	}
	head := b.headLocked()
	// A repaint is owed when a ring replay CANNOT rebuild this client's screen: a fresh
	// subscriber (since == 0 — pipe-pane carries no history), or a reconnect asking for
	// bytes BELOW the retained window, which are provably gone (#1845 follow-up). The
	// second case is the post-recovery reconnect: the recovery discard drops the dead
	// pane's ring and advances base past the cursor the client left on, so the replay it
	// asked for cannot be served — without a repaint its pre-death screen stays frozen
	// until some unrelated later output arrives. A ring eviction reaches this the same
	// way, and wants the same repaint.
	//
	// Deliberately NOT keyed on "cursor was clamped at all": a `since` PAST head also
	// clamps (down, to the live tail), but that client is not missing any byte the
	// broker holds, and repainting it would add flicker to the documented
	// clamp-to-tail path. Only a cursor below base means bytes are missing.
	needRepaint := since == 0 || since < b.base
	var cursor Seq
	if needRepaint {
		// Start at the live tail, NOT at base. The repaint below reconstructs the WHOLE
		// current screen, so every retained ring byte [base, head) is ALREADY baked into
		// it. Starting the replay at base would make NextEvent send the repaint and THEN
		// replay those same bytes on top of it — duplicating output: a command/prompt
		// appended twice, up to the entire retained ring on an eviction or post-recovery
		// reconnect (#1872 P1). The tail cursor is exactly what a since == 0 fresh
		// subscriber already gets; the two paths now share it. Bytes fed between this head
		// read and the snapshot below land in both the snapshot and the replayed tail —
		// the same tiny, bounded double-render a fresh subscribe already accepts — never
		// dropped. The client learns this cursor from the handshake seq / OpHello, so its
		// ?since stays consistent.
		cursor = head
	} else {
		// A seamless reconnect: since is inside the retained window, so the client's
		// screen is current up to `since` and replaying [since, head) brings it forward
		// with no repaint flicker. A since past head clamps down to the live tail.
		cursor = since
		if cursor > head {
			cursor = head
		}
	}
	b.nextSubID++
	sub := &ptySub{
		br:         b,
		id:         b.nextSubID,
		cursor:     cursor,
		resizeSeen: b.resizeGen,
		notify:     make(chan struct{}, 1),
	}
	b.subs[sub.id] = sub
	b.mu.Unlock()

	// Bring up the clientless capture through the serialized reconcile — NOT inline
	// under b.mu — so it can never interleave with a teardown that races it (#1661).
	// Register-first (above) means the reconcile sees this subscriber, so a
	// concurrent last-leaver teardown re-checking the count won't tear down the
	// capture this subscriber needs.
	if err := b.ensureCaptureStarted(); err != nil {
		b.remove(sub.id)
		return nil, err
	}

	// Paint the current screen for a subscriber the replay stream can't reconstruct
	// (see needRepaint): a fresh one (pipe-pane only streams FUTURE output, so without
	// this a just-opened pane renders blank until the next byte) or a reconnect whose
	// cursor was clamped over discarded bytes. A reconnect that lands inside the
	// retained window resumes via ?since replay instead, so it is left seamless.
	// Captured AFTER registration (register-first) so no output can slip in between
	// the snapshot and the cursor: bytes in that tiny window are simply in both the
	// snapshot and the replayed tail (a harmless double-render), never dropped.
	if needRepaint {
		if snap, err := b.ch.Snapshot(); err == nil && snapshotHasRepaintState(snap) {
			rp := buildRepaintSnapshot(snap)
			b.mu.Lock()
			sub.pendingRepaint = &rp
			b.mu.Unlock()
			sub.wake()
		}
	}
	return sub, nil
}

// buildRepaint turns a GRID-form pane snapshot (see PaneSnapshot) into bytes that
// reconstruct the screen when written to the emulator: clear the screen, then place
// each captured row at its OWN absolute line — CSI row;1 H, erase-to-EOL, then the
// row's content — and finally restore the cursor to the pane's real position (1-based
// CSI H) when the snapshot carries one.
//
// The explicit per-row positioning is the #1688 fix. The old form wrote the whole
// screen as one CRLF-joined blob and let the emulator RE-WRAP it by the emulator's
// OWN width, then issued an absolute cursor move to the pane's cursor_y. That is only
// correct when the client width == pane width: under a mismatch (multi-writer
// last-resize-wins — e.g. a browser subscriber opening at a different size than the
// pane) the re-wrap shifts the rows, so the absolute cursor row named the wrong line
// and Claude's relative-cursor status-block redraw corrupted the frame. Pinning each
// pane row at its own absolute line decouples the layout from the emulator's width:
// row i lands on line i whether the emulator is wider or narrower than the pane, so
// cursor_y names the same row it named in the pane. A row that overflows a narrower
// emulator wraps, but the next row's absolute CSI H + erase overwrites the overflow,
// so rows never accumulate a drift — correct by construction at any width.
//
// The cursor restore also fixes the earlier duplicated-prompt artifact (#1676):
// writing the screen leaves the emulator cursor at the bottom (past the trailing
// blank rows), but the pane program's cursor is wherever it really is (row 0 for a
// just-started shell). Without the restore, the pane's next relative-positioned
// redraw (a shell's SIGWINCH prompt redraw, which uses CR to return to the current
// line) renders at the bottom while a stale copy sits at the top. The restore lands
// the emulator cursor on the real position so that redraw overwrites in place.
func buildRepaint(snap PaneSnapshot) []byte {
	var out []byte
	if snap.HasModes {
		out = append(out, snap.Modes.RestoreSequence()...)
	}
	out = append(out, []byte("\x1b[2J")...)
	// capture-pane emits ONE trailing "\n" after the last row and strips trailing
	// blank rows, so that final "\n" is a row SEPARATOR, not a real empty row.
	// Splitting without trimming it would yield a phantom trailing "" element and emit
	// an out-of-range CSI (N+1);1 H + erase — which, in an emulator clamped to the pane
	// height, clamps onto the real bottom row and WIPES it (Claude's input/status
	// line). Trim exactly that one separator; a genuinely-blank last row is impossible
	// here because capture-pane strips it. TrimSuffix is a no-op when there is none.
	screen := strings.TrimSuffix(string(snap.Screen), "\n")
	for i, line := range strings.Split(screen, "\n") {
		out = append(out, []byte(fmt.Sprintf("\x1b[%d;1H\x1b[K", i+1))...)
		out = append(out, line...)
	}
	if snap.HasCursor {
		out = append(out, []byte(fmt.Sprintf("\x1b[%d;%dH", snap.CursorRow+1, snap.CursorCol+1))...)
	}
	return out
}

func buildRepaintSnapshot(snap PaneSnapshot) repaintSnapshot {
	return repaintSnapshot{
		data:     buildRepaint(snap),
		modes:    snap.Modes,
		hasModes: snap.HasModes,
	}
}

// snapshotHasRepaintState keeps authoritative metadata from disappearing merely
// because the grid is blank. A fresh primary-screen pane can have no printable
// cells while its all-false mode snapshot is exactly what resolves ownership.
func snapshotHasRepaintState(snap PaneSnapshot) bool {
	return len(snap.Screen) > 0 || snap.HasCursor || snap.HasModes
}

// ensureCaptureStarted brings the clientless output capture up if it is not
// already running, serialized against every other capture transition by
// captureMu so a concurrent teardown can neither interleave with nor clobber it
// (#1661). It runs WITHOUT holding b.mu across the tmux exec ch.StartCapture
// does, and errors (a vanished session) propagate to the caller so the WS dial
// fails and the client reconnects. Idempotent: a no-op when already capturing.
func (b *ptyBroker) ensureCaptureStarted() error {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	return b.ensureCaptureStartedLocked()
}

// ensureCaptureStartedLocked is ensureCaptureStarted's body with captureMu already
// held, so a caller mid-transition (resetCapture, which stops the stale capture and
// restarts a fresh one atomically under one captureMu hold) can reuse the bring-up
// without dropping the serialization. Callers that are not already holding captureMu
// must use ensureCaptureStarted.
func (b *ptyBroker) ensureCaptureStartedLocked() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("pty broker closed")
	}
	if b.capturing {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	r, err := b.ch.StartCapture()
	if err != nil {
		return fmt.Errorf("start clientless pty capture: %w", err)
	}
	done := make(chan struct{})
	b.mu.Lock()
	b.capturing = true
	b.stopCapture = func() {
		if err := b.ch.StopCapture(); err != nil {
			log.WarningLog.Printf("pty broker: stop clientless capture: %v", err)
		}
		_ = r.Close()
		<-done
	}
	b.mu.Unlock()
	go b.readLoop(r, done)
	return nil
}

// maybeStopCapture tears the capture down IFF no subscriber remains — the
// counterpart to ensureCaptureStarted, and serialized against it by captureMu.
// It RE-CHECKS the live subscriber count under captureMu (not just at the caller's
// earlier read) so a subscriber that (re)connected in the meantime keeps its
// capture: this is the invariant that fixes #1661, where a detach's teardown
// raced a reconnect's bring-up and disabled the pane pipe out from under it. The
// blocking teardown runs WITHOUT b.mu held so the read loop (which takes b.mu in
// feed) can drain and exit.
func (b *ptyBroker) maybeStopCapture() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()

	b.mu.Lock()
	if len(b.subs) != 0 || !b.capturing {
		b.mu.Unlock()
		return
	}
	stop := b.stopCapture
	b.capturing = false
	b.stopCapture = nil
	b.mu.Unlock()

	if stop != nil {
		stop()
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
func (b *ptyBroker) resetCapture() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()

	b.mu.Lock()
	if b.closed {
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
	return &rp
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

// readLoop copies the clientless capture reader into the ring until it errors or
// the capture is stopped.
func (b *ptyBroker) readLoop(r io.Reader, done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.feed(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// feed appends output bytes to the ring, evicting the oldest bytes past the cap,
// and wakes every subscriber.
func (b *ptyBroker) feed(p []byte) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.maxBytes {
		drop := len(b.buf) - b.maxBytes
		// Compact in place (dst index < src index, a safe forward copy) so the
		// backing array does not grow without bound.
		b.buf = append(b.buf[:0], b.buf[drop:]...)
		b.base += Seq(drop)
	}
	b.wakeAllLocked()
	b.mu.Unlock()
}

// input writes raw bytes to the pane. Accepted from any subscriber (multi-writer).
func (b *ptyBroker) input(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	return b.ch.SendRaw(p)
}

// resize records the winning size (last-resize-wins) and broadcasts an
// authoritative echo to every subscriber so their emulators reflow, then applies
// the size to the pane best-effort. The echo is the broker's authoritative
// decision and is broadcast REGARDLESS of whether the tmux apply succeeds — a
// failed resize-window (an old tmux, a vanished pane) must not swallow the echo
// clients depend on to reflow; the apply error is logged, not propagated as a
// missing echo.
func (b *ptyBroker) resize(rows, cols uint16) error {
	b.mu.Lock()
	b.rows, b.cols = rows, cols
	b.hasSize = true
	b.resizeGen++
	b.wakeAllLocked()
	b.mu.Unlock()

	if err := b.ch.Resize(rows, cols); err != nil {
		log.WarningLog.Printf("pty broker: apply resize %dx%d to pane: %v", rows, cols, err)
		return err
	}
	// Deliberately NO capture-pane repaint on resize: resize-window sends the pane's
	// process a SIGWINCH, and both full-screen programs and the shell's readline
	// redraw themselves at the new size through pipe-pane, so the reflowed screen
	// already streams. Injecting an ED-2 repaint here instead RACES that live
	// redraw — its clear can wipe output typed right after the resize (it broke the
	// wrapped-command self-test). The emulator re-windows the existing grid until the
	// SIGWINCH redraw lands, so the pane never blanks. The initial-subscribe repaint
	// (which has no concurrent output to race) remains the fix for a fresh pane.
	return nil
}

// wakeAllLocked signals every subscriber that state changed. The notify channel
// is a coalescing (cap-1) doorbell — a subscriber re-reads all state under mu
// after waking, so a single pending signal is enough. Caller holds mu.
func (b *ptyBroker) wakeAllLocked() {
	for _, s := range b.subs {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// remove drops a subscriber and stops the clientless capture once the last one
// leaves — never touching the PTY/session itself. The stop goes through
// maybeStopCapture, which re-checks the subscriber count under captureMu so a
// subscriber that connects while this teardown is deciding is not stranded on a
// disabled pipe (#1661).
func (b *ptyBroker) remove(id uint64) {
	b.mu.Lock()
	if _, ok := b.subs[id]; !ok {
		b.mu.Unlock()
		return
	}
	delete(b.subs, id)
	lastLeft := len(b.subs) == 0 && b.capturing
	b.mu.Unlock()
	if lastLeft {
		b.maybeStopCapture()
	}
}

// close tears down the broker: every subscriber's NextEvent returns io.EOF and
// the clientless capture is stopped. Called when the session is killed.
func (b *ptyBroker) close() { b.shutdown(false) }

// closeTab is close for a broker whose TAB was closed (#2136) — the same
// teardown, but each subscriber's NextEvent reports ErrTabClosed so the WS writer
// can tell the client its tab went away rather than leaving it to time out on the
// keepalive. Only the closed tab's broker is shut down; a sibling tab of the same
// session has its own broker and keeps streaming.
func (b *ptyBroker) closeTab() { b.shutdown(true) }

// shutdown is the shared teardown behind close/closeTab. Holds captureMu
// (captureMu-then-mu ordering) so it cannot race a concurrent
// ensureCaptureStarted into resurrecting a capture on a closed broker (#1661) —
// a bring-up that lost the race sees b.closed and unwinds. Idempotent: a second
// shutdown (a tab closed while the session is being killed, or the reverse) sees
// closed and returns without re-running the teardown or flipping the reason.
func (b *ptyBroker) shutdown(tabClosed bool) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.tabClosed = tabClosed
	b.wakeAllLocked()
	var stop func()
	if b.capturing {
		stop = b.stopCapture
		b.capturing = false
		b.stopCapture = nil
	}
	b.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// ptySub is one subscriber's cursor into the broker's ring plus a resize-echo
// watermark. All state is read/written under br.mu; notify is the wake doorbell.
type ptySub struct {
	br         *ptyBroker
	id         uint64
	cursor     Seq    // next output byte to deliver
	resizeSeen uint64 // last resizeGen echoed to this subscriber
	// pendingRepaint is a one-shot initial screen repaint delivered before any
	// other event to a fresh subscriber (set by subscribe). Read/written under br.mu.
	pendingRepaint *repaintSnapshot
	// repaintArmed marks this subscriber as held at a recovery boundary: a repaint of
	// the recovered screen is being captured for it, so NextEvent must emit NOTHING
	// until it lands (#1975). Set/cleared under br.mu by resetCapture's arm/release
	// pair, which is the only thing that may set it.
	repaintArmed bool
	notify       chan struct{}
	closeOnce    sync.Once
}

// wake signals this subscriber's doorbell (coalescing, cap-1). Safe to call
// without br.mu held.
func (s *ptySub) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

var _ PTYSubscription = (*ptySub)(nil)

// NextEvent blocks for the next event for this subscriber: a pending resize echo
// (delivered before bytes so the emulator resizes before the reflow bytes land),
// then any output bytes from the cursor, else it waits. Returns io.EOF when the
// broker closes.
func (s *ptySub) NextEvent(ctx context.Context) (PTYEvent, error) {
	for {
		s.br.mu.Lock()
		if s.br.closed {
			tabClosed := s.br.tabClosed
			s.br.mu.Unlock()
			if tabClosed {
				// ErrTabClosed wraps io.EOF, so a consumer that only asks "is the stream
				// over" is unaffected; the daemon's WS writer asks the narrower question
				// and turns it into an exit with a tab_closed reason (#2136).
				return PTYEvent{}, ErrTabClosed
			}
			return PTYEvent{}, io.EOF
		}
		// The initial screen repaint is delivered before anything else, so a fresh
		// subscriber paints the current screen before the first live byte lands. It
		// is a PTYRepaint (not PTYData) so the client renders it without advancing its
		// replay cursor — the repaint is per-subscriber and not part of the ring seq.
		if s.pendingRepaint != nil {
			rp := s.pendingRepaint
			s.pendingRepaint = nil
			s.br.mu.Unlock()
			return PTYEvent{
				Kind:     PTYRepaint,
				Data:     rp.data,
				Modes:    rp.modes,
				HasModes: rp.hasModes,
			}, nil
		}
		// A recovery is in flight and this subscriber's repaint of the recovered screen
		// is still being captured: emit NOTHING until it lands (#1975). Everything this
		// subscriber could send right now paints the WRONG screen — the dead pane's
		// buffered tail, the cursor jump the discard is about to make, or the re-spawned
		// pane's first bytes rendered onto the pre-death frame — and the repaint wipes it
		// a moment later, which is the flicker. Held, not dropped: nothing is consumed
		// here, so after the release the repaint goes first and the same cursor/data
		// events follow it in order.
		if s.repaintArmed {
			if err := s.awaitWake(ctx); err != nil {
				return PTYEvent{}, err
			}
			continue
		}
		if s.br.hasSize && s.resizeSeen != s.br.resizeGen {
			s.resizeSeen = s.br.resizeGen
			ev := PTYEvent{Kind: PTYResize, Rows: s.br.rows, Cols: s.br.cols}
			s.br.mu.Unlock()
			return ev, nil
		}
		head := s.br.headLocked()
		if s.cursor < s.br.base {
			// Fell behind eviction, or the #1840 recovery discard dropped the dead pane's
			// ring: the bytes between the cursor and base provably no longer exist, so
			// skip the lost gap. TELL the client where its cursor landed (#1845
			// follow-up) — the jump is the SERVER's, and a client that derives its cursor
			// as start + bytes-received cannot see it. Left silent, the client keeps
			// counting from its stale cursor, and its next reconnect asks for a ?since
			// below base, gets clamped back up, and is re-sent bytes it already rendered
			// — duplicated output on a screen that was correct. Delivered as its own
			// event (rather than riding the next PTYData) so the re-seed reaches the
			// client even when the recovered pane stays silent afterward. The clamp is
			// idempotent — cursor == base now, so this fires once per jump.
			s.cursor = s.br.base
			ev := PTYEvent{Kind: PTYCursor, Seq: s.cursor}
			s.br.mu.Unlock()
			return ev, nil
		}
		if s.cursor < head {
			off := int(s.cursor - s.br.base)
			data := append([]byte(nil), s.br.buf[off:]...)
			s.cursor = head
			s.br.mu.Unlock()
			return PTYEvent{Kind: PTYData, Data: data}, nil
		}
		if err := s.awaitWake(ctx); err != nil {
			return PTYEvent{}, err
		}
	}
}

// awaitWake RELEASES br.mu (which the caller must hold) and blocks until this
// subscriber's doorbell rings or ctx ends. NextEvent re-takes the lock and re-reads all
// state on the next iteration, so the doorbell only has to say "something changed".
func (s *ptySub) awaitWake(ctx context.Context) error {
	s.br.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.notify:
		return nil
	}
}

// Seq reports the cursor of the next output byte this subscriber will read.
func (s *ptySub) Seq() Seq {
	s.br.mu.Lock()
	defer s.br.mu.Unlock()
	return s.cursor
}

// Close removes the subscriber from the broker's fan-out. Idempotent.
func (s *ptySub) Close() error {
	s.closeOnce.Do(func() { s.br.remove(s.id) })
	return nil
}
