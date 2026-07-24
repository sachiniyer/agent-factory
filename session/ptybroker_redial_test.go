package session

import (
	"testing"
	"time"
)

// waitForStarts blocks until the channel has been dialled `want` times, or fails.
// It exists because the re-dial is driven by the readLoop's exit — asynchronous
// with respect to the test — rather than by a call the test makes.
func waitForStarts(t *testing.T, ch *singleSocketChannel, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		starts, stops := ch.counts()
		if starts >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker never re-dialled on its own: StartCapture calls = %d, StopCapture = %d, "+
				"want %d starts within %s.\n\n"+
				"With one subscriber attached and its daemon-browser WS healthy, NOTHING ever "+
				"re-subscribes — the daemon pings and the browser pongs — so a recovery reachable "+
				"only from subscribe() leaves the pane dead until the user refreshes. #2447 cured "+
				"\"a refresh doesn't help\"; it did not cure \"it froze\" (#2450).",
				starts, stops, want, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPTYBrokerReDialsWithoutANewSubscribe is #2450 item 2.
//
// #2447 made a dead capture recoverable, but only from subscribe(): the recovery
// is reached from ensureCaptureStarted, and ensureCaptureStarted is reached from
// exactly two places — subscribe and resetCapture. resetCapture is the LOCAL
// respawn hook (agentserver_local.go); the remote runtime has no equivalent. So
// with a single client attached over a healthy daemon-browser WebSocket, nothing
// ever re-subscribes and the capture stays dead indefinitely.
//
// The broker must recover from the readLoop's own exit instead, so the pane comes
// back without the user refreshing.
func TestPTYBrokerReDialsWithoutANewSubscribe(t *testing.T) {
	defer setRedialTimingForTest(time.Millisecond)()

	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)
	defer br.close()

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")
	ch.emit(t, []byte("before-drop"))
	mustData(t, a, "before-drop")

	// The WebSocket dies under the live capture. A stays attached and parked in
	// NextEvent; the sandbox is still healthy over REST, so nothing marks the
	// session Lost and no external hook fires.
	ch.snapshot = []byte("RECOVERED")
	ch.dropUpstream()

	// No new subscribe anywhere in this test. That is the whole point.
	waitForStarts(t, ch, 2, 3*time.Second)

	if _, stops := ch.counts(); stops < 1 {
		t.Fatalf("StopCapture calls = %d, want at least 1: the dead socket must be RELEASED, not "+
			"merely forgotten — remoteClientlessChannel.StartCapture refuses while it still holds "+
			"one", stops)
	}

	// The already-attached subscriber resumes on its own: a repaint of the
	// recovered screen first (#2450 item 1 — without it A's emulator is silently
	// desynced across the outage), then live bytes.
	mustRepaintContains(t, a, "RECOVERED")
	ch.emit(t, []byte("after-redial"))
	mustData(t, a, "after-redial")
}

// TestPTYBrokerReDialIsBoundedWhenUpstreamKeepsDying is the storm guard.
//
// A re-dial driven by the readLoop's exit feeds back on itself: an endpoint that
// accepts a socket and immediately drops it produces death -> re-dial -> death,
// as fast as the loop can turn. The web client already backs off exponentially
// (web/src/terminal.ts), but the daemon side had none, so this is the bound the
// fix has to supply.
//
// The assertion is a RATE CEILING derived from the configured backoff, not a
// hand-picked count: the loop sleeps at least redialInitialBackoff before every
// dial, so over `span` it cannot exceed span/initial, plus slack for scheduling.
// An earlier version of this test compared two windows for "acceleration" and
// was vacuous — an unbounded retry spins at a constant, enormous rate rather
// than an accelerating one, so it passed with the backoff deleted. Verified this
// version fails with redialDelay stubbed to zero.
func TestPTYBrokerReDialIsBoundedWhenUpstreamKeepsDying(t *testing.T) {
	const unit = 20 * time.Millisecond
	defer setRedialTimingForTest(unit)()

	ch := &singleSocketChannel{snapshot: []byte("SCREEN"), dieOnStart: true}
	br := newPTYBroker(ch)
	defer br.close()

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	defer func() { _ = a.Close() }()

	const span = 600 * time.Millisecond
	time.Sleep(span)
	starts, _ := ch.counts()

	// The floor: the recovery must actually be running, or the ceiling below is
	// satisfied by doing nothing.
	if starts < 2 {
		t.Fatalf("StartCapture calls = %d after %s, want at least 2: the readLoop-driven recovery "+
			"never ran, so this test's ceiling proves nothing", starts, span)
	}
	// The ceiling. Every dial is preceded by at least redialInitialBackoff, and
	// most by more (the ladder doubles to 4*unit here), so this is generous.
	ceiling := int(span/unit) + 4
	if starts > ceiling {
		t.Fatalf("StartCapture calls = %d in %s, want at most %d.\n\n"+
			"Each re-dial must wait at least %s, doubling to %s. An unbounded retry dials as "+
			"fast as the loop turns, which means the daemon hammers a down sandbox on the "+
			"user's behalf with nobody watching (#2450).",
			starts, span, ceiling, redialInitialBackoff, redialMaxBackoff)
	}
}

// TestPTYBrokerReDialKeepsTryingUntilTheEndpointComesBack is the other half of
// "recovers instead of staying dead": the interesting outage is not one where
// the sandbox is ready the instant we notice, it is one where the dial keeps
// failing for a while.
//
// A recovery that gave up after its first failed dial would leave the pane dead
// exactly when the user is waiting — a sandbox restarting, a tunnel
// re-establishing. The loop must keep climbing its ladder until the endpoint
// answers, and then deliver a working stream to the subscriber that never left.
func TestPTYBrokerReDialKeepsTryingUntilTheEndpointComesBack(t *testing.T) {
	defer setRedialTimingForTest(2 * time.Millisecond)()

	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)
	defer br.close()

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")

	// The endpoint goes away entirely: the socket drops AND the next three dials
	// are refused.
	ch.mu.Lock()
	ch.failStarts = 3
	ch.snapshot = []byte("RECOVERED")
	ch.mu.Unlock()
	ch.dropUpstream()

	// 1 initial + 3 refused + 1 that succeeds.
	waitForStarts(t, ch, 5, 3*time.Second)

	mustRepaintContains(t, a, "RECOVERED")
	ch.emit(t, []byte("back-online"))
	mustData(t, a, "back-online")
}

// TestPTYBrokerReDialStopsWhenTheLastSubscriberLeaves pins the lazy lifecycle
// against the new self-driving recovery: with nobody attached there is no pane to
// keep alive, so the recovery must NOT hold a capture open. Otherwise a broker
// whose last client detached would keep a socket dialled against the sandbox
// forever, which is the resource leak the lazy start/stop exists to avoid.
func TestPTYBrokerReDialStopsWhenTheLastSubscriberLeaves(t *testing.T) {
	defer setRedialTimingForTest(time.Millisecond)()

	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)
	defer br.close()

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")

	// Last subscriber leaves, THEN the upstream dies. maybeStopCapture has already
	// torn the capture down, so there is nothing to recover and nobody to recover
	// for.
	if err := a.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}
	ch.dropUpstream()

	starts, _ := ch.counts()
	time.Sleep(100 * time.Millisecond)
	after, _ := ch.counts()
	if after != starts {
		t.Fatalf("StartCapture calls went %d -> %d with NO subscriber attached; the recovery must "+
			"leave the lazy lifecycle alone and let the next subscribe bring the capture up",
			starts, after)
	}
}

// TestPTYBrokerReDialDoesNotResurrectAClosedBroker is the #1632 lock, applied to
// the new entry point. A readLoop that exits BECAUSE the broker was shut down
// must not hand off to a recovery that dials a fresh socket into a torn-down
// sandbox — nothing would ever close it.
func TestPTYBrokerReDialDoesNotResurrectAClosedBroker(t *testing.T) {
	defer setRedialTimingForTest(time.Millisecond)()

	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")

	// close() tears the capture down, which ends the readLoop. That exit must be
	// recognised as a teardown, not an upstream death.
	br.close()

	starts, _ := ch.counts()
	time.Sleep(100 * time.Millisecond)
	after, _ := ch.counts()
	if after != starts {
		t.Fatalf("StartCapture calls went %d -> %d after the broker was closed; a closed broker "+
			"must never re-dial (#1632)", starts, after)
	}
}
