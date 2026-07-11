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
}

func (f *fakeClientlessChannel) StartCapture() (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.starts++
	f.r, f.w = io.Pipe()
	return f.r, nil
}

func (f *fakeClientlessChannel) StopCapture() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.w != nil {
		_ = f.w.Close()
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
