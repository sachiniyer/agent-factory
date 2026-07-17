package daemon

import (
	"testing"
	"time"
)

// A park is a HOLD, not a failure, and the drainer re-checks it every
// drainBaseBackoff (10s). Logging every re-check emitted ~360 identical lines an
// hour about a state that had not changed — the #1910 hot-loop shape, and the
// kind of repetition that trains people to scroll past the log entirely.
//
// These pin the throttle: one notice on entering a park, a bounded reminder while
// it holds, and never a swallowed transition — a park that is genuinely NEW must
// always announce itself, or the throttle would trade noise for silence.

func TestParkLogThrottle(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	t.Run("the first notice of an episode always logs", func(t *testing.T) {
		var p parkLogThrottle
		if !p.allow("cap", base) {
			t.Fatal("entering a park must announce itself")
		}
	})

	t.Run("an unchanged park is throttled inside the window", func(t *testing.T) {
		var p parkLogThrottle
		p.allow("cap", base)
		// The drainer's real cadence: a re-check every 10s for the whole window.
		for elapsed := watcherDrainBaseBackoff; elapsed < watcherParkLogInterval; elapsed += watcherDrainBaseBackoff {
			if p.allow("cap", base.Add(elapsed)) {
				t.Fatalf("an unchanged park re-logged after %s, inside the %s window", elapsed, watcherParkLogInterval)
			}
		}
	})

	t.Run("a long park reminds once per interval", func(t *testing.T) {
		var p parkLogThrottle
		p.allow("cap", base)
		if !p.allow("cap", base.Add(watcherParkLogInterval)) {
			t.Fatal("a park still holding after the interval must remind, not go silent forever")
		}
		// And the reminder re-arms the window rather than logging every call after it.
		if p.allow("cap", base.Add(watcherParkLogInterval+time.Second)) {
			t.Fatal("the reminder must restart the window")
		}
	})

	t.Run("a changed reason logs immediately", func(t *testing.T) {
		var p parkLogThrottle
		p.allow("cap", base)
		if !p.allow("attached", base.Add(time.Second)) {
			t.Fatal("at-cap and target-attached are different facts; the transition is the interesting moment and must never be throttled")
		}
	})

	t.Run("reset re-arms so the next episode is not swallowed", func(t *testing.T) {
		var p parkLogThrottle
		p.allow("cap", base)
		// A delivery landed: the park ended.
		p.reset()
		// A new park, well inside the throttle window of the previous one.
		if !p.allow("cap", base.Add(time.Second)) {
			t.Fatal("a park entered after a successful delivery is a NEW episode and must announce itself")
		}
	})
}
