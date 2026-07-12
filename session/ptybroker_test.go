package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeClientlessChannel is an in-memory clientlessChannel: StartCapture hands
// back an io.Pipe the test feeds with emit(), and SendRaw/Resize record calls so
// the broker's input/size fan-out can be asserted without a real tmux server.
type fakeClientlessChannel struct {
	mu       sync.Mutex
	r        *io.PipeReader
	w        *io.PipeWriter
	starts   int
	stops    int
	sent     [][]byte
	resizes  [][2]uint16
	startErr error
	// snapshot is the canned current-screen the broker injects as the repaint on
	// subscribe and after a resize; snapshots counts how many times it was read.
	snapshot  []byte
	snapshots int
	// wClosed reports whether the CURRENT capture writer (f.w) has been closed by a
	// StopCapture — the test proxy for "the pane pipe was disabled". Reset on each
	// StartCapture. Used by the #1661 teardown-clobber regression test.
	wClosed bool
	// stopEntered/stopRelease gate StopCapture: when both are non-nil, StopCapture
	// signals stopEntered on entry and then blocks on stopRelease before doing any
	// teardown, so a test can pin the "teardown runs mid-reconnect" interleaving.
	stopEntered chan struct{}
	stopRelease chan struct{}
	// stopDone, when non-nil, is signaled AFTER StopCapture has closed the writer,
	// so a test can order an assertion strictly after the teardown's effect.
	stopDone chan struct{}
}

func (f *fakeClientlessChannel) StartCapture() (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.starts++
	f.r, f.w = io.Pipe()
	f.wClosed = false
	return f.r, nil
}

func (f *fakeClientlessChannel) StopCapture() error {
	// Gate BEFORE taking f.mu so a concurrent StartCapture can proceed while this
	// teardown is parked — the interleaving the #1661 regression test forces.
	if f.stopEntered != nil && f.stopRelease != nil {
		f.stopEntered <- struct{}{}
		<-f.stopRelease
	}
	f.mu.Lock()
	f.stops++
	if f.w != nil {
		_ = f.w.Close()
		f.wClosed = true
	}
	f.mu.Unlock()
	if f.stopDone != nil {
		f.stopDone <- struct{}{}
	}
	return nil
}

func (f *fakeClientlessChannel) SendRaw(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, append([]byte(nil), b...))
	return nil
}

func (f *fakeClientlessChannel) Resize(rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint16{rows, cols})
	return nil
}

func (f *fakeClientlessChannel) Snapshot() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots++
	return append([]byte(nil), f.snapshot...), nil
}

// emit writes pane output into the capture pipe. Write blocks until the broker's
// read loop consumes it, so on return the bytes are in flight to the ring.
func (f *fakeClientlessChannel) emit(t *testing.T, b []byte) {
	t.Helper()
	f.mu.Lock()
	w := f.w
	f.mu.Unlock()
	if w == nil {
		t.Fatal("emit before StartCapture")
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

// emitErr writes pane output into the current capture pipe and RETURNS any error
// (rather than fataling) so a test can assert the pipe is still live — a torn-down
// pipe surfaces as a write error.
func (f *fakeClientlessChannel) emitErr(b []byte) error {
	f.mu.Lock()
	w := f.w
	f.mu.Unlock()
	if w == nil {
		return io.ErrClosedPipe
	}
	_, err := w.Write(b)
	return err
}

func nextWithin(t *testing.T, sub PTYSubscription, d time.Duration) (PTYEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return sub.NextEvent(ctx)
}

// mustData blocks for the next event and asserts it is output bytes equal to want.
func mustData(t *testing.T, sub PTYSubscription, want string) {
	t.Helper()
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev.Kind != PTYData || string(ev.Data) != want {
		t.Fatalf("event = %+v, want data %q", ev, want)
	}
}

// TestPTYBrokerInitialRepaint pins the #1592 PR6 initial-repaint injection: a fresh
// subscriber's FIRST event is a clean repaint of the current screen (pipe-pane
// carries no history, so without it a just-opened pane is blank), delivered as a
// PTYRepaint so the client renders it WITHOUT advancing its replay cursor. A resize
// deliberately injects NO repaint (it would race the SIGWINCH redraw), and a
// reconnecting subscriber (since > 0) gets no repaint either — it resumes via replay.
func TestPTYBrokerInitialRepaint(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-A\nline2")}
	br := newPTYBroker(ch)

	sub, err := br.subscribe(0) // fresh live-tail subscriber
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// First event is the initial repaint: ED2 + home + the screen with CRLF line
	// breaks so columns reset. It is a PTYRepaint (not counted toward the cursor).
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("initial NextEvent: %v", err)
	}
	want := "\x1b[2J\x1b[HSCREEN-A\r\nline2"
	if ev.Kind != PTYRepaint || string(ev.Data) != want {
		t.Fatalf("initial event = %+v, want PTYRepaint %q", ev, want)
	}

	// A resize broadcasts the authoritative echo but NO repaint (the process redraws
	// itself on SIGWINCH through the stream; a repaint here would race it).
	if err := br.resize(40, 100); err != nil {
		t.Fatalf("resize: %v", err)
	}
	ev, err = nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("resize NextEvent: %v", err)
	}
	if ev.Kind != PTYResize || ev.Rows != 40 || ev.Cols != 100 {
		t.Fatalf("first post-resize event = %+v, want the authoritative resize echo", ev)
	}
	// The next event is live output, NOT a resize repaint.
	ch.emit(t, []byte("live-after-resize"))
	mustData(t, sub, "live-after-resize")

	// A reconnecting subscriber (since > 0) gets NO repaint — it resumes seamlessly
	// via replay. A since past the head clamps to the live tail.
	re, err := br.subscribe(1 << 40)
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	ch.emit(t, []byte("live"))
	mustData(t, re, "live")
}

func TestPTYBrokerFanout(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe a: %v", err)
	}
	b, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe b: %v", err)
	}
	ch.emit(t, []byte("hello"))
	mustData(t, a, "hello")
	mustData(t, b, "hello")
	if ch.starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 (lazy, once)", ch.starts)
	}
}

func TestPTYBrokerReplayFromSeq(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	ch.emit(t, []byte("hello"))
	mustData(t, a, "hello")
	if got := a.Seq(); got != 5 {
		t.Fatalf("seq after 5 bytes = %d, want 5", got)
	}
	// A reconnecting client resumes from a prior cursor and replays the gap.
	b, err := br.subscribe(2)
	if err != nil {
		t.Fatalf("subscribe replay: %v", err)
	}
	mustData(t, b, "llo")
}

func TestPTYBrokerSinceZeroIsLiveTail(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	ch.emit(t, []byte("old"))
	mustData(t, a, "old")
	// since==0 means "from the live tail": no replay of already-buffered bytes.
	b, _ := br.subscribe(0)
	ch.emit(t, []byte("new"))
	mustData(t, b, "new")
}

func TestPTYBrokerResizeEchoBroadcast(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	b, _ := br.subscribe(0)
	if err := br.resize(24, 80); err != nil {
		t.Fatalf("resize: %v", err)
	}
	// last-resize-wins: a second resize supersedes the first authoritative size.
	if err := br.resize(30, 100); err != nil {
		t.Fatalf("resize 2: %v", err)
	}
	for _, sub := range []PTYSubscription{a, b} {
		ev, err := nextWithin(t, sub, 2*time.Second)
		if err != nil {
			t.Fatalf("resize NextEvent: %v", err)
		}
		if ev.Kind != PTYResize || ev.Rows != 30 || ev.Cols != 100 {
			t.Fatalf("resize echo = %+v, want 30x100 (last-wins)", ev)
		}
	}
	if len(ch.resizes) != 2 {
		t.Fatalf("channel resizes = %v, want 2 applied", ch.resizes)
	}
}

func TestPTYBrokerInput(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	if err := br.input([]byte{0x1b, 0x5b, 0x41}); err != nil { // up arrow
		t.Fatalf("input: %v", err)
	}
	if len(ch.sent) != 1 || string(ch.sent[0]) != "\x1b[A" {
		t.Fatalf("sent = %v, want the up-arrow bytes", ch.sent)
	}
}

func TestPTYBrokerCloseEOF(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	br.close()
	if _, err := nextWithin(t, a, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("NextEvent after close = %v, want io.EOF", err)
	}
}

func TestPTYBrokerEvictionFastForwards(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	br.maxBytes = 4 // tiny ring
	// Subscribe but do not read, then overflow the ring so its tail is evicted.
	b, err := br.subscribe(1) // a stale cursor into the soon-evicted region
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch.emit(t, []byte("abcdefgh"))
	// The ring retains only the last 4 bytes; the cursor fast-forwards past the
	// lost gap to the oldest retained byte rather than replaying evicted data.
	ev, err := nextWithin(t, b, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev.Kind != PTYData || string(ev.Data) != "efgh" {
		t.Fatalf("event = %+v, want the retained tail %q", ev, "efgh")
	}
}

// TestPTYBrokerTeardownDoesNotClobberReconnect is the #1661 regression: the
// clientless channel drives a SINGLE pane pipe, so the last-subscriber teardown
// (StopCapture → `pipe-pane`-disable) must not run AFTER a new subscriber has
// brought the capture back up — that ordering would disable the fresh pipe and
// leave the reconnected subscriber (e.g. an embedded pane re-bound after a
// full-screen attach+detach) with a live ring but no bytes ever arriving.
//
// The test pins the hostile interleaving deterministically: subscriber A leaves
// and its teardown parks inside StopCapture; subscriber B reconnects while the
// teardown is mid-flight; then the teardown is released. B must end up on a LIVE
// capture and receive freshly emitted output.
func TestPTYBrokerTeardownDoesNotClobberReconnect(t *testing.T) {
	ch := &fakeClientlessChannel{
		stopEntered: make(chan struct{}, 1),
		stopRelease: make(chan struct{}),
		stopDone:    make(chan struct{}, 1),
	}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0) // brings the capture up (StartCapture #1)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}

	// A leaves: its last-subscriber teardown parks inside the gated StopCapture.
	go func() { _ = a.Close() }()
	select {
	case <-ch.stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown never reached StopCapture")
	}

	// B reconnects while the teardown is parked. With the fix this blocks on the
	// capture reconcile until the teardown finishes and then starts a FRESH
	// capture; without it, B starts a capture the parked teardown then disables.
	type subResult struct {
		sub *ptySub
		err error
	}
	bres := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(0)
		bres <- subResult{s, e}
	}()

	// Give B time to reach the capture reconcile (blocked) or, in the buggy path,
	// to start its capture before we release the teardown.
	time.Sleep(50 * time.Millisecond)
	close(ch.stopRelease)

	var b subResult
	select {
	case b = <-bres:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnecting subscribe B never returned")
	}
	if b.err != nil {
		t.Fatalf("subscribe B: %v", b.err)
	}

	// Order the assertion strictly AFTER the parked teardown's writer-close so the
	// emit below cannot race ahead of a clobber and pass spuriously.
	select {
	case <-ch.stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown never completed")
	}

	// The reconnected subscriber must be on a LIVE pipe: a freshly emitted byte
	// reaches it. A clobbered pipe surfaces as a write error here (#1661).
	if err := ch.emitErr([]byte("LIVE-AFTER-RECONNECT")); err != nil {
		t.Fatalf("capture pipe was torn down under the reconnected subscriber (#1661): %v", err)
	}
	mustData(t, b.sub, "LIVE-AFTER-RECONNECT")
}

func TestPTYBrokerStopsCaptureWhenLastSubscriberLeaves(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	b, _ := br.subscribe(0)
	_ = a.Close()
	if ch.stops != 0 {
		t.Fatalf("StopCapture after 1 of 2 leaves = %d, want 0 (capture stays up)", ch.stops)
	}
	_ = b.Close()
	if ch.stops != 1 {
		t.Fatalf("StopCapture after last leaves = %d, want 1", ch.stops)
	}
	// A later subscribe restarts the capture (lazy, again).
	if _, err := br.subscribe(0); err != nil {
		t.Fatalf("re-subscribe: %v", err)
	}
	if ch.starts != 2 {
		t.Fatalf("StartCapture calls = %d, want 2 (restarted)", ch.starts)
	}
}
