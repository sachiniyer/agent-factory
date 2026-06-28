package daemon

import (
	"testing"
	"time"
)

// TestWatcherBackoffResetsAfterHealthyRun pins the restart-backoff state
// machine (#1005). A run that stays healthy for a whole crash window resets the
// chain to baseBackoff and must NOT advance it that cycle, so the next quick
// failure restarts the documented 1s→2s→4s… sequence at baseBackoff instead of
// jumping to 2*baseBackoff. The loop below mirrors taskWatcher.run: it carries
// the backoff forward across runs and asserts the wait emitted for each
// restart. Pure and hermetic — no watch processes, no wall-clock sleeps.
func TestWatcherBackoffResetsAfterHealthyRun(t *testing.T) {
	const (
		base = time.Second
		max  = 5 * time.Minute
	)

	type step struct {
		healthy  bool
		wantWait time.Duration
	}
	steps := []step{
		// Fresh crash chain doubles from the base: 1s → 2s → 4s → 8s.
		{false, 1 * time.Second},
		{false, 2 * time.Second},
		{false, 4 * time.Second},
		{false, 8 * time.Second},
		// A run that stayed healthy for the crash window resets the wait to
		// the base …
		{true, 1 * time.Second},
		// … and the NEXT quick failure must restart the chain at the base.
		// This is the #1005 regression: the old unconditional `backoff *= 2`
		// made this wait 2s. It then doubles afresh.
		{false, 1 * time.Second},
		{false, 2 * time.Second},
		{false, 4 * time.Second},
	}

	backoff := base
	for i, s := range steps {
		wait, next := nextBackoff(backoff, base, max, s.healthy)
		if wait != s.wantWait {
			t.Fatalf("step %d (healthy=%v): wait = %s, want %s", i, s.healthy, wait, s.wantWait)
		}
		backoff = next
	}
}

// TestWatcherBackoffDoublesToCapOnUnhealthyRuns pins the unhealthy path:
// repeated quick failures double the wait and saturate at maxBackoff, never
// overshooting it.
func TestWatcherBackoffDoublesToCapOnUnhealthyRuns(t *testing.T) {
	const (
		base = time.Second
		max  = 5 * time.Minute
	)

	backoff := base
	for i := 0; i < 20; i++ {
		wait, next := nextBackoff(backoff, base, max, false)
		if wait > max {
			t.Fatalf("iteration %d: wait %s exceeded the %s cap", i, wait, max)
		}
		if next > max {
			t.Fatalf("iteration %d: carried backoff %s exceeded the %s cap", i, next, max)
		}
		backoff = next
	}
	if backoff != max {
		t.Fatalf("backoff settled at %s, want the %s cap", backoff, max)
	}
	// Once pinned at the cap, every further unhealthy cycle stays there.
	if wait, next := nextBackoff(max, base, max, false); wait != max || next != max {
		t.Fatalf("at cap: wait/next = %s/%s, want %s/%s", wait, next, max, max)
	}
}

// TestWatcherBackoffHealthyRunResetsFromCap pins the reset even from the top of
// the chain: a healthy run after the backoff has climbed to the cap still drops
// the wait straight back to the base and carries the base forward.
func TestWatcherBackoffHealthyRunResetsFromCap(t *testing.T) {
	const (
		base = time.Second
		max  = 5 * time.Minute
	)
	if wait, next := nextBackoff(max, base, max, true); wait != base || next != base {
		t.Fatalf("healthy reset from cap: wait/next = %s/%s, want %s/%s", wait, next, base, base)
	}
}
