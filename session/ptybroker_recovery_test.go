package session

import (
	"testing"
	"time"
)

// TestPTYBrokerResetCaptureRestartsAfterRecovery is the #1682 regression: after a
// running session's tmux dies and the daemon recovers it (respawn → Restore →
// ConfirmLive), the PTY broker's stale capture must be reset so a NEW subscriber
// gets live output from the re-spawned pane — instead of attaching to a broker that
// is still `capturing` over a readLoop parked forever on the dead pane's FIFO.
//
// The test drives the broker seam directly (reproducing a real tmux death in a unit
// test is impractical): a subscriber brings the capture up (readLoop parks on the
// fake pipe, exactly as the real readLoop parks on the O_RDWR FIFO), then the test
// asserts the pre-fix defect and the post-fix recovery around resetCapture().
func TestPTYBrokerResetCaptureRestartsAfterRecovery(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)

	// A subscriber brings the "gen-1" capture up: StartCapture #1, capturing=true, a
	// readLoop parked on the pipe reader (the fake's analogue of the real readLoop
	// parked on the O_RDWR FIFO that never sees EOF when pipe-pane's writer dies).
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	_ = a
	if ch.starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 after first subscribe", ch.starts)
	}

	// --- Prove the bug on the pre-fix path -------------------------------------
	// tmux has died; the daemon has re-spawned it. WITHOUT a capture reset the
	// broker stays capturing=true, so a fresh subscribe short-circuits
	// ensureCaptureStarted and NO new capture is bound to the recovered pane — the
	// post-recovery subscriber is stranded on the dead gen-1 capture (ch.starts
	// never advances past 1). This is the exact stale-broker defect #1682 reports.
	b, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe B (pre-reset): %v", err)
	}
	_ = b
	if ch.starts != 1 {
		t.Fatalf("pre-reset: StartCapture calls = %d, want 1 (no fresh capture on the recovered pane — the #1682 defect)", ch.starts)
	}

	// --- Recovery resets the stale capture -------------------------------------
	br.resetCapture()
	if ch.stops != 1 {
		t.Fatalf("resetCapture: StopCapture calls = %d, want 1 (stale capture torn down + parked readLoop joined)", ch.stops)
	}
	br.mu.Lock()
	capturing := br.capturing
	br.mu.Unlock()
	if capturing {
		t.Fatal("resetCapture: broker still capturing, want the latch cleared so the next subscribe restarts capture")
	}

	// --- Prove the fix: a NEW subscriber after recovery streams live output -----
	c, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe C (post-reset): %v", err)
	}
	if ch.starts != 2 {
		t.Fatalf("post-reset: StartCapture calls = %d, want 2 (a FRESH capture against the recovered pane)", ch.starts)
	}
	// gen-2 output from the re-spawned pane reaches the post-recovery subscriber.
	ch.emit(t, []byte("LIVE-AFTER-RECOVERY"))
	mustData(t, c, "LIVE-AFTER-RECOVERY")
}

// TestPTYBrokerResetCaptureNoopWhenIdle pins that a reset is harmless when no
// capture is running (a session nobody was streaming when it recovered): no
// StopCapture is issued and the lazy lifecycle is untouched.
func TestPTYBrokerResetCaptureNoopWhenIdle(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	br.resetCapture()
	if ch.starts != 0 || ch.stops != 0 {
		t.Fatalf("idle resetCapture touched the channel: starts=%d stops=%d, want 0/0", ch.starts, ch.stops)
	}
	// The broker still streams normally afterward.
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch.emit(t, []byte("hi"))
	mustData(t, a, "hi")
}

// TestLocalAgentServerResetBrokerCaptures exercises the recovery reset at the
// agentserver seam: resetBrokerCaptures snapshots the tab-broker map and resets
// each broker's stale capture (#1682), and is a no-op once Kill has latched closed.
func TestLocalAgentServerResetBrokerCaptures(t *testing.T) {
	ch0 := &fakeClientlessChannel{}
	ch1 := &fakeClientlessChannel{}
	br0 := newPTYBroker(ch0)
	br1 := newPTYBroker(ch1)
	if _, err := br0.subscribe(0); err != nil {
		t.Fatalf("subscribe tab 0: %v", err)
	}
	if _, err := br1.subscribe(0); err != nil {
		t.Fatalf("subscribe tab 1: %v", err)
	}

	s := &localAgentServer{brokers: map[int]*ptyBroker{0: br0, 1: br1}}
	s.resetBrokerCaptures()

	for name, ch := range map[string]*fakeClientlessChannel{"tab0": ch0, "tab1": ch1} {
		if ch.stops != 1 {
			t.Fatalf("%s: StopCapture calls = %d, want 1 (stale capture reset)", name, ch.stops)
		}
	}
	for name, br := range map[string]*ptyBroker{"tab0": br0, "tab1": br1} {
		br.mu.Lock()
		capturing := br.capturing
		br.mu.Unlock()
		if capturing {
			t.Fatalf("%s: still capturing after resetBrokerCaptures", name)
		}
	}

	// A fresh subscribe on each tab restarts capture against the recovered pane.
	c0, err := br0.subscribe(0)
	if err != nil {
		t.Fatalf("re-subscribe tab 0: %v", err)
	}
	if ch0.starts != 2 {
		t.Fatalf("tab0 StartCapture calls = %d, want 2 (restarted)", ch0.starts)
	}
	ch0.emit(t, []byte("tab0-live"))
	mustData(t, c0, "tab0-live")

	// Once closed (Kill), reset is a no-op — it must not resurrect capture on a
	// session being torn down.
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	prevStops := ch1.stops
	s.resetBrokerCaptures()
	if ch1.stops != prevStops {
		t.Fatalf("resetBrokerCaptures after close touched a broker: stops=%d, want %d", ch1.stops, prevStops)
	}
}

// TestPTYBrokerResetCaptureJoinsParkedReadLoop asserts resetCapture does not return
// until the stale readLoop has actually exited (no goroutine leak): the stopCapture
// closure blocks on the readLoop's done channel, so once resetCapture returns the
// old capture reader is fully drained and the next capture starts clean.
func TestPTYBrokerResetCaptureJoinsParkedReadLoop(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	if _, err := br.subscribe(0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	done := make(chan struct{})
	go func() {
		br.resetCapture()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resetCapture blocked — parked readLoop never joined (goroutine leak)")
	}
}
