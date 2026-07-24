package session

import (
	"sync"
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
	ch.mu.Lock()
	ch.snapshot = []byte("RECOVERED")
	ch.mu.Unlock()
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

// TestPTYBrokerReDialSurvivesAFlapWhileTheLoopIsExiting is the lost-wakeup
// regression.
//
// redialLoop stops driving once it sees a healthy capture. If the flag that says
// "a recovery goroutine exists" is cleared in a DEFERRED section rather than in
// the same critical section as that decision, there is a window between the two
// where the broker claims a driver it no longer has:
//
//  1. the loop sees the capture it just re-dialled as healthy, commits to
//     returning, and releases mu;
//  2. that capture's upstream dies. Its readLoop hand-off takes mu, finds
//     `redialing` still true, and declines to spawn a replacement;
//  3. the deferred clear runs.
//
// The broker is then capturing=true, captureEnded=true, redialing=false, with a
// subscriber attached and NOBODY driving recovery — the #2450 freeze, restored
// by the fix for #2450.
//
// The window is a few instructions wide, so this drives it exactly through
// redialLoopExitHook rather than hoping a stress loop lands in it. -race is no
// substitute: every access is already under mu, so this is a missed signal
// rather than a data race, and the detector is silent either way.
func TestPTYBrokerReDialSurvivesAFlapWhileTheLoopIsExiting(t *testing.T) {
	defer setRedialTimingForTest(2 * time.Millisecond)()

	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)
	defer br.close()

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")

	// Fire once, on the loop's way out, after it has decided the re-dialled
	// capture is healthy. That is the window.
	var once sync.Once
	defer setRedialLoopExitHookForTest(func() {
		once.Do(func() {
			ch.dropUpstream()
			// Give the dying readLoop's hand-off time to take mu and make its
			// spawn decision while this loop is still returning.
			time.Sleep(20 * time.Millisecond)
		})
	})()

	// First death: spawns the loop, which re-dials, sees the new capture healthy,
	// and exits — running the hook, which kills that capture inside the window.
	ch.dropUpstream()

	// 1 initial + 1 re-dial + 1 more after the flap. Reaching 3 is the whole
	// assertion: it means SOMETHING was still driving recovery after the flap.
	waitForStarts(t, ch, 3, 3*time.Second)

	// And the attached subscriber is actually live again, not merely re-dialled.
	ch.emit(t, []byte("after-flap"))
	for {
		ev, err := nextWithin(t, a, 2*time.Second)
		if err != nil {
			t.Fatalf("NextEvent after the flap: %v", err)
		}
		if ev.Kind == PTYData && string(ev.Data) == "after-flap" {
			break
		}
	}
}

// TestPTYBrokerReDialStopsWhenTheLastSubscriberLeaves pins the lazy lifecycle
// against the new self-driving recovery: with nobody attached there is no pane to
// keep alive, so the recovery must NOT leave a capture open. Otherwise a broker
// whose last client detached keeps a socket dialled against the sandbox forever,
// which is the resource leak the lazy start/stop exists to avoid.
//
// The subscriber detaches while the re-dial is IN FLIGHT, which is the window
// that actually leaks. An earlier version of this test closed the subscriber
// BEFORE the drop, so redialLoop never started — it passed with the entire
// hand-off disabled and proved nothing. Reported in review; this is the rewrite.
//
// The leak it now covers: recoverCapture reads the subscriber count, then dials,
// and only sets capturing=true once StartCapture returns. remove() used to gate
// its teardown on `capturing`, so a departure during the dial saw capturing=false,
// skipped maybeStopCapture, and let the completing dial install a capture with
// zero subscribers and nothing to stop it. Rare while recovery was user-driven and
// local; routine once a background timer dials across a remote WS handshake.
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

	// Hold the NEXT dial open, so the detach below lands mid-handshake.
	gate := make(chan struct{})
	entered := make(chan struct{})
	ch.mu.Lock()
	ch.startGate, ch.gateEntered = gate, entered
	ch.mu.Unlock()

	// Release the gate on EVERY exit. Registered after the deferred br.close(), so
	// it runs before it: a t.Fatal below would otherwise leave the dial parked
	// holding captureMu, and close() would deadlock on it instead of reporting the
	// real failure.
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
	defer releaseGate()

	ch.dropUpstream()

	// Wait for the dial to reach its gate. Not `starts >= 2`: that counter is
	// incremented on the far side of the gate, so waiting on it would wait for the
	// thing being held.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-dial never reached StartCapture, so the in-flight window was never entered")
	}

	// Detach CONCURRENTLY. Close blocks: remove() hands off to maybeStopCapture,
	// which waits for captureMu — held by the dial we are gating. That is the real
	// shape (a detach during a slow handshake genuinely waits), so the test has to
	// let it happen rather than serialise it and deadlock itself.
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()

	// Give Close time to reach maybeStopCapture and park on captureMu, so the
	// teardown is genuinely pending when the dial lands.
	time.Sleep(50 * time.Millisecond)
	releaseGate() // let the dial complete, with nobody attached

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close A: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closing the last subscriber never returned — the teardown is wedged behind the dial")
	}

	// The capture that dial installed must be released, not orphaned.
	deadline := time.Now().Add(2 * time.Second)
	for ch.isHeld() {
		if time.Now().After(deadline) {
			starts, stops := ch.counts()
			t.Fatalf("a capture is still held with NO subscriber attached: starts=%d stops=%d.\n\n"+
				"The last subscriber left while the re-dial was in flight, so the teardown was "+
				"skipped and the completing dial installed a socket nothing will ever close — a "+
				"leaked capture against the sandbox for the life of the broker.", starts, stops)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRedialDelayClimbsAndCaps locks the ladder SHAPE, which the rate-ceiling
// test above cannot: a flat backoff pinned at redialInitialBackoff yields 30
// dials against its ceiling of 34, so that test passes with the doubling
// removed. This one is pure arithmetic — no timing, no flakiness.
func TestRedialDelayClimbsAndCaps(t *testing.T) {
	defer setRedialTimingForTest(10 * time.Millisecond)()

	if got, want := redialDelay(0), redialInitialBackoff; got != want {
		t.Errorf("redialDelay(0) = %s, want %s (the first retry waits the initial rung)", got, want)
	}
	if got, want := redialDelay(1), 2*redialInitialBackoff; got != want {
		t.Errorf("redialDelay(1) = %s, want %s — the ladder must DOUBLE, not stay flat", got, want)
	}
	if got, want := redialDelay(2), 4*redialInitialBackoff; got != want {
		t.Errorf("redialDelay(2) = %s, want %s", got, want)
	}
	// And it must saturate rather than grow without bound.
	for _, attempt := range []int{3, 10, 1000} {
		if got := redialDelay(attempt); got != redialMaxBackoff {
			t.Errorf("redialDelay(%d) = %s, want the cap %s", attempt, got, redialMaxBackoff)
		}
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
