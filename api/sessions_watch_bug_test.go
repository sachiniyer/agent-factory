package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// These tests guard the wall-clock contract of `af sessions watch <title>`:
// --timeout is a hard return bound, not an advisory one. watchForReady must
// never sleep past the deadline, so an operator who picks an --interval that
// does not divide --timeout (including --interval > --timeout, which
// validateWatchFlags accepts) still gets the process back within the advertised
// window. The fleet loop (watchFleet) was already clamped for this in
// api/sessions_watch_fleet.go; these tests pin the same property for the
// single-title loop so the two paths cannot diverge again.

// TestBug_NeverSleepsPastDeadline_SingleTitle is the single-title parallel of
// TestWatchFleet_NeverSleepsPastTheDeadline (api/sessions_watch_fleet_test.go).
// --interval and --timeout are validated independently, so
// `--timeout 5s --interval 1h` is accepted. The watch must clamp the final
// sleep to the remaining timeout rather than blocking for the full interval,
// otherwise it would poll once, block for an hour, and then report a
// five-second timeout — making the advertised bound a fiction.
func TestBug_NeverSleepsPastDeadline_SingleTitle(t *testing.T) {
	clock := time.Unix(0, 0)
	var slept []time.Duration
	deps := watchDeps{
		get:      func(string) (*session.InstanceData, error) { return running(), nil },
		interval: time.Hour,
		timeout:  5 * time.Second,
		now:      func() time.Time { return clock },
		sleep:    func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) },
	}
	_, err := watchForReady(deps, "x")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out after 5s") {
		t.Fatalf("expected 'timed out after 5s', got: %v", err)
	}
	// The fleet equivalent asserts slept == [5s]: the wait is clamped to the
	// remaining timeout, not the full hour. The single-title path must do the
	// same so both watch surfaces honor the --timeout bound identically.
	if len(slept) != 1 || slept[0] != 5*time.Second {
		t.Fatalf("single-title slept %v; want [5s] (clamped to the remaining timeout, as the fleet path does)", slept)
	}
	if !clock.Equal(time.Unix(5, 0)) {
		t.Fatalf("single-title returned at %v; want 5s (the advertised --timeout bound)", clock)
	}
}

// TestBug_OvershootsCloseGap guards the realistic close-gap regime: --interval
// just below --timeout, where --interval does not divide --timeout. Before the
// clamp the loop slept a full interval at t=29s, polled next at t=58s, and
// returned ~28s past the 30s deadline. After the clamp the t=29s sleep is
// shortened to the remaining 1s, the next poll lands at t=30s, and the timeout
// fires on the advertised deadline.
func TestBug_OvershootsCloseGap(t *testing.T) {
	clock := time.Unix(0, 0)
	var slept []time.Duration
	deps := watchDeps{
		get:      func(string) (*session.InstanceData, error) { return running(), nil },
		interval: 29 * time.Second,
		timeout:  30 * time.Second,
		now:      func() time.Time { return clock },
		sleep:    func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) },
	}
	_, err := watchForReady(deps, "x")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out after 30s") {
		t.Fatalf("expected 'timed out after 30s', got: %v", err)
	}
	// Clamped: poll 0 (sleep 29s), poll 29s (sleep clamped to 1s -> next at 30s),
	// poll 30s -> timed out. Return wall-clock 30s, on the advertised deadline.
	if !clock.Equal(time.Unix(30, 0)) {
		t.Fatalf("single-title returned at %v; want 30s (clamped to the advertised --timeout deadline, not 58s)", clock)
	}
	// The second sleep is clamped to the remaining 1s rather than the full 29s.
	if len(slept) != 2 || slept[0] != 29*time.Second || slept[1] != 1*time.Second {
		t.Fatalf("single-title slept %v; want [29s, 1s] (final sleep clamped to the remaining timeout)", slept)
	}
}

// TestBug_TimeoutZeroWaitsForever confirms --timeout 0 (the "wait forever"
// sentinel) is honored: the clamp's d.timeout > 0 gate stays false, so the
// loop sleeps the FULL interval every time and never hits the timeout branch.
// A bounded harness polls a fixed number of times then reports the result; if
// the loop had wrongly treated timeout=0 as a finite deadline, the harness
// would see a timeout error instead of the sentinel it returns on exhaustion.
// This complements TestValidateWatchFlags/zero-timeout-waits-forever (which only
// confirms --timeout 0 is ACCEPTED) by confirming it BEHAVES as wait-forever.
func TestBug_TimeoutZeroWaitsForever(t *testing.T) {
	clock := time.Unix(0, 0)
	var slept []time.Duration
	polls := 0
	const maxPolls = 50 // bounded harness: if it didn't time out by poll 50, it never will
	deps := watchDeps{
		get: func(string) (*session.InstanceData, error) {
			polls++
			if polls > maxPolls {
				// Signal exhaustion. Because watchForReady has no "give up after N
				// polls" path of its own (only the timeout branch), the only way to
				// reach this return-nil path is if the loop never matched a timeout
				// branch — i.e. timeout=0 was honored.
				return nil, fmt.Errorf("harness-exhausted-after-%d-polls", maxPolls)
			}
			return running(), nil
		},
		interval: 3 * time.Second,
		timeout:  0, // wait forever
		now:      func() time.Time { return clock },
		sleep:    func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) },
	}
	_, err := watchForReady(deps, "x")
	// The harness-exhaustion sentinel is the expected outcome: the loop polled
	// maxPolls+1 times without ever returning a timeout error, proving the
	// deadline guard (gated on d.timeout > 0) never fired.
	if err == nil {
		t.Fatal("expected harness-exhaustion sentinel, got nil (the loop returned a ready snapshot it should never have)")
	}
	if !strings.Contains(err.Error(), "harness-exhausted") {
		t.Fatalf("expected harness-exhaustion sentinel (timeout=0 should never fire the timeout branch), got: %v", err)
	}
	if polls != maxPolls+1 {
		t.Fatalf("expected %d polls, got %d (the loop should have polled until the harness gave up)", maxPolls+1, polls)
	}
	// Every sleep is the full 3s interval — the clamp never engaged because
	// d.timeout == 0.
	for i, s := range slept {
		if s != 3*time.Second {
			t.Fatalf("sleep[%d] = %v; want 3s (the clamp must not engage when --timeout 0, so every sleep is the full interval)", i, s)
		}
	}
}
