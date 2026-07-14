package session

import (
	"strings"
	"testing"
	"time"
)

// mustRepaintContains blocks for the next event and asserts it is a PTYRepaint whose
// bytes contain want (the recovered screen re-seed).
func mustRepaintContains(t *testing.T, sub PTYSubscription, want string) {
	t.Helper()
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent (want repaint %q): %v", want, err)
	}
	if ev.Kind != PTYRepaint || !strings.Contains(string(ev.Data), want) {
		t.Fatalf("event = %+v, want PTYRepaint containing %q", ev, want)
	}
}

// TestPTYBrokerRecoveryResumesExistingSubscriber is the #1682 residual (T-Rex
// reproduced): a subscriber that was ALREADY connected when tmux died must resume
// seeing output on its own after recovery — repaint the recovered screen and stream
// live bytes — WITHOUT a second Subscribe. resetCapture (the recovery hook) stops the
// stale capture, then — because a subscriber is still attached — restarts the capture
// against the re-spawned pane and re-seeds the subscriber.
//
// Fail-before/pass-after: a stop-only resetCapture (the first-cut fix) would leave the
// already-attached subscriber with capturing=false and no restart, so it hangs until
// an unrelated later Subscribe brings the capture back up — exactly the residual. The
// asserts below (fresh StartCapture #2 driven by the reset itself, repaint of the
// recovered screen, then live output) all fail on that path.
func TestPTYBrokerRecoveryResumesExistingSubscriber(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-BEFORE-DEATH")}
	br := newPTYBroker(ch)

	// A connects before tmux dies: StartCapture #1, initial repaint of the pre-death
	// screen, then a live byte. Consume both so the ring/cursor are at the live tail.
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")
	ch.emit(t, []byte("pre-death-output"))
	mustData(t, a, "pre-death-output")
	if ch.starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 before recovery", ch.starts)
	}

	// tmux dies and the daemon re-spawns it. The recovered pane shows a new screen.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()

	// Recovery hook: reset the stale broker capture. Because A is still attached this
	// must restart the capture AND re-seed A — with no second Subscribe.
	br.resetCapture()

	if ch.stops != 1 {
		t.Fatalf("StopCapture calls = %d, want 1 (stale capture torn down + parked readLoop joined)", ch.stops)
	}
	if ch.starts != 2 {
		t.Fatalf("StartCapture calls = %d, want 2 (capture restarted by the reset itself, NOT waiting for a new Subscribe — the #1682 residual)", ch.starts)
	}

	// A resumes on its own: a repaint of the RECOVERED screen, then live output from
	// the re-spawned pane — without ever issuing a second Subscribe.
	mustRepaintContains(t, a, "SCREEN-AFTER-RECOVERY")
	ch.emit(t, []byte("post-recovery-output"))
	mustData(t, a, "post-recovery-output")
}

// TestPTYBrokerRecoveryNewSubscriberAlsoStreams pins that a NEW subscriber connecting
// after recovery streams too: the reset restarted the capture, so the fresh subscribe
// short-circuits (no third StartCapture) and rides the live capture.
func TestPTYBrokerRecoveryNewSubscriberAlsoStreams(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("S")}
	br := newPTYBroker(ch)
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "S")

	br.resetCapture() // A still attached → restart (StartCapture #2)
	if ch.starts != 2 {
		t.Fatalf("StartCapture after reset = %d, want 2", ch.starts)
	}

	mustRepaintContains(t, a, "S") // A's recovery re-seed repaint

	b, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe B after recovery: %v", err)
	}
	if ch.starts != 2 {
		t.Fatalf("StartCapture after new subscribe = %d, want 2 (capture already live, no restart)", ch.starts)
	}
	mustRepaintContains(t, b, "S") // B's initial-subscribe repaint
	ch.emit(t, []byte("live-for-both"))
	mustData(t, a, "live-for-both")
	mustData(t, b, "live-for-both")
}

// TestPTYBrokerResetCaptureNoopWhenIdle pins that a reset is harmless when no
// subscriber was attached at recovery time (a session nobody was streaming): no
// capture is started or stopped, and the lazy lifecycle is untouched.
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
// agentserver seam: resetBrokerCaptures snapshots the tab-broker map and recovers
// each broker's capture (#1682), restarting + re-seeding brokers with live
// subscribers, and is a no-op once Kill has latched closed.
func TestLocalAgentServerResetBrokerCaptures(t *testing.T) {
	ch0 := &fakeClientlessChannel{snapshot: []byte("tab0-screen")}
	ch1 := &fakeClientlessChannel{snapshot: []byte("tab1-screen")}
	br0 := newPTYBroker(ch0)
	br1 := newPTYBroker(ch1)
	a0, err := br0.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe tab 0: %v", err)
	}
	mustRepaintContains(t, a0, "tab0-screen")
	a1, err := br1.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe tab 1: %v", err)
	}
	mustRepaintContains(t, a1, "tab1-screen")

	s := &localAgentServer{brokers: map[string]*ptyBroker{"tab0": br0, "tab1": br1}}
	s.resetBrokerCaptures()

	for name, ch := range map[string]*fakeClientlessChannel{"tab0": ch0, "tab1": ch1} {
		if ch.stops != 1 {
			t.Fatalf("%s: StopCapture calls = %d, want 1 (stale capture reset)", name, ch.stops)
		}
		if ch.starts != 2 {
			t.Fatalf("%s: StartCapture calls = %d, want 2 (capture restarted for the attached subscriber)", name, ch.starts)
		}
	}

	// Each still-attached subscriber resumed on its own: a recovered-screen repaint,
	// then live output — no new Subscribe.
	mustRepaintContains(t, a0, "tab0-screen")
	ch0.emit(t, []byte("tab0-live"))
	mustData(t, a0, "tab0-live")
	mustRepaintContains(t, a1, "tab1-screen")
	ch1.emit(t, []byte("tab1-live"))
	mustData(t, a1, "tab1-live")

	// Once closed (Kill), reset is a no-op — it must not resurrect capture on a
	// session being torn down.
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	prevStarts, prevStops := ch1.starts, ch1.stops
	s.resetBrokerCaptures()
	if ch1.starts != prevStarts || ch1.stops != prevStops {
		t.Fatalf("resetBrokerCaptures after close touched a broker: starts=%d stops=%d, want %d/%d", ch1.starts, ch1.stops, prevStarts, prevStops)
	}
}

// TestPTYBrokerResetCaptureJoinsParkedReadLoop asserts resetCapture does not return
// until the stale readLoop has actually exited (no goroutine leak): the stopCapture
// closure blocks on the readLoop's done channel, so once resetCapture returns the old
// capture reader is fully drained before the fresh capture starts.
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
