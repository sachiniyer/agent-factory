package session

import (
	"errors"
	"io"
	"testing"
	"time"
)

// TestBareSessionStreamer_StreamsInputResize proves the streamer reuses the broker
// data plane over a bare session: the capture starts lazily on the first Subscribe,
// output bytes fan to the subscriber, and Input/Resize reach the clientless channel
// — with no Instance anywhere.
func TestBareSessionStreamer_StreamsInputResize(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("READY")}
	s := newBareSessionStreamerWithChannel(func() clientlessChannel { return ch })

	// Lazy: no capture until the first Subscribe.
	if got := captureStarts(ch); got != 0 {
		t.Fatalf("capture started before any Subscribe (starts=%d); it must be lazy", got)
	}

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got := captureStarts(ch); got != 1 {
		t.Fatalf("capture starts=%d after first Subscribe, want 1", got)
	}
	// First event is the fresh-subscriber repaint of the current screen.
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("initial NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint {
		t.Fatalf("first event = %+v, want the initial PTYRepaint", ev)
	}
	// Live output reaches the subscriber.
	ch.emit(t, []byte("hello"))
	mustData(t, sub, "hello")

	// Input is a clientless send-keys on the same channel.
	if err := s.Input([]byte("ls\n")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	if sent := lastSent(ch); sent != "ls\n" {
		t.Fatalf("channel recorded send %q, want %q", sent, "ls\n")
	}

	// Resize applies to the channel and echoes to the subscriber.
	if err := s.Resize(30, 90); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	ev, err = nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("resize NextEvent: %v", err)
	}
	if ev.Kind != PTYResize || ev.Rows != 30 || ev.Cols != 90 {
		t.Fatalf("post-resize event = %+v, want the 30x90 resize echo", ev)
	}
}

// TestBareSessionStreamer_CloseRefusesResubscribe pins the #1632 no-resurrect rule
// at the bare-session layer: Close ends every live subscriber (io.EOF) and stops the
// capture, and a Subscribe that races the reap is REFUSED rather than lazily
// building a fresh capture goroutine on a session being torn down.
func TestBareSessionStreamer_CloseRefusesResubscribe(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("READY")}
	s := newBareSessionStreamerWithChannel(func() clientlessChannel { return ch })

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Drain the initial repaint so the next event is the session-end.
	if _, err := nextWithin(t, sub, 2*time.Second); err != nil {
		t.Fatalf("initial NextEvent: %v", err)
	}

	s.Close()

	// The live subscriber sees the stream end.
	if _, err := nextWithin(t, sub, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("post-Close NextEvent err = %v, want io.EOF", err)
	}
	// The capture was torn down.
	if got := captureStops(ch); got == 0 {
		t.Fatal("Close did not stop the clientless capture")
	}
	// A Subscribe after Close is refused — never a resurrected capture (#1632).
	if _, err := s.Subscribe(0); err == nil {
		t.Fatal("Subscribe after Close returned nil error; it must refuse (no #1632 resurrection)")
	}
	// Close is idempotent.
	s.Close()
}

func captureStarts(ch *fakeClientlessChannel) int {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.starts
}

func captureStops(ch *fakeClientlessChannel) int {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.stops
}

func lastSent(ch *fakeClientlessChannel) string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.sent) == 0 {
		return ""
	}
	return string(ch.sent[len(ch.sent)-1])
}
