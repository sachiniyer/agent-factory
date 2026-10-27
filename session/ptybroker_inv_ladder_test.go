package session

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Regression coverage for the redial ladder reset on a span-surviving capture
// that is torn down by its last subscriber leaving (#2461).
//
// The survival reset (b.redialAttempts = 0 after redialHealthySpan) lived inside
// the `if spontaneous` guard of readLoop's defer, so it fired only when a
// span-surviving capture died on its own (upstream drop, capturing still true).
// A span-surviving capture torn down by the last subscriber leaving takes the
// OTHER exit: maybeStopCapture clears capturing under mu BEFORE stop() ends the
// readLoop, so the defer computes spontaneous == false and skipped the reset,
// leaving a stale non-zero ladder on the broker. On a broker that PERSISTS across
// the idle gap (remote runtime WS reconnect; local runtime WS blip without a tab
// close), the next incident's first automatic re-dial inherited that stale rung
// and waited redialDelay(N) instead of the initial redialDelay(0).
//
// The fix moves the survival reset out of the `if spontaneous` guard so it runs
// for BOTH exits. These two tests reproduce the persisting-broker path end to end
// against the singleSocketChannel fake (the remote clientlessChannel contract):
//
//   - TestPTYBrokerRedialLadderResetsWhenSpanSurvivorIsTornDown: the cap case,
//     climbed naturally to the ladder floor (rung >= 6), to show the full
//     storm -> survive -> teardown -> reopen sequence end to end.
//   - TestPTYBrokerRedialLadderInheritedAtEachRung: the reachable continuum
//     (rungs 1/2/3), seeded directly via the same-package field so the rung is
//     exact rather than storm-timing-dependent, to show the inheritance holds at
//     every rung, not just the cap.
//
// The test timing (setRedialTimingForTest with unit=5ms: initial=5ms, max=20ms,
// healthySpan=250ms) maps to production (initial=500ms, max=30s, healthySpan=60s)
// by the ladder shape. So a rung of 6 -> 20ms here == 30s in production, and a
// fresh incident's rung 0 -> 5ms here == 500ms in production; rungs 1/2/3 -> the
// reachable 1s/2s/4s continuum. The assertions below use the test delays; the
// production figures read off redialDelay with the production vars.

// redialAttemptsState returns the broker fields these tests assert against. All
// under one mu hold, so a poll sees a consistent snapshot.
func redialAttemptsState(b *ptyBroker) (capturing, ended, redialing bool, redialAttempts int, captureStarted time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capturing, b.captureEnded, b.redialing, b.redialAttempts, b.captureStarted
}

// waitForRedialAttemptsAtLeast blocks until redialAttempts climbs to at least
// `want` (a storm climbing the ladder), or fails. The counter is written under
// mu inside redialLoop, so reading it under mu in a poll is race-free.
func waitForRedialAttemptsAtLeast(t *testing.T, b *ptyBroker, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		_, _, _, ra, _ := redialAttemptsState(b)
		if ra >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("redialAttempts = %d, want >= %d within %s: the storm never climbed the "+
				"ladder — with dieOnStart, every re-dial dies immediately, so redialLoop should "+
				"keep incrementing redialAttempts once per iteration", ra, want, within)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForSurvivor blocks until a healthy, span-surviving capture is installed AND
// the storm's redialLoop has exited: {capturing && !captureEnded && !redialing},
// with captureStarted set. That is the state from which a teardown models a
// client-side WS drop on a broker that persists across the idle gap.
func waitForSurvivor(t *testing.T, b *ptyBroker, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		capturing, ended, redialing, _, started := redialAttemptsState(b)
		if capturing && !ended && !redialing && !started.IsZero() {
			return
		}
		if time.Now().After(deadline) {
			capturing, ended, redialing, ra, started := redialAttemptsState(b)
			t.Fatalf("never reached a span-surviving healthy capture within %s: "+
				"{capturing:%v captureEnded:%v redialing:%v redialAttempts:%d captureStarted:%v}",
				within, capturing, ended, redialing, ra, started)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForTeardownComplete blocks until the last-subscriber teardown has fully
// finished, including the readLoop's defer latching captureEnded. capturing is
// cleared by maybeStopCapture BEFORE stop() joins the readLoop, so observing
// !capturing alone could race the defer; !capturing && captureEnded means the
// defer (where the survival reset lives) has run.
func waitForTeardownComplete(t *testing.T, b *ptyBroker, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		capturing, ended, _, _, _ := redialAttemptsState(b)
		if !capturing && ended {
			return
		}
		if time.Now().After(deadline) {
			capturing, ended, _, ra, _ := redialAttemptsState(b)
			t.Fatalf("capture teardown never completed within %s: "+
				"{capturing:%v captureEnded:%v redialAttempts:%d}", within, capturing, ended, ra)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// firstRedialRungAfterReopen reopens br into a fresh flap — a capture that dies
// immediately — and returns the rung the new incident's first re-dial was
// scheduled at. It is the end-to-end impact check: a stale ladder inherited from a
// prior, already-resolved storm shows up here as a first re-dial at rung N instead
// of rung 0.
//
// Determinism uses the singleSocketChannel startGate. dieOnStart is left FALSE so
// subscribe's capture lives until we drop it, which lets the gate be armed AFTER
// subscribe (so subscribe's StartCapture does NOT consume it). The re-dial — the
// 2nd StartCapture of this incident — is what reaches the gate. redialLoop has
// already incremented redialAttempts once for its first iteration before that
// StartCapture runs, and it cannot run a second iteration while the gate holds
// the first (ensureCaptureStartedLocked holds captureMu across StartCapture), so
// the rung read at the gate is exact: first re-dial rung = redialAttempts - 1.
func firstRedialRungAfterReopen(t *testing.T, br *ptyBroker, ch *singleSocketChannel) int {
	t.Helper()
	gate := make(chan struct{})
	entered := make(chan struct{})
	var gateOnce sync.Once
	release := func() { gateOnce.Do(func() { close(gate) }) }
	defer release() // safety net: a fatal below must not strand redialLoop at the gate

	// dieOnStart stays false: subscribe's capture lives until dropUpstream, so the
	// gate armed next is consumed by the re-dial, not by subscribe.
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("reopen subscribe: %v", err)
	}
	defer func() { _ = a.Close() }()
	mustRepaintContains(t, a, "SCREEN")

	// Arm the gate for the re-dial (the 2nd StartCapture of this incident).
	ch.mu.Lock()
	ch.startGate, ch.gateEntered = gate, entered
	ch.mu.Unlock()

	// Kill the subscribe capture: a spontaneous death (capturing stays true) that
	// hands off to redialLoop. The capture just started, so it did NOT survive
	// redialHealthySpan, and readLoop's survival reset does NOT fire here — the new
	// incident's first re-dial reads the ladder exactly as the teardown left it.
	ch.dropUpstream()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the new incident's first re-dial never reached StartCapture — " +
			"redialLoop did not drive a re-dial after the spontaneous death")
	}

	_, _, _, ra, _ := redialAttemptsState(br)
	release() // let the held re-dial proceed
	return ra - 1
}

// TestPTYBrokerRedialLadderResetsWhenSpanSurvivorIsTornDown is the cap case, end
// to end. A storm climbs the ladder naturally to the cap floor (rung >= 6), the
// endpoint comes back and the capture survives redialHealthySpan, the last
// subscriber leaves (a client-side WS drop, NOT a tab close), and the SAME
// persisting broker is reopened into a fresh flap. The stale ladder must be
// cleared at the teardown and the new incident's first re-dial must start at
// rung 0.
func TestPTYBrokerRedialLadderResetsWhenSpanSurvivorIsTornDown(t *testing.T) {
	const unit = 5 * time.Millisecond // initial=5ms, max=20ms, healthySpan=250ms
	defer setRedialTimingForTest(unit)()

	ch := &singleSocketChannel{snapshot: []byte("SCREEN"), dieOnStart: true}
	br := newPTYBroker(ch)
	defer br.close()

	// 1. Storm: every re-dial dies immediately; redialLoop climbs the ladder.
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")
	waitForRedialAttemptsAtLeast(t, br, 6, 2*time.Second) // cap floor: redialDelay >= 6 saturates

	// 2. Resolve: a re-dial succeeds and the capture survives; redialLoop exits.
	ch.mu.Lock()
	ch.dieOnStart = false
	ch.mu.Unlock()
	waitForSurvivor(t, br, 2*time.Second)

	// 3. Survive past redialHealthySpan so the capture qualifies as a fresh
	//    incident — the precondition for the survival reset to fire.
	time.Sleep(redialHealthySpan + 30*time.Millisecond)

	// 4. Teardown via last-subscriber leave: models a client-side WS drop on a
	//    broker that PERSISTS (remote runtime reconnect; local WS blip without a
	//    tab close), NOT a local tab close that would retire the broker.
	if err := a.Close(); err != nil {
		t.Fatalf("close last subscriber: %v", err)
	}
	waitForTeardownComplete(t, br, 2*time.Second)

	// 5. Assert the accounting: the survival reset must have cleared the ladder.
	_, _, _, ra, _ := redialAttemptsState(br)
	if ra != 0 {
		t.Errorf("post-teardown redialAttempts = %d, want 0: the readLoop defer's survival "+
			"reset was skipped on a teardown (maybeStopCapture clears `capturing` before "+
			"stop() so `spontaneous` is false), leaving a stale ladder on a persisting broker",
			ra)
	}

	// 6. Reopen into a fresh flap on the SAME persisting broker and read the new
	//    incident's first re-dial rung. A fresh incident starts at rung 0.
	firstRung := firstRedialRungAfterReopen(t, br, ch)
	if firstRung != 0 {
		t.Errorf("new incident's first re-dial rung = %d (delay %s), want 0 (delay %s): "+
			"the stale post-teardown ladder was inherited by the next incident on the "+
			"persisting broker — production mapping: rung %d -> %s vs a fresh 500ms",
			firstRung, redialDelay(firstRung), redialDelay(0), firstRung, prodRedialDelay(firstRung))
	}
}

// TestPTYBrokerRedialLadderInheritedAtEachRung is the reachable continuum:
// rungs 1/2/3 (the production 1s/2s/4s first-re-dial, vs a fresh incident's
// 500ms). Rather than climb a storm (storm-timing-dependent), it seeds the
// ladder directly with the rung a storm would have left — a same-package seam —
// then runs the SAME span-survive -> teardown -> reopen sequence for each rung.
// The fix must clear the ladder at the teardown for every rung, so the new
// incident's first re-dial starts at rung 0 in every case.
func TestPTYBrokerRedialLadderInheritedAtEachRung(t *testing.T) {
	const unit = 5 * time.Millisecond // initial=5ms, max=20ms, healthySpan=250ms
	defer setRedialTimingForTest(unit)()

	for _, rung := range []int{1, 2, 3} {
		rung := rung
		t.Run(fmt.Sprintf("rung%d", rung), func(t *testing.T) {
			ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
			br := newPTYBroker(ch)
			defer br.close()

			// Install a capture, then seed the ladder with the rung a storm would
			// have left. The capture is healthy and redialLoop is NOT running, so the
			// write is race-free.
			a, err := br.subscribe(0)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			mustRepaintContains(t, a, "SCREEN")
			br.mu.Lock()
			br.redialAttempts = rung
			br.mu.Unlock()

			// Survive past redialHealthySpan so the capture qualifies as a fresh
			// incident — the precondition for the survival reset to fire.
			time.Sleep(redialHealthySpan + 30*time.Millisecond)

			// Teardown via last-subscriber leave (client-side WS drop, not tab close).
			if err := a.Close(); err != nil {
				t.Fatalf("close last subscriber: %v", err)
			}
			waitForTeardownComplete(t, br, 2*time.Second)

			// The ladder must be cleared at the teardown, regardless of the stale rung.
			_, _, _, ra, _ := redialAttemptsState(br)
			if ra != 0 {
				t.Errorf("post-teardown redialAttempts = %d, want 0: the readLoop defer's "+
					"survival reset was skipped on a teardown, so the stale ladder (rung %d) "+
					"persists on the persisting broker", ra, rung)
			}

			// The new incident's first re-dial must start at rung 0, not inherit rung.
			firstRung := firstRedialRungAfterReopen(t, br, ch)
			if firstRung != 0 {
				t.Errorf("new incident's first re-dial rung = %d (delay %s), want 0 (delay %s): "+
					"the stale post-teardown ladder (rung %d) was inherited — production "+
					"mapping: inherited rung %d -> %s vs a fresh 500ms",
					firstRung, redialDelay(firstRung), redialDelay(0), rung, firstRung, prodRedialDelay(firstRung))
			}
		})
	}
}

// prodRedialDelay mirrors redialDelay with the PRODUCTION backoffs (initial=500ms,
// max=30s) for the failure messages above, which run under the compressed test
// timing. It is exposition only — the assertions use the test-timing redialDelay.
func prodRedialDelay(attempt int) time.Duration {
	const initial = 500 * time.Millisecond
	const max = 30 * time.Second
	d := initial
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}
