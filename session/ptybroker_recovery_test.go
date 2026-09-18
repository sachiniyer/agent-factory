package session

import (
	"context"
	"errors"
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

// waitRingHead blocks until the broker's ring head reaches want — i.e. feed() has
// actually appended the emitted bytes. emit()'s Write returns once the readLoop's
// Read consumed the bytes, which is one step SHORT of them landing in the ring, so a
// test that must observe a lagging cursor has to synchronise on the ring itself
// rather than on the write.
func waitRingHead(t *testing.T, br *ptyBroker, want Seq) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		br.mu.Lock()
		head := br.headLocked()
		br.mu.Unlock()
		if head >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ring head never reached %d", want)
}

// TestPTYBrokerRecoveryDoesNotReplayStaleBytes is the #1840 regression: a subscriber
// that is BEHIND at recovery time must not receive the dead pane's buffered bytes
// after the recovery repaint.
//
// The setup the existing recovery tests never hit: every one of them drains each
// event as it lands, so the subscriber's cursor is always at the live tail when
// resetCapture runs and the ring holds nothing to replay. In production a subscriber
// falls behind whenever its WS write blocks (up to the write timeout) while the pane
// keeps producing — so at recovery its cursor sits below head with dead-pane bytes in
// between.
//
// Fail-before/pass-after: before the fix, reseed injects the repaint but leaves the
// ring and cursors untouched, so NextEvent returns the repaint and then — seeing
// cursor < head — hands back the dead pane's bytes, which overwrite the freshly
// repainted screen. After the fix resetCapture discards the dead pane's ring bytes at
// the recovery boundary, so the repaint is the last thing A sees until the re-spawned
// pane produces real output.
func TestPTYBrokerRecoveryDoesNotReplayStaleBytes(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-BEFORE-DEATH")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")

	// A falls behind: the dying pane emits bytes A never consumes (its WS write was
	// blocked). They sit in the ring with A's cursor still behind head.
	const stale = "STALE-BYTES-FROM-DEAD-PANE"
	ch.emit(t, []byte(stale))
	waitRingHead(t, br, Seq(len(stale)))

	// tmux dies and the daemon re-spawns it; the recovered pane shows a new screen.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()
	br.resetCapture()

	// A repaints the recovered screen...
	mustRepaintContains(t, a, "SCREEN-AFTER-RECOVERY")

	// ...then learns where the discard left its cursor. A was BEHIND at recovery, so
	// the discard fast-forwarded it over the dead bytes; that jump is the server's and
	// A's client cannot infer it, so it must be announced (#1845 follow-up) or A's next
	// reconnect asks to replay bytes it has already rendered.
	ev, err := nextWithin(t, a, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent (want the cursor re-seed): %v", err)
	}
	if ev.Kind != PTYCursor || ev.Seq != Seq(len(stale)) {
		t.Fatalf("event = %+v, want PTYCursor Seq=%d (base advanced over the discarded ring)", ev, len(stale))
	}

	// ...and must NOT be handed the dead pane's bytes on top of the repaint. The
	// discard dropped them; it did not merely defer them.
	if ev, err := nextWithin(t, a, 250*time.Millisecond); err == nil {
		t.Fatalf("after the recovery repaint A got Kind=%d Data=%q, want no event: "+
			"dead-pane bytes must not overwrite the recovered screen", ev.Kind, ev.Data)
	}

	// The recovered pane still streams: discarding the dead bytes must not wedge the
	// ring or strand A's cursor above head.
	ch.emit(t, []byte("post-recovery-output"))
	mustData(t, a, "post-recovery-output")
}

// TestPTYBrokerRecoveryRepaintPrecedesLiveBytes is #1975: a subscriber that stayed
// attached across a tmux respawn must see the recovery repaint BEFORE any byte from
// the re-spawned pane — never a stale/partial frame that the repaint then wipes.
//
// The window is `reseedSubscribersLocked`'s Snapshot, which runs WITHOUT b.mu (a real
// `capture-pane` exec takes milliseconds) AFTER resetCapture has already restarted the
// capture. The fresh readLoop feeds and wakes subscribers during that window, and
// pendingRepaint is not set yet, so NextEvent hands back PTYCursor/PTYData first: the
// client renders live bytes onto its pre-death screen, then the repaint clears and
// redraws it — the visible flicker. Order delivered was PTYData -> PTYRepaint; the
// order owed is PTYRepaint -> PTYCursor -> PTYData.
//
// Fail-before/pass-after: the snapshotHook drives exactly that interleaving — it emits
// pane output while the recovery snapshot is still being taken and waits (bounded) for
// the subscriber to consume it. On the pre-fix ordering the subscriber's cursor moves
// inside the hook and the first recorded event is PTYData, so the PTYRepaint assert
// below fails. With the barrier the subscriber stays parked for the whole recovery, the
// repaint lands first, and the live bytes follow it undropped.
func TestPTYBrokerRecoveryRepaintPrecedesLiveBytes(t *testing.T) {
	const live = "LIVE-BYTES-FROM-RESPAWNED-PANE"
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-BEFORE-DEATH")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")

	// A stays attached and parked in NextEvent across the respawn — the common case
	// (a web/TUI client whose socket never dropped). Record its event stream in order.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan PTYEvent, 8)
	go func() {
		for {
			ev, err := a.NextEvent(ctx)
			if err != nil {
				return
			}
			events <- ev
		}
	}()

	// tmux dies and the daemon re-spawns it. The recovered pane shows a new screen —
	// and starts producing output while the recovery snapshot is still being captured.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.snapshotHook = func() {
		ch.emit(t, []byte(live))
		waitRingHead(t, br, Seq(len(live)))
		// Give the woken subscriber room to deliver those bytes. On the pre-fix
		// ordering it does so at once (its cursor moves); with the barrier it stays
		// parked and this simply burns its deadline.
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) && a.Seq() == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	ch.mu.Unlock()

	br.resetCapture()

	// The repaint is the FIRST thing A sees after recovery. Anything before it is a
	// frame rendered against the wrong screen that the repaint immediately wipes.
	select {
	case ev := <-events:
		if ev.Kind != PTYRepaint || !strings.Contains(string(ev.Data), "SCREEN-AFTER-RECOVERY") {
			t.Fatalf("first post-recovery event = Kind=%d Data=%q, want the PTYRepaint of "+
				"the recovered screen: pane output delivered before the recovery repaint "+
				"renders a stale frame the repaint then wipes (the flicker)", ev.Kind, ev.Data)
		}
		if ev.RepaintProvenance != PTYRepaintRecovery {
			t.Fatalf("recovery repaint provenance = %d, want recovery", ev.RepaintProvenance)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no post-recovery event: A was never re-seeded")
	}

	// Holding the stream is not dropping it: the bytes the pane produced during the
	// snapshot window still arrive, after the repaint.
	select {
	case ev := <-events:
		if ev.Kind != PTYData || string(ev.Data) != live {
			t.Fatalf("event after the repaint = Kind=%d Data=%q, want PTYData %q", ev.Kind, ev.Data, live)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("pane output produced during the recovery snapshot was never delivered")
	}
}

// clientCursor models what a REAL client does with the events it receives — the same
// arithmetic ui/termpane's readStream and web/src/terminal.ts run: adopt the server's
// cursor whenever it announces one, advance it by each PTYData byte count, and ignore
// repaints (per-subscriber, outside the ring seq).
//
// Modelling it is the point of the tests below: a client knows only what it was TOLD.
// It cannot see the broker's base/head, so a cursor the SERVER moves without saying so
// desyncs it silently — and the damage only surfaces one reconnect later, which is
// exactly why the broker-internal assertions in the other tests missed it.
type clientCursor struct{ seq Seq }

// applyAll drains every event available within d, folds each into the client's cursor
// the way a real client would, and returns the PTY bytes the client rendered.
func (c *clientCursor) applyAll(t *testing.T, sub PTYSubscription, d time.Duration) []byte {
	t.Helper()
	var rendered []byte
	for {
		ev, err := nextWithin(t, sub, d)
		if err != nil {
			return rendered // drained: no event within d
		}
		switch ev.Kind {
		case PTYCursor:
			c.seq = ev.Seq
		case PTYData:
			c.seq += Seq(len(ev.Data))
			rendered = append(rendered, ev.Data...)
		}
	}
}

// TestPTYBrokerReconnectAfterRecoveryDoesNotReplayRenderedBytes is the first codex P2
// on the merged #1845: a subscriber that saw post-recovery output and THEN reconnects
// must not be re-sent the bytes it already rendered.
//
// #1845 advances `base` over the discarded dead-pane ring, which fast-forwards a
// lagging subscriber's server-side cursor through the eviction clamp. That jump is
// invisible to the client, which derives its own cursor as start + bytes-received: it
// keeps counting from the pre-recovery value, so its cursor sits BELOW the broker's new
// base. On reconnect that stale ?since is clamped back up to base and the broker
// replays post-recovery bytes the client already has — duplicated/corrupt terminal
// output until something forces a full repaint.
//
// Fail-before/pass-after: without the PTYCursor re-seed the client's cursor and the
// server's diverge by the discarded gap (the first assert), and the reconnect replays
// POST-RECOVERY-OUTPUT a second time (the last assert).
func TestPTYBrokerReconnectAfterRecoveryDoesNotReplayRenderedBytes(t *testing.T) {
	const drain = 250 * time.Millisecond
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-BEFORE-DEATH")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	// The client seeds from the opening hello / X-Af-Stream-Seq header, then drains the
	// initial repaint.
	client := &clientCursor{seq: a.Seq()}
	client.applyAll(t, a, drain)

	// A falls behind: its WS write blocked while the dying pane kept producing, so
	// these bytes sit unread in the ring with A's cursor below head.
	const stale = "STALE-BYTES-FROM-DEAD-PANE"
	ch.emit(t, []byte(stale))
	waitRingHead(t, br, Seq(len(stale)))

	// tmux dies and the daemon re-spawns it: the #1845 discard drops the dead pane's
	// bytes and advances base over A's cursor.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()
	br.resetCapture()

	// A's write unblocks and it drains the recovery repaint + the cursor re-seed.
	client.applyAll(t, a, drain)

	// The re-spawned pane produces real output, which A receives and renders.
	const post = "POST-RECOVERY-OUTPUT"
	ch.emit(t, []byte(post))
	if got := client.applyAll(t, a, drain); string(got) != post {
		t.Fatalf("A rendered %q after recovery, want %q", got, post)
	}

	// The invariant: what the client believes its cursor to be IS what the server knows
	// it to be. Everything below follows from this.
	if client.seq != a.Seq() {
		t.Fatalf("client cursor = %d, server cursor = %d: the recovery jump was never "+
			"announced, so the client's next ?since is stale by the discarded gap",
			client.seq, a.Seq())
	}

	// The user-visible consequence, end to end: A's socket drops and it reconnects with
	// the cursor it tracked. It has rendered every byte the broker holds, so it must be
	// handed nothing to replay.
	_ = a.Close()
	b, err := br.subscribe(client.seq)
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	if ev, err := nextWithin(t, b, drain); err == nil {
		t.Fatalf("reconnect at cursor %d replayed Kind=%d Data=%q, want no event: the "+
			"client already rendered those bytes and would render them twice",
			client.seq, ev.Kind, ev.Data)
	}
}

// TestPTYBrokerReconnectDoesNotReplayBytesTheRepaintAlreadyShows is the codex P1 on
// #1872: a reconnect owed a repaint must NOT then be replayed the retained ring the
// repaint already reflects.
//
// The #1845 follow-up (repaint when since < base) captures a snapshot of the CURRENT
// screen — which already reflects every retained ring byte [base, head). If the new
// subscription still starts its replay at base, NextEvent sends the repaint and THEN
// hands back [base, head) on top of it, rendering that output twice: a command/prompt
// appended again, up to the whole retained ring on an eviction or post-recovery
// reconnect.
//
// Fail-before/pass-after: before the fix the reconnect's cursor is clamped to base, so
// after the repaint it receives the retained tail as PTYData (the duplicate); after the
// fix the cursor starts at the live tail, so the repaint is the last thing it sees until
// genuinely new output arrives.
func TestPTYBrokerReconnectDoesNotReplayBytesTheRepaintAlreadyShows(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("CURRENT-SCREEN")}
	br := newPTYBroker(ch)
	br.maxBytes = 4 // tiny ring so base advances past 0 while the ring stays NONEMPTY

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "CURRENT-SCREEN")

	// Fill then overflow the ring: "AAAA" is evicted by "BBBB", so base advances to 4
	// while the ring still holds [4,8)="BBBB". A stays attached to keep the capture up.
	ch.emit(t, []byte("AAAA"))
	mustData(t, a, "AAAA")
	ch.emit(t, []byte("BBBB"))
	mustData(t, a, "BBBB")
	waitRingHead(t, br, 8)

	// A client reconnects with a cursor BELOW base (it fell far behind): the bytes it
	// asked for are gone, so it is owed a repaint. The repaint reconstructs the whole
	// current screen, which already reflects the retained ring — so [4,8) must NOT be
	// replayed on top of it.
	b, err := br.subscribe(2) // since = 2 < base = 4, ring nonempty
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	// The reconnect starts at the live tail, so its ?since seed is head, not base.
	if got := b.Seq(); got != 8 {
		t.Fatalf("reconnect cursor = %d, want 8 (the live tail, so the retained ring is not replayed)", got)
	}
	mustRepaintContains(t, b, "CURRENT-SCREEN")

	// No PTYData replay of the retained ring after the repaint.
	if ev, err := nextWithin(t, b, 250*time.Millisecond); err == nil {
		t.Fatalf("after the repaint B got Kind=%d Data=%q, want no event: the repaint "+
			"already reflects the retained ring, so replaying it duplicates output",
			ev.Kind, ev.Data)
	}

	// Genuinely new output past the snapshot still streams to the reconnect.
	ch.emit(t, []byte("CCCC"))
	mustData(t, b, "CCCC")
}

// TestPTYBrokerReconnectRepaintsWhenRecoveryDiscardedItsCursor is the second codex P2
// on the merged #1845: a reconnect whose cursor the recovery discard skipped must be
// repainted, because it can get no replay.
//
// The path is recovery with NOBODY attached (the last client's keepalive had already
// lapsed, or it dropped just after `resume` was computed): resetCapture discards the
// ring and advances base unconditionally, but takes the no-subscriber branch, so no
// re-seed fans out. The client then reconnects with the cursor it left on — now below
// base. subscribe clamps it up to base, which yields no replay, and the repaint
// injection used to be keyed on `since == 0`, so a reconnect got none either. The
// client is left rendering its pre-death screen until the re-spawned pane happens to
// emit something on its own.
//
// Fail-before/pass-after: before the fix the reconnect receives NO event at all and
// mustRepaintContains times out.
func TestPTYBrokerReconnectRepaintsWhenRecoveryDiscardedItsCursor(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-BEFORE-DEATH")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")

	// The client renders some output, so it holds a real (non-zero) replay cursor —
	// NOT the `since == 0` fresh-subscriber sentinel, which would repaint anyway.
	ch.emit(t, []byte("seen"))
	mustData(t, a, "seen")
	cursor := a.Seq()

	// It then falls behind on bytes it never renders and its socket drops, leaving its
	// cursor below head. The last subscriber leaving also stops the capture.
	const unseen = "NEVER-RENDERED"
	ch.emit(t, []byte(unseen))
	waitRingHead(t, br, cursor+Seq(len(unseen)))
	_ = a.Close()

	// tmux dies and is re-spawned while nobody is attached: the discard advances base
	// past the client's cursor, and there is no subscriber to re-seed.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()
	br.resetCapture()

	// The client reconnects on the cursor it left on. The broker cannot serve that
	// replay — those bytes are gone — so a repaint of the RECOVERED screen is the only
	// thing that can resync it.
	b, err := br.subscribe(cursor)
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	mustRepaintContains(t, b, "SCREEN-AFTER-RECOVERY")
}

// TestPTYBrokerReconnectRepaintsWhenCaughtUpAfterDiscard is the caught-up counterpart to
// TestPTYBrokerReconnectRepaintsWhenRecoveryDiscardedItsCursor: a reconnect whose cursor
// lands EXACTLY at the post-discard base must STILL be repainted, even though
// `since < b.base` is false.
//
// The behind-client test emits a NEVER-RENDERED chunk the client never consumes, so the
// discard advances base PAST the cursor and `since < b.base` fires. This test OMITS that
// emit: the pane is idle, the client is caught up at the live tail (cursor == head) when
// its socket drops, and the no-subscriber discard sets `base = head == cursor`. With the
// pre-fix strict `<` that landed inside a pane-replacing recovery, `since == b.base`
// took the seamless branch and the client got NO event — its emulator kept the dead
// pane's last screen until the recovered pane happened to emit, and that emit arrived as
// bare PTYData on the un-cleared frame. recoveryDiscardAt marks that the current base
// came from a pane-replacing discard (not a same-pane clamp), so subscribe repaints the
// caught-up reconnect the same way it repaints the behind one.
//
// Fail-before/pass-after: before the fix the 500ms nextWithin below times out with no
// repaint; after the fix mustRepaintContains passes.
func TestPTYBrokerReconnectRepaintsWhenCaughtUpAfterDiscard(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-BEFORE-DEATH")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")

	// The client renders some output, so it holds a real (non-zero) replay cursor —
	// NOT the `since == 0` fresh-subscriber sentinel, which would repaint anyway.
	ch.emit(t, []byte("seen"))
	mustData(t, a, "seen")
	cursor := a.Seq()

	// NO further emit: the pane is idle, so the client is caught up at the live tail
	// (cursor == head) when its socket drops. The last subscriber leaving stops the
	// capture; nothing is emitted between the drop and the discard below.
	_ = a.Close()

	// tmux dies and is re-spawned while nobody is attached. The no-subscriber discard
	// sets base = head == cursor (the idle-pane case the behind-client test does NOT
	// construct — there `cursor < base`), and the early return skips the re-seed.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()
	br.resetCapture()

	// The client reconnects on the cursor it left on — EXACTLY the post-discard base.
	// The replay is empty (base == head == cursor), and the pane behind the cursor was
	// REPLACED, so a repaint of the RECOVERED screen is the only thing that can resync
	// it: bare PTYData on the dead pane's last frame would leave the screen
	// stale-changed, not reset.
	b, err := br.subscribe(cursor)
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	mustRepaintContains(t, b, "SCREEN-AFTER-RECOVERY")
	// The repaint already reflects the (empty) live tail, so nothing follows it and the
	// cursor sits at the tail, not below base.
	if got := b.Seq(); got != cursor {
		t.Fatalf("reconnect cursor = %d, want %d (the live tail; the recovered screen "+
			"repaint already reflects the empty replay, so the cursor must advance to it)", got, cursor)
	}
	if ev, err := nextWithin(t, b, 250*time.Millisecond); err == nil {
		t.Fatalf("after the repaint B got Kind=%d Data=%q, want no event: the replay "+
			"is empty (base == head == cursor), so anything here is a duplicate", ev.Kind, ev.Data)
	}

	// The recovered pane still streams: repainting must not wedge the ring.
	ch.emit(t, []byte("post-recovery-output"))
	mustData(t, b, "post-recovery-output")
}

// TestPTYBrokerReconnectAtBaseAfterEvictionStaysSeamless is the no-regression guard for
// the recoveryDiscardAt watermark: a same-pane eviction that leaves a reconnect at
// `since == base` (the cursor an eviction clamp announced via PTYCursor) must take the
// SEAMLESS replay path — NOT repaint. The strict-`<` in subscribe exists precisely to
// avoid a clear-and-redraw flicker on this clamp-to-base reconnect; switching to `<=`
// would regress it. The watermark is the reason the fix can repaint the
// pane-replacing `since == base` case (above) WITHOUT repainting this one: an eviction
// advances base via `b.base += drop` in feed(), which never sets recoveryDiscardAt, so
// `recoveryDiscardAt == b.base` is false and the caught-up repaint condition does not
// fire. This test fails (B gets a PTYRepaint) if the fix naively uses `<=`.
func TestPTYBrokerReconnectAtBaseAfterEvictionStaysSeamless(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("S")}
	br := newPTYBroker(ch)
	br.maxBytes = 4 // tiny ring so base advances via eviction while the ring stays nonempty

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "S")

	// Overflow the ring: base advances to 4 and the retained window is [4,8)="efgh".
	ch.emit(t, []byte("abcdefgh"))
	// A fell behind its own eviction (cursor 0 < base 4), so NextEvent fast-forwards it
	// to base with a PTYCursor — the server-side jump #1845 announces — then hands back
	// the retained tail. Consume ONLY the cursor re-seed, modelling a client that
	// adopted the re-seed and then dropped before rendering the retained bytes: its
	// tracked cursor is now exactly the post-eviction base.
	ev, err := nextWithin(t, a, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent (want the cursor re-seed): %v", err)
	}
	if ev.Kind != PTYCursor || ev.Seq != 4 {
		t.Fatalf("event = %+v, want PTYCursor Seq=4 (eviction clamp to base)", ev)
	}
	cursor := ev.Seq
	_ = a.Close()

	// The client reconnects at exactly the post-eviction base. This is a SAME-PANE
	// eviction — no recovery discard replaced the pane — so recoveryDiscardAt is unset
	// and the caught-up repaint condition must NOT fire. subscribe takes the seamless
	// path: replay [base, head) as PTYData, with no clear-and-redraw flicker.
	b, err := br.subscribe(cursor)
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	ev, err = nextWithin(t, b, 2*time.Second)
	if err != nil {
		t.Fatalf("B NextEvent: %v", err)
	}
	if ev.Kind == PTYRepaint {
		t.Fatalf("B got a PTYRepaint at since==base after a same-pane eviction: the " +
			"recovery-discard watermark must NOT fire for a base the discard did not set " +
			"(a same-pane eviction clamp), or the seamless clamp-to-tail reconnect " +
			"regresses to a clear-and-redraw flicker")
	}
	if ev.Kind != PTYData || string(ev.Data) != "efgh" {
		t.Fatalf("B first event = %+v, want PTYData %q (seamless replay of the "+
			"retained window [base, head))", ev, "efgh")
	}
}

// caughtUpReconnectAfterDiscard stages the #4615 shape: a reconnect whose cursor lands
// EXACTLY at a base the recovery discard set, so subscribe takes the watermark repaint
// path — with the recovered pane's output still RETAINED in the ring above that base
// whenever recovered is non-empty.
//
// TestPTYBrokerReconnectRepaintsWhenCaughtUpAfterDiscard builds the same watermark with
// nobody attached, which leaves the capture stopped and the ring empty, so [base, head)
// can never hold anything and the reconnect has nothing to lose. Here A stays attached
// across the respawn — a second browser tab, or the TUI next to one — so the recovery
// restarts the capture (resume) and the recovered pane's bytes land in the ring before
// the caught-up client reconnects. That is the case where starting the reconnect at
// the tail skips bytes that are still there.
//
// Returns the reconnect cursor: the caught-up position a client left on before the
// respawn, which the discard turned into the new base.
func caughtUpReconnectAfterDiscard(t *testing.T, recovered string) (*fakeClientlessChannel, *ptyBroker, Seq) {
	t.Helper()
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-BEFORE-DEATH")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")
	// A real (non-zero) cursor, so the reconnect is not the since == 0 fresh sentinel.
	ch.emit(t, []byte("pre-death"))
	mustData(t, a, "pre-death")
	since := a.Seq() // caught up: the cursor a client at the live tail leaves on

	// tmux dies and is re-spawned while A is attached: the discard sets base = head,
	// marks it as a recovery discard, restarts the capture and re-seeds A.
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-AFTER-RECOVERY")
	ch.mu.Unlock()
	br.resetCapture()
	mustRepaintContains(t, a, "SCREEN-AFTER-RECOVERY")

	if recovered != "" {
		ch.emit(t, []byte(recovered))
		// A's consumption proves feed() landed the bytes in the ring, not merely that
		// the pipe write was read.
		mustData(t, a, recovered)
	}

	// Pin the fixture to the watermark case, so it cannot drift into since < base (the
	// behind reconnect) or an unmarked base (the eviction clamp) without failing here.
	br.mu.Lock()
	base, mark, head := br.base, br.recoveryDiscardAt, br.headLocked()
	br.mu.Unlock()
	if base != since || mark != base {
		t.Fatalf("fixture: base=%d recoveryDiscardAt=%d since=%d, want since == base == "+
			"recoveryDiscardAt (the caught-up reconnect after a recovery discard)", base, mark, since)
	}
	if want := since + Seq(len(recovered)); head != want {
		t.Fatalf("fixture: head=%d, want %d (the recovered bytes retained above base)", head, want)
	}
	return ch, br, since
}

// TestPTYBrokerCaughtUpReconnectReplaysRingWhenRepaintUnavailable is #4615, the state
// that has to CHANGE: the caught-up reconnect after a recovery discard, with the
// recovered pane's output retained in [base, head), and no repaint to show for it.
//
// cc0ff9a5 (#4384) routed this reconnect onto the repaint path, and that path started
// every subscriber at the live tail at CREATION — before Snapshot() runs. For the two
// older triggers that costs nothing (since == 0 has no history; since < base asked for
// bytes that are gone), but here since == base, so [base, head) is exactly the
// recovered pane's buffered output and every byte of it is still in the ring. A
// snapshot that fails or carries no repaint state then delivers neither the repaint
// nor those bytes: the client stays on the dead pane's frozen frame and the recovered
// output never arrives. Before cc0ff9a5 the same reconnect replayed [base, head)
// whatever the snapshot did.
//
// Both halves of subscribe's best-effort gate are covered — `err == nil` and
// snapshotHasRepaintState — because either one leaves the ring as the only thing that
// can render the pane.
//
// The client side is modelled with clientCursor, seeded from Seq() read AFTER subscribe
// returns: that is when the daemon reads it for X-Af-Stream-Seq and the opening
// OpHello (daemon/ws_pty.go), so it is the value the client is actually told.
//
// Fail-before/pass-after: on master Seq() is head, B gets no event at all, and the
// first assertion below fails.
func TestPTYBrokerCaughtUpReconnectReplaysRingWhenRepaintUnavailable(t *testing.T) {
	const recovered = "RECOVERED-PANE-OUTPUT"
	for _, tc := range []struct {
		name string
		fail func(ch *fakeClientlessChannel)
	}{
		{"snapshot error", func(ch *fakeClientlessChannel) {
			ch.snapshotErr = errors.New("capture-pane: no such pane")
		}},
		{"snapshot without repaint state", func(ch *fakeClientlessChannel) {
			// Blank grid, no cursor, no modes: snapshotHasRepaintState is false, so no
			// repaint is queued even though Snapshot() itself succeeded.
			ch.snapshot = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, br, since := caughtUpReconnectAfterDiscard(t, recovered)
			ch.mu.Lock()
			tc.fail(ch)
			ch.mu.Unlock()

			b, err := br.subscribe(since)
			if err != nil {
				t.Fatalf("reconnect subscribe: %v", err)
			}
			t.Cleanup(func() { _ = b.Close() })
			// The published cursor is the replay start, not the tail: nothing was repainted,
			// so nothing above base may be skipped.
			if got := b.Seq(); got != since {
				t.Fatalf("published cursor = %d, want %d (base): no repaint was built, so "+
					"starting at the tail drops the recovered pane's retained output", got, since)
			}
			client := &clientCursor{seq: b.Seq()}
			if got := client.applyAll(t, b, 250*time.Millisecond); string(got) != recovered {
				t.Fatalf("client rendered %q, want %q replayed from the ring", got, recovered)
			}
			// What the client counted is what the server holds, so its next ?since is exact.
			if client.seq != b.Seq() {
				t.Fatalf("client cursor = %d, server cursor = %d", client.seq, b.Seq())
			}

			// The recovered pane keeps streaming after the replay.
			ch.emit(t, []byte("later"))
			mustData(t, b, "later")
		})
	}
}

// TestPTYBrokerCaughtUpReconnectRepaintDoesNotReplayRing is the snapshot-SUCCESS state of
// the same reconnect, and the #1872 guard for it. The repaint reconstructs the whole
// recovered screen, so [base, head) is already on it; replaying those bytes on top
// would render the recovered output twice.
//
// It passes on master too. Its job is to hold the fix to its premise: the reconnect's
// creation cursor now sits at base, so the only thing standing between it and a
// duplicate is the success-path commit that moves the cursor to repaintTail together
// with the queued repaint. Deleting that commit fails this test (the PTYData check), and
// the published cursor check catches the same mutation before it.
func TestPTYBrokerCaughtUpReconnectRepaintDoesNotReplayRing(t *testing.T) {
	const recovered = "RECOVERED-PANE-OUTPUT"
	ch, br, since := caughtUpReconnectAfterDiscard(t, recovered)
	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-WITH-RECOVERED-OUTPUT")
	ch.mu.Unlock()

	b, err := br.subscribe(since)
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	head := since + Seq(len(recovered))
	if got := b.Seq(); got != head {
		t.Fatalf("published cursor = %d, want %d (the tail): the repaint already shows "+
			"[base, head), so the replay must start past it", got, head)
	}
	mustRepaintContains(t, b, "SCREEN-WITH-RECOVERED-OUTPUT")
	client := &clientCursor{seq: b.Seq()}
	if got := client.applyAll(t, b, 250*time.Millisecond); len(got) != 0 {
		t.Fatalf("after the repaint the client rendered %q, want nothing: the repaint "+
			"already shows those bytes (#1872)", got)
	}
	if client.seq != b.Seq() {
		t.Fatalf("client cursor = %d, server cursor = %d", client.seq, b.Seq())
	}

	ch.emit(t, []byte("later"))
	mustData(t, b, "later")
}

// TestPTYBrokerCaughtUpReconnectIdleRingWithoutRepaint is the third state: the snapshot
// fails and the recovered pane has not emitted, so [base, head) is empty. There is
// nothing to replay and nothing to paint. The reconnect must still return promptly,
// deliver no blank or partial repaint and no spurious bytes, sit at the tail, and pick
// up the recovered pane's first output when it arrives. The screen stays on the dead
// pane's last frame until then; no reconnect can do better without a snapshot.
//
// Unchanged by the fix (the replay start and the tail coincide here). It pins that
// keying the creation cursor on replayability did not turn "nothing to replay" into a
// hang or a stray event.
func TestPTYBrokerCaughtUpReconnectIdleRingWithoutRepaint(t *testing.T) {
	ch, br, since := caughtUpReconnectAfterDiscard(t, "")
	ch.mu.Lock()
	ch.snapshotErr = errors.New("capture-pane: no such pane")
	ch.mu.Unlock()

	type subResult struct {
		sub *ptySub
		err error
	}
	res := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(since)
		res <- subResult{s, e}
	}()
	var b subResult
	select {
	case b = <-res:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect subscribe never returned with the snapshot failing")
	}
	if b.err != nil {
		t.Fatalf("reconnect subscribe: %v", b.err)
	}
	t.Cleanup(func() { _ = b.sub.Close() })
	if got := b.sub.Seq(); got != since {
		t.Fatalf("published cursor = %d, want %d (base == head)", got, since)
	}
	if ev, err := nextWithin(t, b.sub, 250*time.Millisecond); err == nil {
		t.Fatalf("reconnect got Kind=%d Data=%q, want no event: the snapshot failed and "+
			"the ring is empty", ev.Kind, ev.Data)
	}

	ch.emit(t, []byte("first-recovered-output"))
	mustData(t, b.sub, "first-recovered-output")
}
