package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// fakeBareStreamer stands in for session.BareSessionStreamer so the hub's whole
// lifecycle (reuse, subscriber counting, grace reap, refusal after close) is driven
// with no tmux. It records how many times it was closed.
type fakeBareStreamer struct {
	mu     sync.Mutex
	closed int
}

func (f *fakeBareStreamer) Subscribe(since session.Seq) (session.PTYSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed > 0 {
		return nil, errors.New("bare streamer closed")
	}
	return &fakeSub{ch: make(chan session.PTYEvent)}, nil
}
func (f *fakeBareStreamer) Input(b []byte) error           { return nil }
func (f *fakeBareStreamer) Resize(rows, cols uint16) error { return nil }
func (f *fakeBareStreamer) Close() {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
}
func (f *fakeBareStreamer) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// hubHarness wires a hub to fakes instead of the manager, with a short grace window
// and thread-safe counters. Each spawn produces a FRESH streamer (tracked), so a test
// can tell one assistant's streamer from the next.
type hubHarness struct {
	hub *configAssistantHub

	mu        sync.Mutex
	spawns    int
	streamers []*fakeBareStreamer
	reaped    []string

	// blockSpawn, when non-nil, makes spawn block (honoring ctx) until it is closed —
	// so a test can force a DELETE to land mid-spawn. spawnEntered is signaled once the
	// spawn is actually parked in the block.
	blockSpawn   chan struct{}
	spawnEntered chan struct{}
}

func newHubHarness(t *testing.T, grace time.Duration) *hubHarness {
	t.Helper()
	h := &hubHarness{}
	h.hub = &configAssistantHub{graceWindow: grace}
	h.hub.spawn = func(ctx context.Context) (string, bareStreamer, error) {
		h.mu.Lock()
		block, entered := h.blockSpawn, h.spawnEntered
		h.mu.Unlock()
		if block != nil {
			if entered != nil {
				entered <- struct{}{}
			}
			select {
			case <-block:
			case <-ctx.Done():
				return "", nil, ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		st := &fakeBareStreamer{}
		h.mu.Lock()
		h.spawns++
		n := h.spawns
		h.streamers = append(h.streamers, st)
		h.mu.Unlock()
		return fmt.Sprintf("af_af-config-%d", n), bareStreamer(st), nil
	}
	h.hub.reapFn = func(name string) error {
		h.mu.Lock()
		h.reaped = append(h.reaped, name)
		h.mu.Unlock()
		return nil
	}
	return h
}

func (h *hubHarness) spawnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spawns
}

func (h *hubHarness) reapCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reaped)
}

func (h *hubHarness) lastStreamer() *fakeBareStreamer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.streamers) == 0 {
		return nil
	}
	return h.streamers[len(h.streamers)-1]
}

func (h *hubHarness) streamerAt(i int) *fakeBareStreamer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streamers[i]
}

// waitFor polls cond until true or the deadline, so a grace-timer reap (which fires
// on its own goroutine) is observed without a fixed sleep.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// graceArmedForTest reports whether the idle reaper's timer is currently armed. Lets
// a test assert arm/cancel decisions directly instead of racing a wall-clock timer.
func (h *configAssistantHub) graceArmedForTest() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.grace != nil
}

// fireGraceForTest synchronously runs the armed idle reaper as if its timer had
// expired, and reports whether one was armed. It drives the SAME onGraceExpired the
// real time.AfterFunc invokes, so tests exercise the true reap path deterministically
// — no dependency on a 40ms timer winning a race against the next Go statement, which
// a scheduler/GC stall on a contended box would lose (#2467 review round 2). Tests
// that use it set a long window so the real timer never self-fires mid-test.
func (h *configAssistantHub) fireGraceForTest() bool {
	h.mu.Lock()
	if h.grace == nil {
		h.mu.Unlock()
		return false
	}
	h.grace.Stop()
	h.grace = nil
	gen := h.gen
	h.mu.Unlock()
	h.onGraceExpired(gen)
	return true
}

// TestConfigAssistantHub_EnsureSpawnsOnceAndReuses pins the idempotency: a second
// ensure while one is running reuses it rather than spawning a second assistant.
func TestConfigAssistantHub_EnsureSpawnsOnceAndReuses(t *testing.T) {
	h := newHubHarness(t, time.Minute)
	ctx := context.Background()

	name1, err := h.hub.ensure(ctx)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	name2, err := h.hub.ensure(ctx)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if name1 != name2 {
		t.Fatalf("reuse returned a different name: %q vs %q", name1, name2)
	}
	if got := h.spawnCount(); got != 1 {
		t.Fatalf("spawns=%d, want 1 (the second ensure must reuse)", got)
	}
}

// TestConfigAssistantHub_SubscribeWithNoAssistant is the stream route's 404 source.
func TestConfigAssistantHub_SubscribeWithNoAssistant(t *testing.T) {
	h := newHubHarness(t, time.Minute)
	if _, _, err := h.hub.subscribe(0); !errors.Is(err, errNoConfigAssistant) {
		t.Fatalf("subscribe with no assistant err = %v, want errNoConfigAssistant", err)
	}
}

// TestConfigAssistantHub_BarePostNeverSubscribedIsReaped is the #2467 P1 regression:
// a POST that spawns the assistant with NO stream ever opened (a failed WS dial, a
// tab closed in the ~60s gap, a scripted curl) must still be reaped after the grace
// window — not leak a permission-skipping agent for the daemon's life. The idle timer
// therefore has to arm on the SPAWN path, not only when a subscriber leaves.
func TestConfigAssistantHub_BarePostNeverSubscribedIsReaped(t *testing.T) {
	h := newHubHarness(t, 30*time.Millisecond)
	if _, err := h.hub.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// No subscribe, ever.
	if !waitFor(t, 2*time.Second, func() bool { return h.reapCount() == 1 }) {
		t.Fatalf("a POST that was never streamed leaked (reaps=%d); the idle reaper must arm on spawn", h.reapCount())
	}
	if got := h.lastStreamer().closeCount(); got != 1 {
		t.Fatalf("streamer closeCount=%d after the bare-POST idle reap, want 1", got)
	}
	if _, _, err := h.hub.subscribe(0); !errors.Is(err, errNoConfigAssistant) {
		t.Fatalf("subscribe after the bare-POST reap err = %v, want errNoConfigAssistant", err)
	}
}

// TestConfigAssistantHub_IdleReapAfterLastSubscriber: no reap while a subscriber is
// live; reaped once the last one leaves. Deterministic — a live subscriber cancels the
// timer (asserted directly), and the last-detach reap is driven by fireGraceForTest,
// not a wall-clock race.
func TestConfigAssistantHub_IdleReapAfterLastSubscriber(t *testing.T) {
	h := newHubHarness(t, time.Hour) // long window: the real timer never self-fires mid-test
	ctx := context.Background()

	if _, err := h.hub.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sub, _, err := h.hub.subscribe(0) // a live subscriber cancels the spawn's idle timer
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if h.hub.graceArmedForTest() {
		t.Fatal("idle reaper armed while a subscriber is live")
	}
	if got := h.reapCount(); got != 0 {
		t.Fatalf("reaped while a subscriber was live (reaps=%d)", got)
	}
	// Last subscriber leaves → arm → (fire) → reap.
	if err := sub.Close(); err != nil {
		t.Fatalf("sub.Close: %v", err)
	}
	if !h.hub.graceArmedForTest() {
		t.Fatal("idle reaper did not arm after the last subscriber left")
	}
	if !h.hub.fireGraceForTest() {
		t.Fatal("no idle timer to fire")
	}
	if got := h.reapCount(); got != 1 {
		t.Fatalf("assistant was not idle-reaped after the window fired (reaps=%d)", got)
	}
	if got := h.lastStreamer().closeCount(); got != 1 {
		t.Fatalf("streamer closeCount=%d after idle reap, want 1", got)
	}
}

// TestConfigAssistantHub_NewSubscriberCancelsPendingReap: a subscriber arriving during
// the window cancels the pending reap; only its own departure re-arms it. Deterministic
// via the armed/fire hooks.
func TestConfigAssistantHub_NewSubscriberCancelsPendingReap(t *testing.T) {
	h := newHubHarness(t, time.Hour)
	ctx := context.Background()

	if _, err := h.hub.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first, _, err := h.hub.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := first.Close(); err != nil { // arms the grace timer
		t.Fatalf("first.Close: %v", err)
	}
	if !h.hub.graceArmedForTest() {
		t.Fatal("reaper did not arm after the first subscriber left")
	}
	second, _, err := h.hub.subscribe(0) // reconnect cancels the pending reap
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	if h.hub.graceArmedForTest() {
		t.Fatal("reconnect did not cancel the pending reap")
	}
	// The reconnect leaving re-arms and reaps.
	if err := second.Close(); err != nil {
		t.Fatalf("second.Close: %v", err)
	}
	if !h.hub.fireGraceForTest() {
		t.Fatal("reaper did not re-arm after the reconnect left")
	}
	if got := h.reapCount(); got != 1 {
		t.Fatalf("assistant was not reaped after the reconnect left (reaps=%d)", got)
	}
}

// TestConfigAssistantHub_EnsureDuringGraceReArms is the #2467 P1-failure-2 regression
// (and the correction of the test that used to PIN the bug): a POST during the grace
// window REUSES the assistant and RE-ARMS the reaper — it does not cancel it. So
// open → close → warm-POST → walk away (never re-subscribe) is STILL reaped when the
// fresh window fires, not leaked. Deterministic via the fire hook.
func TestConfigAssistantHub_EnsureDuringGraceReArms(t *testing.T) {
	h := newHubHarness(t, time.Hour)
	ctx := context.Background()

	name1, err := h.hub.ensure(ctx)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sub, _, err := h.hub.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.Close(); err != nil { // arm grace
		t.Fatalf("sub.Close: %v", err)
	}
	name2, err := h.hub.ensure(ctx) // POST during grace → reuse + RE-ARM
	if err != nil {
		t.Fatalf("ensure during grace: %v", err)
	}
	if name1 != name2 {
		t.Fatalf("ensure during grace spawned a new assistant: %q vs %q", name1, name2)
	}
	if got := h.spawnCount(); got != 1 {
		t.Fatalf("spawns=%d, want 1 (ensure during grace must reuse)", got)
	}
	// The correct behavior: the reaper is still armed (re-armed, not cancelled), and
	// with no subscriber it reaps when it fires.
	if !h.hub.graceArmedForTest() {
		t.Fatal("a POST during grace cancelled the reaper instead of re-arming it")
	}
	if !h.hub.fireGraceForTest() {
		t.Fatal("no idle timer to fire after the warm POST")
	}
	if got := h.reapCount(); got != 1 {
		t.Fatalf("a POST during grace with no re-subscribe leaked (reaps=%d); it must RE-ARM, not cancel", got)
	}
}

// TestConfigAssistantHub_ExplicitReap is the DELETE path: it tears the assistant down
// at once, closing the streamer, and is a no-op when none is running.
func TestConfigAssistantHub_ExplicitReap(t *testing.T) {
	h := newHubHarness(t, time.Minute)
	ctx := context.Background()

	if err := h.hub.reap(); err != nil { // no-op when none running
		t.Fatalf("reap with no assistant: %v", err)
	}
	if got := h.reapCount(); got != 0 {
		t.Fatalf("reap with no assistant called the reaper (reaps=%d)", got)
	}

	if _, err := h.hub.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := h.hub.reap(); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := h.reapCount(); got != 1 {
		t.Fatalf("explicit reap count=%d, want 1", got)
	}
	if got := h.lastStreamer().closeCount(); got != 1 {
		t.Fatalf("streamer closeCount=%d after explicit reap, want 1", got)
	}
	if _, _, err := h.hub.subscribe(0); !errors.Is(err, errNoConfigAssistant) {
		t.Fatalf("subscribe after explicit reap err = %v, want errNoConfigAssistant", err)
	}
}

// TestConfigAssistantHub_DeleteDuringColdSpawnIsHonored is the #2467 P2 regression: a
// DELETE that lands while a ~60s cold spawn is in flight must not be silently dropped.
// It cancels the spawn (so the wait aborts) and ensure() returns without storing an
// assistant the user asked to remove.
func TestConfigAssistantHub_DeleteDuringColdSpawnIsHonored(t *testing.T) {
	h := newHubHarness(t, time.Minute)
	h.blockSpawn = make(chan struct{})
	h.spawnEntered = make(chan struct{}, 1)

	type res struct {
		name string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		name, err := h.hub.ensure(context.Background())
		done <- res{name, err}
	}()

	select {
	case <-h.spawnEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("spawn never entered the block")
	}
	// DELETE during the spawn.
	if err := h.hub.reap(); err != nil {
		t.Fatalf("reap during spawn: %v", err)
	}

	select {
	case r := <-done:
		// A create aborted by a concurrent DELETE is RETRYABLE, not "resource gone":
		// ensure must classify it as the aborted-create sentinel (→ 409), never the raw
		// context error or errNoConfigAssistant (→ the stream route's settle-and-stop
		// 404, which a client reads as "give up").
		if !errors.Is(r.err, errConfigAssistantSpawnAborted) {
			t.Fatalf("ensure for a DELETE-cancelled spawn err = %v (name %q), want errConfigAssistantSpawnAborted", r.err, r.name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ensure did not return after the DELETE cancelled the spawn")
	}
	// Nothing was stored, so a stream request finds no assistant.
	if _, _, err := h.hub.subscribe(0); !errors.Is(err, errNoConfigAssistant) {
		t.Fatalf("subscribe after a DELETE-cancelled spawn err = %v, want errNoConfigAssistant", err)
	}
}

// TestConfigAssistantHub_DeleteRacingSpawnStoreIsHonored covers the razor-thin variant
// of #2467 P2: the spawn COMPLETED but a DELETE ran before ensure could store it (so
// reap saw current==nil and had nothing to tear down). ensure re-checks the reap epoch
// and tears down what it spawned rather than leaking it.
func TestConfigAssistantHub_DeleteRacingSpawnStoreIsHonored(t *testing.T) {
	h := newHubHarness(t, time.Minute)
	// After the spawn returns success, before ensure stores it, a DELETE lands.
	h.hub.afterSpawnHook = func() { _ = h.hub.reap() }

	name, err := h.hub.ensure(context.Background())
	if !errors.Is(err, errConfigAssistantSpawnAborted) {
		t.Fatalf("ensure racing a store-time DELETE err = %v (name %q), want errConfigAssistantSpawnAborted", err, name)
	}
	// The just-spawned assistant was torn down: its streamer closed and its session
	// reaped (by ensure, since reap itself saw nothing to reap).
	if got := h.lastStreamer().closeCount(); got != 1 {
		t.Fatalf("orphaned assistant streamer closeCount=%d, want 1", got)
	}
	if got := h.reapCount(); got != 1 {
		t.Fatalf("orphaned assistant reap count=%d, want 1 (ensure must reap what it spawned)", got)
	}
	if _, _, err := h.hub.subscribe(0); !errors.Is(err, errNoConfigAssistant) {
		t.Fatalf("subscribe after the store-time DELETE err = %v, want errNoConfigAssistant", err)
	}
}

// TestConfigAssistantHub_StaleSubscriptionDoesNotKillNextAssistant is the #2467 P3
// regression: a subscription counted against a reaped assistant must not decrement —
// or arm the reaper against — the NEXT assistant, which would kill a live browser
// terminal mid-conversation. Deterministic: it asserts A2's reaper is NOT armed after
// the stale close, rather than sleeping past a window.
func TestConfigAssistantHub_StaleSubscriptionDoesNotKillNextAssistant(t *testing.T) {
	h := newHubHarness(t, time.Hour)
	ctx := context.Background()

	// Assistant A1 with one subscriber.
	if _, err := h.hub.ensure(ctx); err != nil {
		t.Fatalf("ensure A1: %v", err)
	}
	staleSub, _, err := h.hub.subscribe(0) // counted against A1
	if err != nil {
		t.Fatalf("subscribe A1: %v", err)
	}
	// Reap A1 (explicit DELETE) and spawn A2 with its own live subscriber.
	if err := h.hub.reap(); err != nil {
		t.Fatalf("reap A1: %v", err)
	}
	if _, err := h.hub.ensure(ctx); err != nil {
		t.Fatalf("ensure A2: %v", err)
	}
	liveSub, _, err := h.hub.subscribe(0) // counted against A2
	if err != nil {
		t.Fatalf("subscribe A2: %v", err)
	}
	// The A1 subscription closes LATE. It must not touch A2's count or reaper.
	if err := staleSub.Close(); err != nil {
		t.Fatalf("staleSub.Close: %v", err)
	}
	// A2 has a live subscriber, so its reaper must NOT be armed, and only A1's explicit
	// reap should have happened.
	if h.hub.graceArmedForTest() {
		t.Fatal("a stale subscription's close armed the reaper against a live assistant")
	}
	if got := h.reapCount(); got != 1 {
		t.Fatalf("stale subscription reaped a live assistant (reaps=%d, want 1 = only A1's explicit reap)", got)
	}
	if got := h.streamerAt(1).closeCount(); got != 0 {
		t.Fatalf("A2 streamer was closed (closeCount=%d) by a stale subscription's decrement", got)
	}
	_ = liveSub.Close()
}

// TestConfigAssistantHub_UnavailableWhenUnwired proves an unwired build (nil request
// builder) surfaces errConfigAssistantUnavailable rather than spawning nothing.
func TestConfigAssistantHub_UnavailableWhenUnwired(t *testing.T) {
	prev := configAssistantRequestBuilder
	configAssistantRequestBuilder = nil
	defer func() { configAssistantRequestBuilder = prev }()

	h := newConfigAssistantHub(&Manager{})
	if _, err := h.ensure(context.Background()); !errors.Is(err, errConfigAssistantUnavailable) {
		t.Fatalf("ensure with no request builder err = %v, want errConfigAssistantUnavailable", err)
	}
}
