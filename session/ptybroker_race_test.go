package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// waitSubscriberCount blocks until the broker holds exactly n subscribers.
//
// This is the synchronization these interleavings need, and a sleep is not a
// substitute for it (#3731, the third codex P2 on #3495). What every test below
// must pin is that the racing subscriber computed its repaint decision against the
// PRE-transition base — and registration in b.subs is that decision's own last
// step: subscribe() computes needRepaint, sets the cursor, inserts into b.subs and
// releases b.mu in ONE section. So observing the count IS observing the decision,
// whereas a sleep only makes it likely: on a loaded runner the goroutine can stay
// unscheduled past the release, compute against the POST-transition base, and pass
// the test with the production fix removed. Measured: with the #3495 re-check
// deleted and the 50 ms sleep gone, TestPTYBrokerSubscribeDuringRecoveryRace passed
// 10/10.
//
// Polling rather than a hook, for the same reason waitRingHead polls: the state is
// already reachable from the test (same package), so pinning it needs no test-only
// seam in production code.
func waitSubscriberCount(t *testing.T, br *ptyBroker, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		br.mu.Lock()
		got := len(br.subs)
		br.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("broker never reached %d subscribers", n)
}

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
// Synchronization is deterministic via stopEntered/stopRelease (recovery parks
// inside StopCapture, see TestPTYBrokerTeardownDoesNotClobberReconnect) plus
// waitSubscriberCount, which proves B registered — and therefore decided — before
// recovery is released. It used to be a 50 ms sleep, which proved nothing; see
// waitSubscriberCount.
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

	// B has registered, so its needRepaint=false was computed against the
	// PRE-recovery base. Recovery holds captureMu for its whole transition, so B can
	// only be parked in ensureCaptureStarted from here.
	waitSubscriberCount(t, br, 2)

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
	waitSubscriberCount(t, br, 2)
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

// TestPTYBrokerSubscribeDuringRecoveryClampedCursorRace is the first codex P2 left
// live on the merged #3495 (#3731): the post-recovery re-check compared the RAW
// `since`, not the cursor the subscriber actually holds.
//
// A `since` past head clamps DOWN to the live tail (the documented clamp-to-tail
// path — that client is missing no byte the broker holds, so it is deliberately not
// repainted). The subscriber therefore leaves the first section with
// cursor == head_pre while `since` stays large. It then parks in
// ensureCaptureStarted on captureMu, which recovery holds across stop / discard /
// base advance / restart. The dying pane keeps producing while recovery is parked in
// StopCapture, so the discard advances base ABOVE that clamped cursor — but still
// BELOW the large `since`.
//
// Both halves of the real state now say a repaint is owed: cursor < base, the bytes
// between them provably no longer exist, and the pane behind them has been replaced.
// `since < b.base` says otherwise, because `since` was never the cursor.
//
// Fail-before/pass-after: before the fix B's first event is a bare PTYCursor jump for
// a pane whose whole screen changed, so its terminal keeps rendering the pre-death
// frame until unrelated later output arrives — the exact #3488 symptom, surviving
// inside the branch added to fix it.
func TestPTYBrokerSubscribeDuringRecoveryClampedCursorRace(t *testing.T) {
	ch := &fakeClientlessChannel{
		snapshot:    []byte("SCREEN-BEFORE-DEATH"),
		stopEntered: make(chan struct{}, 1),
		stopRelease: make(chan struct{}),
	}
	br := newPTYBroker(ch)

	// A brings the capture up and drains, so the ring is nonempty: base=0, head=16.
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-BEFORE-DEATH")

	const preDeath = "PRE-DEATH-OUTPUT" // 16 bytes
	ch.emit(t, []byte(preDeath))
	mustData(t, a, preDeath)

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

	// B reconnects on a cursor PAST head — a client whose seq ran ahead of what this
	// broker retains. subscribe clamps it DOWN to head (16) and, correctly for that
	// path, decides no repaint is owed.
	const bSince = Seq(100)
	type subResult struct {
		sub *ptySub
		err error
	}
	bres := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(bSince)
		bres <- subResult{s, e}
	}()
	waitSubscriberCount(t, br, 2)

	// The dying pane keeps producing while recovery is parked: StopCapture gates
	// BEFORE closing the writer, so the old capture's readLoop is still feeding the
	// ring. head 16 → 34, which is what the discard is about to make the new base —
	// above B's clamped cursor (16), below B's `since` (100). That gap is the bug.
	const dyingOutput = "OUTPUT-WHILE-DYING" // 18 bytes
	ch.emit(t, []byte(dyingOutput))
	newBase := Seq(len(preDeath) + len(dyingOutput))
	waitRingHead(t, br, newBase)
	if bSince <= newBase {
		t.Fatalf("test setup: since (%d) must stay ABOVE the post-recovery base (%d), "+
			"otherwise the raw-`since` comparison would catch this by accident", bSince, newBase)
	}

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
	ev, err := nextWithin(t, b.sub, 2*time.Second)
	if err != nil {
		t.Fatalf("B NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint {
		t.Fatalf("B first event = Kind %d, want PTYRepaint (the recovered screen): the "+
			"discard advanced base past B's CLAMPED cursor (%d), so the replay it holds "+
			"cannot be served — comparing the raw since (%d) misses that", ev.Kind, newBase, bSince)
	}
	if !strings.Contains(string(ev.Data), "SCREEN-AFTER-RECOVERY") {
		t.Fatalf("B repaint = %q, want the recovered screen content", ev.Data)
	}

	// The repaint already reflects the live tail, so nothing follows it — in
	// particular no PTYCursor announcing a gap the repaint just closed.
	if ev, err := nextWithin(t, b.sub, 250*time.Millisecond); err == nil {
		t.Fatalf("after the repaint B got Kind=%d Data=%q, want no event", ev.Kind, ev.Data)
	}
	if got := b.sub.Seq(); got < newBase {
		t.Fatalf("B cursor = %d, want >= the post-recovery base (%d): a repainted "+
			"subscriber must sit at the live tail, not below base", got, newBase)
	}

	ch.emit(t, []byte("post-recovery-live"))
	mustData(t, b.sub, "post-recovery-live")
}

// TestPTYBrokerSubscribeKeepsReplayableBytesWhenRepaintUnavailable is the second
// codex P2 left live on the merged #3495 (#3731): the post-recovery re-check advanced
// the cursor to the live tail whenever a repaint was OWED, without waiting to find out
// whether one could actually be DELIVERED.
//
// The repaint is best-effort — `if snap, err := b.ch.Snapshot(); err == nil && …` —
// so the two can disagree. When they do, the advance has thrown away the subscriber's
// only remaining way to render the pane: the bytes between its pre-bring-up cursor and
// the new tail are still IN THE RING and still replayable, and the cursor jumped over
// them for a repaint that never arrives.
//
// The window is real because ensureCaptureStarted takes captureMu, which every
// capture transition holds while it works; the test holds it directly rather than
// staging a recovery, because the discard would make the same bytes unreachable for a
// different reason and hide what is under test.
//
// Fail-before/pass-after: before the fix B's cursor lands at the post-window tail with
// no repaint queued, so it receives NOTHING — a terminal that renders blank until the
// pane happens to emit again.
func TestPTYBrokerSubscribeKeepsReplayableBytesWhenRepaintUnavailable(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-A")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-A")
	ch.emit(t, []byte("early"))
	mustData(t, a, "early")

	// Stand in for a capture transition in flight: recovery, a teardown, or another
	// subscriber's bring-up all hold captureMu exactly this way, and B's
	// ensureCaptureStarted parks on it.
	br.captureMu.Lock()

	type subResult struct {
		sub *ptySub
		err error
	}
	bres := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(0)
		bres <- subResult{s, e}
	}()
	waitSubscriberCount(t, br, 2)

	// Output lands while B is parked, so the ring grows PAST the cursor B decided on.
	// A's own consumption is the barrier: it proves feed() completed, not merely that
	// the pipe write was read.
	const window = "BYTES-DURING-BRINGUP"
	ch.emit(t, []byte(window))
	mustData(t, a, window)

	// The pane vanishes under the capture-pane exec, so no repaint can be built.
	ch.mu.Lock()
	ch.snapshotErr = errors.New("capture-pane: no such pane")
	ch.mu.Unlock()

	br.captureMu.Unlock()

	var b subResult
	select {
	case b = <-bres:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe B never returned")
	}
	if b.err != nil {
		t.Fatalf("subscribe B: %v", b.err)
	}

	// No repaint was delivered, so the ring is all B has — and it still holds every
	// byte B is missing.
	ev, err := nextWithin(t, b.sub, 2*time.Second)
	if err != nil {
		t.Fatalf("B NextEvent (want the window bytes replayed from the ring): %v — the "+
			"cursor was advanced past them for a repaint that could not be built", err)
	}
	if ev.Kind != PTYData || string(ev.Data) != window {
		t.Fatalf("B first event = Kind %d Data %q, want PTYData %q", ev.Kind, ev.Data, window)
	}

	ch.emit(t, []byte("later"))
	mustData(t, b.sub, "later")
}

// TestPTYBrokerSubscribeAdvancesCursorOnceRepaintIsObtained is the other half of the
// contract the test above pins, and the reason the fix is "advance after a usable
// repaint has been obtained" rather than "never advance": when the snapshot DOES
// arrive it already reflects every byte in the ring, so replaying the window on top of
// it would duplicate output on a screen that was already correct (#1872).
//
// Same window, same bytes — only the snapshot's fate differs.
func TestPTYBrokerSubscribeAdvancesCursorOnceRepaintIsObtained(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-A")}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	mustRepaintContains(t, a, "SCREEN-A")
	ch.emit(t, []byte("early"))
	mustData(t, a, "early")

	br.captureMu.Lock()

	type subResult struct {
		sub *ptySub
		err error
	}
	bres := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(0)
		bres <- subResult{s, e}
	}()
	waitSubscriberCount(t, br, 2)

	const window = "BYTES-DURING-BRINGUP"
	ch.emit(t, []byte(window))
	mustData(t, a, window)

	ch.mu.Lock()
	ch.snapshot = []byte("SCREEN-WITH-EVERYTHING")
	ch.mu.Unlock()

	br.captureMu.Unlock()

	var b subResult
	select {
	case b = <-bres:
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe B never returned")
	}
	if b.err != nil {
		t.Fatalf("subscribe B: %v", b.err)
	}

	mustRepaintContains(t, b.sub, "SCREEN-WITH-EVERYTHING")
	if ev, err := nextWithin(t, b.sub, 250*time.Millisecond); err == nil {
		t.Fatalf("after the repaint B got Kind=%d Data=%q, want no event: the snapshot "+
			"already shows those bytes, so replaying them duplicates output", ev.Kind, ev.Data)
	}

	ch.emit(t, []byte("later"))
	mustData(t, b.sub, "later")
}
