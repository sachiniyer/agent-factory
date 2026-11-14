package session

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// deafCaptureReader is a capture reader whose Read IGNORES Close: once parked,
// it returns only when the test releases it. The real FIFO reader no longer
// behaves like this — an abort Close wakes it even after a drain stop (#7302) —
// so this is the reader the bounded join exists for: one that ignores even the
// wake, whatever the reason.
type deafCaptureReader struct {
	entered   chan struct{} // closed when the first Read parks
	closed    chan struct{} // closed by Close, which does NOT end the Read
	release   chan []byte   // a send returns those bytes from Read; a close returns EOF
	returned  chan struct{} // closed when a Read returns io.EOF
	enterOnce sync.Once
	closeOnce sync.Once
}

func newDeafCaptureReader() *deafCaptureReader {
	return &deafCaptureReader{
		entered:  make(chan struct{}),
		closed:   make(chan struct{}),
		release:  make(chan []byte),
		returned: make(chan struct{}),
	}
}

func (r *deafCaptureReader) Read(p []byte) (int, error) {
	r.enterOnce.Do(func() { close(r.entered) })
	b, ok := <-r.release
	if !ok {
		close(r.returned)
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (r *deafCaptureReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

// deafFirstCaptureChannel hands its FIRST capture a deafCaptureReader and every
// later one an ordinary io.Pipe, so a test can wedge one capture's reader and
// then prove a reconnect still brings up a healthy one.
type deafFirstCaptureChannel struct {
	deaf        *deafCaptureReader
	releaseOnce sync.Once // guards close(deaf.release)

	mu     sync.Mutex
	starts int
	stops  int
	w      *io.PipeWriter // the current healthy capture's writer
}

func (c *deafFirstCaptureChannel) StartCapture() (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	if c.starts == 1 {
		return c.deaf, nil
	}
	r, w := io.Pipe()
	c.w = w
	return r, nil
}

func (c *deafFirstCaptureChannel) StopCapture() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops++
	if c.w != nil {
		_ = c.w.Close()
		c.w = nil
	}
	return nil
}

func (c *deafFirstCaptureChannel) emit(t *testing.T, b []byte) {
	t.Helper()
	c.mu.Lock()
	w := c.w
	c.mu.Unlock()
	if w == nil {
		t.Fatal("emit with no healthy capture running")
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

func (c *deafFirstCaptureChannel) startCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}

func (*deafFirstCaptureChannel) SendRaw([]byte) error            { return nil }
func (*deafFirstCaptureChannel) Resize(uint16, uint16) error     { return nil }
func (*deafFirstCaptureChannel) Snapshot() (PaneSnapshot, error) { return PaneSnapshot{}, nil }
func (*deafFirstCaptureChannel) PaneDead() bool                  { return false }

// wedgeFirstCapture subscribes once so the first capture's deaf reader parks,
// then closes that subscriber, which requests the background teardown (#4320).
func wedgeFirstCapture(t *testing.T) (*ptyBroker, *deafFirstCaptureChannel) {
	t.Helper()
	ch := &deafFirstCaptureChannel{deaf: newDeafCaptureReader()}
	br := newPTYBroker(ch)
	br.captureDrainGrace = 20 * time.Millisecond
	br.captureJoinTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		// Release a still-parked deaf Read BEFORE closing the broker, so the
		// readLoop does not outlive the test, and so a teardown that is wedged on
		// it (an unbounded join, the bug) frees captureMu for shutdown instead of
		// hanging the package.
		ch.releaseOnce.Do(func() { close(ch.deaf.release) })
		br.close()
	})

	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	select {
	case <-ch.deaf.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first capture's reader never parked in Read")
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("close subscription: %v", err)
	}
	select {
	case <-ch.deaf.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the teardown never closed the capture reader")
	}
	return br, ch
}

func abandonedCounts(br *ptyBroker) (abandoned, returned int) {
	br.mu.Lock()
	defer br.mu.Unlock()
	return br.abandonedReaders, br.abandonedReturned
}

// TestPTYBrokerCaptureStopIsBoundedWhenTheReaderIgnoresClose pins #7302's
// property at its narrowest: a capture stop returns within a bound whatever the
// reader does. The teardown that the last subscriber's departure requested holds
// captureMu while it joins the read loop. Before the bound, a reader that ignored
// Close held that lock forever, so any later capture transition — this second
// maybeStopCapture included — queued behind it for good.
func TestPTYBrokerCaptureStopIsBoundedWhenTheReaderIgnoresClose(t *testing.T) {
	br, _ := wedgeFirstCapture(t)

	returned := make(chan struct{})
	go func() {
		br.maybeStopCapture()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("maybeStopCapture is still waiting: the teardown is holding captureMu on a reader that ignores Close")
	}

	// Bounded means ABANDONED, not joined: the reader is still parked, so the
	// teardown gave up on it and said so.
	if abandoned, _ := abandonedCounts(br); abandoned != 1 {
		t.Fatalf("abandoned capture readers = %d, want 1", abandoned)
	}
}

// TestPTYBrokerReconnectAfterAnAbandonedReaderGetsAFreshCapture is the user-facing
// half of #7302: a subscriber reconnecting after the wedge must get a WORKING
// capture, not queue behind the parked one. Then the abandoned reader comes back
// to life. Whatever it returns belongs to a capture the broker has already
// replaced, so it must reach neither the ring nor the new capture's state. If its
// exit latched captureEnded, the next subscribe would tear down the healthy
// capture, and the capturing flag it would see is the new capture's — it would
// start a redial against a capture that is fine.
func TestPTYBrokerReconnectAfterAnAbandonedReaderGetsAFreshCapture(t *testing.T) {
	br, ch := wedgeFirstCapture(t)

	type subscribeResult struct {
		sub *ptySub
		err error
	}
	subscribed := make(chan subscribeResult, 1)
	go func() {
		sub, err := br.subscribe(0)
		subscribed <- subscribeResult{sub, err}
	}()
	var sub *ptySub
	select {
	case res := <-subscribed:
		if res.err != nil {
			t.Fatalf("reconnect subscribe: %v", res.err)
		}
		sub = res.sub
	case <-time.After(3 * time.Second):
		t.Fatal("the reconnect is still waiting: ensureCaptureStarted queued behind the wedged teardown")
	}
	defer sub.Close()
	if got := ch.startCount(); got != 2 {
		t.Fatalf("StartCapture calls = %d, want 2 (a fresh capture for the reconnect)", got)
	}

	// The fresh capture streams.
	ch.emit(t, []byte("FRESH-1"))
	expectData(t, sub, "FRESH-1")

	// The abandoned reader returns: first with bytes, then with EOF.
	ch.deaf.release <- []byte("STALE")
	ch.releaseOnce.Do(func() { close(ch.deaf.release) })
	<-ch.deaf.returned
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, returned := abandonedCounts(br); returned == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned read loop's return was never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}

	br.mu.Lock()
	ring := append([]byte(nil), br.buf...)
	capturing, ended, redialing := br.capturing, br.captureEnded, br.redialing
	br.mu.Unlock()
	if bytes.Contains(ring, []byte("STALE")) {
		t.Fatalf("the abandoned reader's bytes reached the ring: %q", ring)
	}
	if !capturing || ended {
		t.Fatalf("after the abandoned loop exited: capturing=%v captureEnded=%v, want the fresh capture still live (true, false)", capturing, ended)
	}
	if redialing {
		t.Fatal("the abandoned loop's exit started a redial against the healthy capture")
	}

	// And the fresh capture is still the one streaming.
	ch.emit(t, []byte("FRESH-2"))
	expectData(t, sub, "FRESH-2")
	if got := ch.startCount(); got != 2 {
		t.Fatalf("StartCapture calls = %d after the abandoned loop exited, want still 2", got)
	}
}

// expectData reads sub until a data event carrying want arrives. A repaint (the
// fresh subscriber's opening paint) is skipped; a data event that carries the
// abandoned reader's bytes fails the test.
func expectData(t *testing.T, sub *ptySub, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no data event carrying %q", want)
		}
		ev, err := nextWithin(t, sub, remaining)
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		if ev.Kind != PTYData {
			continue
		}
		if bytes.Contains(ev.Data, []byte("STALE")) {
			t.Fatalf("the subscriber received the abandoned reader's bytes: %q", ev.Data)
		}
		if bytes.Contains(ev.Data, []byte(want)) {
			return
		}
	}
}
