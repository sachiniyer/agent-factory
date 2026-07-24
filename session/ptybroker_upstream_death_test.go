package session

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// singleSocketChannel models the REMOTE clientlessChannel contract
// (remoteClientlessChannel, agentserver_remote.go): ONE capture socket at a time.
// StartCapture refuses while a socket is still held, and only StopCapture
// releases it:
//
//	if c.conn != nil {
//	    return nil, fmt.Errorf("remote clientless capture already started")
//	}
//
// That refusal is load-bearing for #2438 and is exactly what fakeClientlessChannel
// does NOT model — it re-starts happily, so a test written against it would pass
// on a fix that merely clears the `capturing` latch while leaving the dead socket
// held, and the remote session would still never recover in the field.
type singleSocketChannel struct {
	mu sync.Mutex
	w  *io.PipeWriter
	// held is true between a successful StartCapture and the StopCapture that
	// releases it — the fake's stand-in for remoteClientlessChannel.conn != nil.
	held     bool
	starts   int
	stops    int
	snapshot []byte
	// dieOnStart makes every dialled socket drop immediately, modelling an
	// endpoint that accepts the WebSocket and then loses it (a proxy flapping, a
	// sandbox mid-restart). It is what turns a readLoop-driven re-dial into a
	// feedback loop, so it is how the backoff bound is exercised.
	dieOnStart bool
	// failStarts is how many further StartCapture calls must fail outright,
	// modelling an endpoint that is unreachable rather than flapping — the dial
	// itself errors, so no socket is ever held and no readLoop is ever spawned.
	// Each failure decrements it, so a test can have the endpoint come back.
	failStarts int
	// startGate, when non-nil, holds StartCapture until it is closed — standing in
	// for the remote WebSocket handshake, which is bounded at ten seconds. It is
	// what lets a test act (detach a subscriber, say) while a dial is genuinely
	// in flight, which is a real and now-routine window rather than a contrived
	// one. Read under mu; waited on WITHOUT mu so the gate cannot deadlock the fake.
	startGate chan struct{}
	// gateEntered is closed when a gated StartCapture reaches its gate. The dial's
	// visible counter (starts) is incremented on the FAR side of the gate, so it
	// cannot be used to detect that the dial is in flight — waiting on it would
	// wait for the very thing the gate is holding.
	gateEntered chan struct{}
}

// held reports whether the fake is still holding a capture socket. A capture
// held with no subscriber attached is a leak: nothing will ever release it.
func (c *singleSocketChannel) isHeld() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

func (c *singleSocketChannel) StartCapture() (io.ReadCloser, error) {
	c.mu.Lock()
	gate, entered := c.startGate, c.gateEntered
	c.startGate, c.gateEntered = nil, nil
	c.mu.Unlock()
	if gate != nil {
		if entered != nil {
			close(entered)
		}
		<-gate
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held {
		return nil, fmt.Errorf("remote clientless capture already started")
	}
	if c.failStarts > 0 {
		c.failStarts--
		c.starts++
		return nil, fmt.Errorf("dial sandbox agent-server: connection refused")
	}
	c.held = true
	c.starts++
	r, w := io.Pipe()
	c.w = w
	if c.dieOnStart {
		// Close the write end straight away: the broker's readLoop gets EOF on its
		// first Read, exactly as it would if the socket dropped mid-flight.
		c.w = nil
		_ = w.Close()
	}
	return r, nil
}

func (c *singleSocketChannel) StopCapture() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops++
	c.held = false
	if c.w != nil {
		_ = c.w.Close()
		c.w = nil
	}
	return nil
}

// dropUpstream simulates the WebSocket dying under a live capture: the broker's
// reader ends (so its readLoop returns) but NOTHING calls StopCapture, so the
// channel still holds its socket. That is precisely remoteClientlessChannel's
// state after a proxy/tunnel drops the long-lived connection — its own readLoop
// returns on the read error and leaves c.conn set.
func (c *singleSocketChannel) dropUpstream() {
	c.mu.Lock()
	w := c.w
	c.w = nil
	c.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

func (c *singleSocketChannel) SendRaw([]byte) error        { return nil }
func (c *singleSocketChannel) Resize(uint16, uint16) error { return nil }

func (c *singleSocketChannel) Snapshot() (PaneSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return PaneSnapshot{Screen: append([]byte(nil), c.snapshot...)}, nil
}

func (c *singleSocketChannel) counts() (starts, stops int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts, c.stops
}

func (c *singleSocketChannel) emit(t *testing.T, b []byte) {
	t.Helper()
	c.mu.Lock()
	w := c.w
	c.mu.Unlock()
	if w == nil {
		t.Fatal("emit with no live capture socket")
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

// subscribeUntilReDial keeps subscribing until the broker has re-established
// capture (a second StartCapture), or fails after a bounded wait. The retry is
// not incidental: the readLoop notices the dead upstream ASYNCHRONOUSLY, so a
// subscribe racing that exit legitimately rides the still-live capture. What
// #2438 is about is the steady state — a broker that can NEVER re-dial no matter
// how many subscribers arrive.
func subscribeUntilReDial(t *testing.T, br *ptyBroker, ch *singleSocketChannel) *ptySub {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sub, err := br.subscribe(0)
		if err != nil {
			t.Fatalf("subscribe after the upstream died: %v\n\n"+
				"the broker refused a NEW subscriber outright. Clearing the capturing latch "+
				"without releasing the dead socket leaves StartCapture returning "+
				"\"already started\" forever (#2438)", err)
		}
		if starts, _ := ch.counts(); starts >= 2 {
			return sub
		}
		_ = sub.Close()
		if time.Now().After(deadline) {
			starts, stops := ch.counts()
			t.Fatalf("broker never re-dialled after its upstream died: StartCapture calls = %d, "+
				"StopCapture calls = %d, want a second StartCapture.\n\n"+
				"The capturing latch stays true over a dead readLoop, so every later subscribe "+
				"short-circuits in ensureCaptureStartedLocked and the remote session is frozen "+
				"permanently — REST probes keep answering, so nothing marks it Lost and nothing "+
				"replaces the broker (#2438).", starts, stops)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPTYBrokerReDialsAfterUpstreamDies is #2438.
//
// A remote session's data plane (the WS to the sandbox agent-server) and its
// control plane (REST Snapshot/Alive probes) are independent transports. When a
// proxy, tunnel, or load balancer drops the long-lived WebSocket while the
// sandbox stays healthy over REST:
//
//   - the channel's reader ends, so the broker's readLoop returns;
//   - `capturing` stays TRUE over that dead readLoop, because nothing clears it;
//   - REST keeps answering, so the session is never marked Lost and no recovery
//     hook replaces the broker (resetBrokerCaptures exists only on
//     localAgentServer — the remote runtime has no equivalent);
//   - so every later subscribe short-circuits on the latch and no new socket is
//     ever dialled. The terminal is frozen permanently, and a browser refresh
//     does not help because it reuses the same wedged broker.
//
// The broker must instead reconcile on the next bring-up: release the dead
// capture (which JOINS the finished readLoop and lets the channel drop its
// socket) and dial a fresh one.
func TestPTYBrokerReDialsAfterUpstreamDies(t *testing.T) {
	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")
	ch.emit(t, []byte("before-drop"))
	mustData(t, a, "before-drop")
	if starts, _ := ch.counts(); starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 before the drop", starts)
	}

	// The WebSocket dies. A stays attached (it is parked in NextEvent), and the
	// sandbox is still perfectly healthy over REST.
	ch.dropUpstream()

	// The reconnecting client. It must get a working stream.
	b := subscribeUntilReDial(t, br, ch)
	defer func() { _ = b.Close() }()

	starts, stops := ch.counts()
	if stops < 1 {
		t.Fatalf("StopCapture calls = %d, want at least 1: the dead socket must be RELEASED, "+
			"not merely forgotten — remoteClientlessChannel.StartCapture refuses while it still "+
			"holds one, so a latch-only fix leaves the broker permanently un-restartable (#2438)",
			stops)
	}
	if starts != 2 {
		t.Fatalf("StartCapture calls = %d, want exactly 2 (one re-dial, not a storm)", starts)
	}

	// The whole point: the reconnected subscriber actually receives live output.
	mustRepaintContains(t, b, "SCREEN")
	ch.emit(t, []byte("after-redial"))
	mustData(t, b, "after-redial")
}

// TestPTYBrokerHealthyCaptureIsNotReDialled is the no-regression half: a live
// capture must still be shared by every subscriber. A "fix" that tore the
// capture down and re-dialled on every subscribe would satisfy the test above
// while reintroducing the #1661 clobber and blipping every attached client.
func TestPTYBrokerHealthyCaptureIsNotReDialled(t *testing.T) {
	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")

	b, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	mustRepaintContains(t, b, "SCREEN")

	starts, stops := ch.counts()
	if starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 (a healthy capture is shared, never re-dialled)", starts)
	}
	if stops != 0 {
		t.Fatalf("StopCapture calls = %d, want 0 (nothing died, so nothing may be torn down)", stops)
	}

	// Both subscribers ride the one capture.
	ch.emit(t, []byte("shared"))
	mustData(t, a, "shared")
	mustData(t, b, "shared")
}

// TestPTYBrokerUpstreamDeathDoesNotResurrectAClosedBroker pins the guard the
// reconcile must not lose: once the broker is closed (session killed, tab
// closed), a dead upstream must NOT be re-dialled. Otherwise the recovery path
// would dial a fresh socket into a torn-down sandbox that nothing will ever
// close — the #1632 resurrection this broker already refuses.
//
// This one is a LOCK, not a fail-first: the `b.closed` check short-circuits
// before the reconcile is even reached, so it passes on master too. It is here
// so a later change to the reconcile cannot quietly grow a resurrection path.
func TestPTYBrokerUpstreamDeathDoesNotResurrectAClosedBroker(t *testing.T) {
	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")

	ch.dropUpstream()
	br.close()

	if _, err := br.subscribe(0); err == nil {
		t.Fatal("subscribe on a CLOSED broker returned nil error: a dead upstream must never " +
			"resurrect a broker whose session was torn down")
	}
	if starts, _ := ch.counts(); starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 (a closed broker must never re-dial)", starts)
	}
}

// TestPTYBrokerCaptureEndedIsOnlyMeaningfulWithCapturing pins what captureEnded
// actually promises, because #2438 shipped a change that assumed the opposite
// and did nothing.
//
// This exercises the teardown that HAS a capture to release — the case #2438's
// clears were written for, and the case they could not serve. Such a teardown
// ends by joining the readLoop (`<-done` inside stopCapture), and the loop's
// last act before closing `done` is to latch the flag, so a clear placed before
// that join is simply undone by it. (A teardown that finds `capturing` already
// false joins nothing and could clear it — but by then there is no capture left
// to describe. See the field doc.)
//
// What this locks is the contract: the flag is meaningless once `capturing` is
// false, and the next bring-up dials a fresh capture regardless of the residue.
// Asserting the residue directly is the point — it stops the next reader from
// "fixing" a latch that is not a bug.
//
// Note this test cannot fail-first on the code change it accompanies: removing
// three writes that were already being undone has no observable effect, which is
// the change's own thesis. It is a doc-lock, and the assertion it protects is
// the one #2438's comment got wrong.
func TestPTYBrokerCaptureEndedIsOnlyMeaningfulWithCapturing(t *testing.T) {
	state := func(b *ptyBroker) (capturing, ended bool) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.capturing, b.captureEnded
	}

	ch := &singleSocketChannel{snapshot: []byte("SCREEN")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN")
	if capturing, ended := state(br); !capturing || ended {
		t.Fatalf("live capture = {capturing:%v ended:%v}, want {true false}", capturing, ended)
	}

	// The last subscriber leaves: maybeStopCapture tears the capture down and
	// JOINS the readLoop, whose defer latches the flag on its way out.
	if err := a.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}
	capturing, ended := state(br)
	if capturing {
		t.Fatalf("capturing = true after the last subscriber left, want false")
	}
	if !ended {
		t.Fatal("captureEnded = false after a teardown joined the readLoop.\n\n" +
			"If this ever passes, the join stopped latching the flag — re-read the field doc, " +
			"because it says a teardown CANNOT clear it and that is why no teardown tries.")
	}

	// And the residue is harmless: the next bring-up reconciles it away and dials
	// a genuinely fresh capture. This is the only property any caller depends on.
	b, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe after teardown: %v", err)
	}
	defer func() { _ = b.Close() }()
	if capturing, ended := state(br); !capturing || ended {
		t.Fatalf("re-armed capture = {capturing:%v ended:%v}, want {true false}", capturing, ended)
	}
	if starts, _ := ch.counts(); starts != 2 {
		t.Fatalf("StartCapture calls = %d, want 2 (the teardown's residue must not block a fresh dial)", starts)
	}
	mustRepaintContains(t, b, "SCREEN")
	ch.emit(t, []byte("after-teardown"))
	mustData(t, b, "after-teardown")
}
