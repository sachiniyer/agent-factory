package session

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
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
	// Snapshot returns the pane's current visible screen AND the pane cursor
	// position — the repaint the broker injects on subscribe, since pipe-pane never
	// delivers tmux's screen redraw (#1592 Phase 2 PR6). The cursor position lets the
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
type PaneSnapshot struct {
	Screen    []byte
	CursorRow int
	CursorCol int
	HasCursor bool
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
	cursor := since
	// since == 0 means "from the live tail" (the documented default); a real
	// replay cursor is clamped into the retained window [base, head].
	if cursor == 0 || cursor > head {
		cursor = head
	}
	if cursor < b.base {
		cursor = b.base
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

	// A fresh subscriber (since == 0, the live tail) gets an initial repaint of the
	// current screen — pipe-pane only streams future output, so without this a
	// just-opened pane renders blank until the next byte. A reconnecting client
	// (since > 0) resumes via ?since replay instead, so it is left seamless.
	// Captured AFTER registration (register-first) so no output can slip in between
	// the snapshot and the cursor: bytes in that tiny window are simply in both the
	// snapshot and the replayed tail (a harmless double-render), never dropped.
	if since == 0 {
		if snap, err := b.ch.Snapshot(); err == nil && len(snap.Screen) > 0 {
			rp := buildRepaint(snap)
			b.mu.Lock()
			sub.pendingRepaint = rp
			b.mu.Unlock()
			sub.wake()
		}
	}
	return sub, nil
}

// buildRepaint turns a pane snapshot into bytes that reconstruct the screen when
// written to the emulator: clear + home, then the captured lines with CR before
// each LF so every line starts at column 0 (capture-pane joins lines with bare
// "\n", which would otherwise leave the column where the previous line ended).
//
// It ends by restoring the cursor to the pane's actual position (1-based CSI H)
// when the snapshot carries one. Writing the full screen leaves the emulator
// cursor at the bottom of the captured lines — including the trailing blank rows a
// fresh shell's snapshot carries — but the pane program's cursor is wherever it
// really is (row 0 for a just-started shell). Without the restore, the pane's next
// relative-positioned output (a shell's SIGWINCH prompt redraw, which uses CR to
// return to the current line) renders at the bottom while the repaint's copy sits
// stale at the top: the duplicated-prompt artifact. The restore lands the emulator
// cursor on the real position so that redraw overwrites in place.
func buildRepaint(snap PaneSnapshot) []byte {
	body := strings.ReplaceAll(string(snap.Screen), "\n", "\r\n")
	out := append([]byte("\x1b[2J\x1b[H"), body...)
	if snap.HasCursor {
		out = append(out, []byte(fmt.Sprintf("\x1b[%d;%dH", snap.CursorRow+1, snap.CursorCol+1))...)
	}
	return out
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
// the clientless capture is stopped. Called when the session is killed. Holds
// captureMu (captureMu-then-mu ordering) so it cannot race a concurrent
// ensureCaptureStarted into resurrecting a capture on a closed broker (#1661) —
// a bring-up that lost the race sees b.closed and unwinds.
func (b *ptyBroker) close() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
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
	pendingRepaint []byte
	notify         chan struct{}
	closeOnce      sync.Once
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
			s.br.mu.Unlock()
			return PTYEvent{}, io.EOF
		}
		// The initial screen repaint is delivered before anything else, so a fresh
		// subscriber paints the current screen before the first live byte lands. It
		// is a PTYRepaint (not PTYData) so the client renders it without advancing its
		// replay cursor — the repaint is per-subscriber and not part of the ring seq.
		if len(s.pendingRepaint) > 0 {
			data := s.pendingRepaint
			s.pendingRepaint = nil
			s.br.mu.Unlock()
			return PTYEvent{Kind: PTYRepaint, Data: data}, nil
		}
		if s.br.hasSize && s.resizeSeen != s.br.resizeGen {
			s.resizeSeen = s.br.resizeGen
			ev := PTYEvent{Kind: PTYResize, Rows: s.br.rows, Cols: s.br.cols}
			s.br.mu.Unlock()
			return ev, nil
		}
		head := s.br.headLocked()
		if s.cursor < s.br.base {
			s.cursor = s.br.base // fell behind eviction: skip the lost gap
		}
		if s.cursor < head {
			off := int(s.cursor - s.br.base)
			data := append([]byte(nil), s.br.buf[off:]...)
			s.cursor = head
			s.br.mu.Unlock()
			return PTYEvent{Kind: PTYData, Data: data}, nil
		}
		s.br.mu.Unlock()

		select {
		case <-ctx.Done():
			return PTYEvent{}, ctx.Err()
		case <-s.notify:
		}
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
