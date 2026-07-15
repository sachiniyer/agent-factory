package termpane

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStream is an in-memory termpane.Stream: the test pushes inbound events (PTY
// output, resize echoes) and drops the connection at will, and reads back the
// INPUT/RESIZE frames the TermPane sent. It stands in for the daemon WS stream so
// the emulator + reconnect logic is exercised with no real socket or tmux.
type fakeStream struct {
	startSeq uint64
	events   chan Event
	closed   chan struct{}
	once     sync.Once

	mu      sync.Mutex
	input   []byte
	resizes [][2]uint16 // {rows, cols}
}

func newFakeStream(startSeq uint64) *fakeStream {
	return &fakeStream{startSeq: startSeq, events: make(chan Event, 64), closed: make(chan struct{})}
}

func (s *fakeStream) StartSeq() uint64 { return s.startSeq }

func (s *fakeStream) Recv(ctx context.Context) (Event, error) {
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case <-s.closed:
		return Event{}, io.EOF
	case ev := <-s.events:
		return ev, nil
	}
}

func (s *fakeStream) SendInput(_ context.Context, b []byte) error {
	s.mu.Lock()
	s.input = append(s.input, b...)
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) SendResize(_ context.Context, rows, cols uint16) error {
	s.mu.Lock()
	s.resizes = append(s.resizes, [2]uint16{rows, cols})
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// feed pushes verbatim PTY output onto the stream (a server → client PTY_OUT
// frame).
func (s *fakeStream) feed(b string) { s.events <- Event{Kind: EventData, Data: []byte(b)} }

// feedRepaint pushes a one-shot repaint (a server → client OpRepaint frame):
// rendered like output but must NOT advance the replay cursor.
func (s *fakeStream) feedRepaint(b string) { s.events <- Event{Kind: EventRepaint, Data: []byte(b)} }

// feedCursor pushes a cursor re-seed (a server → client OpHello frame): the server
// announcing that it moved this subscription's cursor over bytes it no longer holds.
func (s *fakeStream) feedCursor(seq uint64) { s.events <- Event{Kind: EventCursor, Seq: seq} }

func (s *fakeStream) sentInput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.input)
}

func (s *fakeStream) lastResize() ([2]uint16, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.resizes) == 0 {
		return [2]uint16{}, false
	}
	return s.resizes[len(s.resizes)-1], true
}

// queueDialer returns a Dialer that hands out pre-seeded streams in order and
// records the ?since cursor each dial requested (so a test can prove a reconnect
// replayed from the tracked cursor). Once the queue is exhausted it blocks until
// ctx is cancelled, so a spent dialer never busy-loops on error backoff.
type queueDialer struct {
	mu      sync.Mutex
	streams []*fakeStream
	sinces  []uint64
	idx     int
}

func (d *queueDialer) dial(ctx context.Context, since uint64) (Stream, error) {
	d.mu.Lock()
	d.sinces = append(d.sinces, since)
	if d.idx >= len(d.streams) {
		d.mu.Unlock()
		<-ctx.Done() // no more streams: park until Close cancels the run loop
		return nil, ctx.Err()
	}
	s := d.streams[d.idx]
	d.idx++
	d.mu.Unlock()
	return s, nil
}

func (d *queueDialer) sinceAt(i int) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i < 0 || i >= len(d.sinces) {
		return 0, false
	}
	return d.sinces[i], true
}

func plainRender(tp *TermPane, width, height int) string {
	return ansi.Strip(tp.Render(width, height, false))
}

func waitForRender(t *testing.T, tp *TermPane, width, height int, want string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		return strings.Contains(plainRender(tp, width, height), want)
	}, 5*time.Second, 10*time.Millisecond, "grid never showed %q; last frame:\n%s", want, plainRender(tp, width, height))
}

// newSingleStreamPane starts a TermPane wired to one persistent fake stream.
func newSingleStreamPane(t *testing.T, width, height int) (*TermPane, *fakeStream) {
	t.Helper()
	s := newFakeStream(0)
	d := &queueDialer{streams: []*fakeStream{s}}
	tp := New(d.dial, width, height)
	t.Cleanup(func() { _ = tp.Close() })
	return tp, s
}

func TestStreamRendersIntoGrid(t *testing.T) {
	tp, s := newSingleStreamPane(t, 40, 6)
	s.feed("MARKER-PR6-preview")
	waitForRender(t, tp, 40, 6, "MARKER-PR6-preview")

	// The width x height contract holds.
	lines := strings.Split(tp.Render(40, 6, false), "\n")
	require.Len(t, lines, 6)
	for i, line := range lines {
		require.Equalf(t, 40, ansi.StringWidth(line), "line %d width", i)
	}
}

func TestResizeSendsResizeFrame(t *testing.T) {
	tp, s := newSingleStreamPane(t, 80, 24)
	s.feed("hi")
	waitForRender(t, tp, 80, 24, "hi")

	tp.Resize(100, 30)
	require.Eventually(t, func() bool {
		r, ok := s.lastResize()
		return ok && r == [2]uint16{30, 100} // rows, cols
	}, 2*time.Second, 10*time.Millisecond, "Resize must send a RESIZE frame (rows,cols)=(30,100)")
}

// TestResizeKeepsContentThenServerRepaints pins the PR6 resize behavior: unlike
// the old tmux attach client (which blanked locally and relied on tmux's redraw),
// the WS client does NOT blank on resize — pipe-pane never carries tmux's redraw,
// so blanking would leave the pane empty until output. Instead the emulator
// re-windows (content stays visible), and the daemon injects a clean capture-pane
// repaint (ED 2 + reflowed screen) that the client applies a round-trip later.
func TestResizeKeepsContentThenServerRepaints(t *testing.T) {
	tp, s := newSingleStreamPane(t, 40, 6)
	s.feed("keep-me-on-resize\r\n")
	waitForRender(t, tp, 40, 6, "keep-me-on-resize")

	// Resize does not blank: the content is still there (re-windowed), never an
	// empty pane.
	tp.Resize(30, 8)
	assert.Contains(t, plainRender(tp, 30, 8), "keep-me-on-resize",
		"resize must NOT blank the WS pane — pipe-pane carries no tmux redraw to recover from a blank")

	// The daemon's repaint (clear + reflowed screen) arrives over the stream and
	// cleanly redraws at the new size.
	s.feed("\x1b[2J\x1b[Hreflowed-after-resize\r\n")
	waitForRender(t, tp, 30, 8, "reflowed-after-resize")
	assert.NotContains(t, plainRender(tp, 30, 8), "keep-me-on-resize",
		"the server repaint (ED 2) must clear the stale grid before redrawing")
}

func TestSendKeyEmitsInputFrame(t *testing.T) {
	tp, s := newSingleStreamPane(t, 40, 6)
	// A feed first so the connection is live before we type.
	s.feed("ready")
	waitForRender(t, tp, 40, 6, "ready")

	for _, r := range "typed-pr6" {
		require.True(t, tp.SendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
	}
	require.Eventually(t, func() bool {
		return strings.Contains(s.sentInput(), "typed-pr6")
	}, 2*time.Second, 10*time.Millisecond, "SendKey must emit INPUT frames to the stream")
}

// TestReconnectReplaysWithSinceNoGap is the §6.3 acceptance at the unit level: a
// dropped subscription reconnects and REPLAYS the gap it missed via ?since —
// repainting from the stream, never from a capture fallback (there is none). The
// second dial must request since = startSeq + bytesReceived, and the bytes
// produced while disconnected must appear after reconnect.
func TestReconnectReplaysWithSinceNoGap(t *testing.T) {
	s1 := newFakeStream(0)
	// s2 replays from the drop cursor: its StartSeq is the byte count s1 delivered,
	// and it carries the bytes produced during the gap.
	s2 := newFakeStream(uint64(len("AAA")))
	d := &queueDialer{streams: []*fakeStream{s1, s2}}
	tp := New(d.dial, 40, 6)
	t.Cleanup(func() { _ = tp.Close() })

	s1.feed("AAA")
	waitForRender(t, tp, 40, 6, "AAA")

	// The connection drops mid-stream. "BBB" is produced while disconnected and the
	// server's ring retains it for the replay.
	_ = s1.Close()
	s2.feed("BBB")

	// Reconnect repaints the gap from ?since — AAABBB, seamless, no capture snapshot.
	waitForRender(t, tp, 40, 6, "AAABBB")

	// The first dial started at the live tail (0); the reconnect requested exactly
	// the drop cursor, proving replay rather than a fresh live tail.
	first, ok := d.sinceAt(0)
	require.True(t, ok)
	assert.Equal(t, uint64(0), first)
	second, ok := d.sinceAt(1)
	require.True(t, ok)
	assert.Equal(t, uint64(len("AAA")), second, "reconnect must replay from the tracked cursor (?since)")
}

// TestRepaintRendersWithoutAdvancingCursor pins the fix for the cursor desync a
// naive repaint caused: a repaint (initial screen / post-resize redraw) must render
// like output but NOT count toward the replay cursor — it is per-subscriber and not
// part of the server's ring seq, so counting it would make a reconnect request a
// ?since past the gap and miss it.
func TestRepaintRendersWithoutAdvancingCursor(t *testing.T) {
	s1 := newFakeStream(0)
	// s2 replays from the DATA cursor (len "AAA"), not len("AAA")+len(repaint).
	s2 := newFakeStream(uint64(len("AAA")))
	d := &queueDialer{streams: []*fakeStream{s1, s2}}
	tp := New(d.dial, 40, 6)
	t.Cleanup(func() { _ = tp.Close() })

	s1.feed("AAA") // EventData: advances the cursor to 3
	waitForRender(t, tp, 40, 6, "AAA")
	s1.feedRepaint("\x1b[2J\x1b[HZZZ") // EventRepaint: renders, but must not advance
	waitForRender(t, tp, 40, 6, "ZZZ")

	// Drop → reconnect. The reconnect must request ?since = 3 (the DATA count), not
	// 6 (data + repaint), or it would skip the gap.
	_ = s1.Close()
	s2.feed("BBB")
	waitForRender(t, tp, 40, 6, "BBB")
	second, ok := d.sinceAt(1)
	require.True(t, ok)
	assert.Equal(t, uint64(len("AAA")), second, "a repaint must not advance the replay cursor")
}

// TestCursorEventReseedsReplayCursor pins the client half of the #1845 follow-up: when
// the SERVER moves this subscription's cursor — a ring eviction, or the post-recovery
// discard of a dead pane's ring — the pane must adopt the announced cursor VERBATIM
// rather than keep counting from its own start + bytes-received total, which cannot see
// a jump it did not receive bytes for.
//
// Fail-before/pass-after: without the EventCursor case the pane ignores the
// announcement and keeps its stale count, so the reconnect requests ?since=6 (3 + 3)
// instead of 503 — a cursor below the broker's base, which clamps it back up and
// re-sends bytes the pane already rendered (duplicated terminal output).
func TestCursorEventReseedsReplayCursor(t *testing.T) {
	s1 := newFakeStream(0)
	// s2 replays from the re-seeded cursor plus the bytes rendered since it.
	s2 := newFakeStream(503)
	d := &queueDialer{streams: []*fakeStream{s1, s2}}
	tp := New(d.dial, 40, 6)
	t.Cleanup(func() { _ = tp.Close() })

	s1.feed("AAA") // EventData: advances the cursor 0 → 3
	waitForRender(t, tp, 40, 6, "AAA")

	// The server discards its ring and fast-forwards this subscriber to 500 — every
	// byte below that is gone. It announces the jump; our local count (3) is now stale.
	s1.feedCursor(500)
	s1.feed("BBB") // EventData: advances the ADOPTED cursor 500 → 503
	waitForRender(t, tp, 40, 6, "AAABBB")

	// Drop → reconnect. ?since must be the announced cursor plus what we rendered after
	// it (500 + 3), not our own byte count (3 + 3).
	_ = s1.Close()
	s2.feed("CCC")
	waitForRender(t, tp, 40, 6, "AAABBBCCC")
	second, ok := d.sinceAt(1)
	require.True(t, ok)
	assert.Equal(t, uint64(503), second, "the pane must adopt the server's announced cursor, not its own byte count")
}

// TestReconnectReassertsDesiredSize pins that a pane resized while disconnected
// re-asserts its size to the server on reconnect (last-resize-wins), so the new
// stream sizes the window to the pane.
func TestReconnectReassertsDesiredSize(t *testing.T) {
	s1 := newFakeStream(0)
	s2 := newFakeStream(0)
	d := &queueDialer{streams: []*fakeStream{s1, s2}}
	tp := New(d.dial, 80, 24)
	t.Cleanup(func() { _ = tp.Close() })

	s1.feed("x")
	waitForRender(t, tp, 80, 24, "x")

	tp.Resize(120, 40) // rows=40, cols=120
	_ = s1.Close()     // drop; reconnect should re-assert the new size

	require.Eventually(t, func() bool {
		r, ok := s2.lastResize()
		return ok && r == [2]uint16{40, 120}
	}, 2*time.Second, 10*time.Millisecond, "reconnect must re-assert the desired size to the new stream")
}

// TestDialFailureRetriesThenConnects pins that a failed dial backs off and retries
// rather than giving up — the session may not be ready on the first attempt.
func TestDialFailureRetriesThenConnects(t *testing.T) {
	s := newFakeStream(0)
	var mu sync.Mutex
	attempts := 0
	dial := func(ctx context.Context, _ uint64) (Stream, error) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			return nil, fmt.Errorf("not ready yet")
		}
		return s, nil
	}
	tp := New(dial, 30, 4)
	t.Cleanup(func() { _ = tp.Close() })

	s.feed("late-connect")
	waitForRender(t, tp, 30, 4, "late-connect")
}

// TestSendKeyNeverBlocksWhileDisconnected pins the pump-drain contract: the
// emulator's input pipe is unbuffered, so if the send pump stopped draining while
// disconnected, the next SendKey would block the event loop forever. With no live
// stream the bytes are dropped, but SendKey must always return promptly.
func TestSendKeyNeverBlocksWhileDisconnected(t *testing.T) {
	// A dialer that never connects, so the pane is permanently "reconnecting".
	dial := func(ctx context.Context, _ uint64) (Stream, error) {
		return nil, fmt.Errorf("never connects")
	}
	tp := New(dial, 20, 4)
	t.Cleanup(func() { _ = tp.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			tp.SendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SendKey blocked while disconnected")
	}
}

func TestCloseIsIdempotentAndDrains(t *testing.T) {
	tp, s := newSingleStreamPane(t, 20, 4)
	s.feed("up")
	waitForRender(t, tp, 20, 4, "up")

	require.NoError(t, tp.Close())
	// The last frame stays renderable after Close.
	assert.Contains(t, plainRender(tp, 20, 4), "up")
	// Idempotent.
	require.NoError(t, tp.Close())
}

func TestStartStopCyclesDoNotLeakGoroutines(t *testing.T) {
	warm, ws := newFakeStream(0), 0
	_ = ws
	d := &queueDialer{streams: []*fakeStream{warm}}
	tp := New(d.dial, 20, 4)
	warm.feed("warm")
	waitForRender(t, tp, 20, 4, "warm")
	require.NoError(t, tp.Close())

	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		s := newFakeStream(0)
		dd := &queueDialer{streams: []*fakeStream{s}}
		cycle := New(dd.dial, 30, 5)
		s.feed(fmt.Sprintf("cycle-%d", i))
		waitForRender(t, cycle, 30, 5, fmt.Sprintf("cycle-%d", i))
		require.NoError(t, cycle.Close())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= base {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines must drain to baseline after close cycles (base=%d, now=%d)", base, runtime.NumGoroutine())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
