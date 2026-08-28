package session

import (
	"strings"
	"testing"
	"time"
)

// TestPTYBrokerSubscribeDuringRecoveryRace reproduces the race where a subscriber
// attaches during recovery's blocking stop() call: it computes needRepaint against
// the PRE-recovery base, blocks on captureMu (held by recovery), and by the time it
// unblocks recovery has advanced base past its cursor. The subscriber's cursor is
// now below base, but its stale needRepaint=false decision means it receives only a
// PTYCursor jump instead of a PTYRepaint — its terminal shows the stale pre-death
// screen until unrelated later output arrives.
//
// The fix re-checks needRepaint AFTER ensureCaptureStarted() unblocks, so the
// decision uses the post-recovery base and the subscriber is correctly repainted.
//
// Synchronization is deterministic via the existing stopEntered/stopRelease hooks
// (see TestPTYBrokerTeardownDoesNotClobberReconnect): recovery parks inside
// StopCapture, the test starts B's subscribe while it is parked, then releases
// recovery. The 50ms sleep is a safety margin for B to reach the captureMu block,
// not the synchronization mechanism — the same pattern the #1661 regression test
// uses.
func TestPTYBrokerSubscribeDuringRecoveryRace(t *testing.T) {
	ch := &fakeClientlessChannel{
		snapshot:    []byte("SCREEN-BEFORE-DEATH"),
		stopEntered: make(chan struct{}, 1),
		stopRelease: make(chan struct{}),
	}
	br := newPTYBroker(ch)

	// A connects before tmux dies: brings the capture up and consumes the initial
	// repaint plus some output so the ring is non-empty (head > base). This gives B a
	// `since` that is valid under the pre-recovery base (since >= base, since < head,
	// since != 0) but will be BELOW the post-recovery base (recovery sets base = head).
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")

	const preDeath = "PRE-DEATH-OUTPUT" // 16 bytes
	ch.emit(t, []byte(preDeath))
	mustData(t, a, preDeath)
	// Ring: base=0, head=16, A's cursor=16. B will subscribe at since=5, strictly
	// inside [0, 16) — valid now, but below the post-recovery base (16).

	// The recovered pane shows a new screen.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()

	// Start recovery in a goroutine; it parks inside the gated StopCapture.
	recoveryDone := make(chan struct{})
	go func() {
		br.resetCapture()
		close(recoveryDone)
	}()
	select {
	case <-ch.stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery never reached StopCapture")
	}

	// B subscribes while recovery is parked in stop(). B's `since` is valid under the
	// pre-recovery base, so subscribe() computes needRepaint=false, registers, releases
	// b.mu, and then blocks in ensureCaptureStarted() on captureMu (held by recovery).
	type subResult struct {
		sub *ptySub
		err error
	}
	bres := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(5)
		bres <- subResult{s, e}
	}()

	// Safety margin for B to reach the captureMu block. Not the synchronization
	// mechanism — stopEntered/stopRelease is. Mirrors the pattern in
	// TestPTYBrokerTeardownDoesNotClobberReconnect.
	time.Sleep(50 * time.Millisecond)

	// Release recovery: stop() returns, the ring is discarded (base → head=16), and
	// the capture is restarted. B's cursor (5) is now below the new base (16).
	close(ch.stopRelease)

	select {
	case <-recoveryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery never completed")
	}

	var b subResult
	select {
	case b = <-bres:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe B never returned")
	}
	if b.err != nil {
		t.Fatalf("subscribe B: %v", b.err)
	}

	// B's FIRST event must be a PTYRepaint of the recovered screen — NOT a bare
	// PTYCursor. Without the fix, B's needRepaint was computed (false) before recovery
	// advanced base, so B gets only a cursor jump and its terminal shows the stale
	// pre-death screen.
	ev, err := nextWithin(t, b.sub, 2*time.Second)
	if err != nil {
		t.Fatalf("B NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint {
		t.Fatalf("B first event = Kind %d, want PTYRepaint (the recovered screen): "+
			"a subscriber whose cursor was invalidated by recovery's ring discard "+
			"must be repainted, not just cursor-jumped", ev.Kind)
	}
	if !strings.Contains(string(ev.Data), "SCREEN-AFTER-RECOVERY") {
		t.Fatalf("B repaint = %q, want the recovered screen content", ev.Data)
	}

	// No spurious PTYCursor after the repaint. With the fix, B's cursor was moved to
	// the live tail when the repaint decision was re-evaluated, so there is no gap to
	// announce. A PTYCursor here would mean the cursor was left below base, defeating
	// the purpose of the repaint (the client's stale cursor would re-request
	// already-discarded bytes on reconnect).
	if ev, err := nextWithin(t, b.sub, 250*time.Millisecond); err == nil {
		if ev.Kind == PTYCursor {
			t.Fatalf("B got a spurious PTYCursor after the repaint (Seq=%d): cursor "+
				"was not moved to the live tail, so the subscriber still sees a stale gap",
				ev.Seq)
		}
	}

	// B resumes live output from the re-spawned pane — no regression.
	ch.emit(t, []byte("post-recovery-live"))
	mustData(t, b.sub, "post-recovery-live")
}

// TestPTYBrokerSubscribeDuringRecoveryFreshSubscriber verifies the fix does not
// regress the since==0 (fresh subscriber) path when a subscribe races recovery.
// A fresh subscriber is owed a repaint regardless of base (since==0), so this is
// the non-bug path — but it exercises the same re-check to confirm the cursor is
// moved to the post-recovery tail and live output still streams.
func TestPTYBrokerSubscribeDuringRecoveryFreshSubscriber(t *testing.T) {
	ch := &fakeClientlessChannel{
		snapshot:    []byte("SCREEN-BEFORE-DEATH"),
		stopEntered: make(chan struct{}, 1),
		stopRelease: make(chan struct{}),
	}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")
	ch.emit(t, []byte("pre-death"))
	mustData(t, a, "pre-death")

	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()

	recoveryDone := make(chan struct{})
	go func() {
		br.resetCapture()
		close(recoveryDone)
	}()
	select {
	case <-ch.stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery never reached StopCapture")
	}

	type subResult struct {
		sub *ptySub
		err error
	}
	bres := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(0)
		bres <- subResult{s, e}
	}()
	time.Sleep(50 * time.Millisecond)
	close(ch.stopRelease)

	select {
	case <-recoveryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery never completed")
	}

	var b subResult
	select {
	case b = <-bres:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe B never returned")
	}
	if b.err != nil {
		t.Fatalf("subscribe B: %v", b.err)
	}

	// B gets the recovered screen repaint.
	ev, err := nextWithin(t, b.sub, 2*time.Second)
	if err != nil {
		t.Fatalf("B NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint || !strings.Contains(string(ev.Data), "SCREEN-AFTER-RECOVERY") {
		t.Fatalf("B first event = %+v, want PTYRepaint of the recovered screen", ev)
	}

	// B streams live output from the re-spawned pane.
	ch.emit(t, []byte("live-after-recovery"))
	mustData(t, b.sub, "live-after-recovery")
}
