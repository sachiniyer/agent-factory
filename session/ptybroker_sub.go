package session

// The subscriber side of the WS PTY broker: ptySub is one subscriber's cursor
// into the broker's ring plus its delivery loop (NextEvent), wake doorbell, and
// Close. The broker state machine itself — the ring, the clientless capture
// lifecycle, fan-out — stays in ptybroker.go; this file is the per-subscriber
// view that turns shared broker state into a per-client event stream.
// Split out of ptybroker.go as a file-length decomposition (#1145) — same
// package, so nothing here is a new boundary.

import (
	"context"
	"io"
	"sync"
)

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
		// The authoritative size echo goes before ANY screen content — a fresh
		// subscriber's repaint must land in an emulator already at the pane's real
		// size, or the snapshot wraps at whatever geometry the client started with
		// (#4480). A lagging subscriber likewise learns the new size before the
		// reflowed bytes that follow it. The echo is size metadata, not screen
		// content, so it also precedes the recovery barrier: an armed subscriber
		// may hear the size while its repaint is still being captured.
		if s.br.hasSize && s.resizeSeen != s.br.resizeGen {
			s.resizeSeen = s.br.resizeGen
			ev := PTYEvent{Kind: PTYResize, Rows: s.br.rows, Cols: s.br.cols}
			s.br.mu.Unlock()
			return ev, nil
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
				Kind:              PTYRepaint,
				Data:              rp.data,
				Modes:             rp.modes,
				HasModes:          rp.hasModes,
				RepaintProvenance: rp.provenance,
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

// Close removes the subscriber from the broker's fan-out and requests capture
// teardown without waiting for its blocking reader to exit. Idempotent.
func (s *ptySub) Close() error {
	s.closeOnce.Do(func() { s.br.remove(s.id) })
	return nil
}
