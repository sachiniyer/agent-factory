package task

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// firstProbeOutcomes are the terminal answers one pane capture can give the
// readiness loop. TestWaitForReadyFirstProbeMatchesTickerProbe runs each of them
// at the look before the first tick AND at a tick, because the two sites must
// agree: a sentinel honored at one capture site and not the other is exactly the
// asymmetry #989 closed between the ticker and timeout branches.
var firstProbeOutcomes = []struct {
	name    string
	content string
	err     error
	check   func(t *testing.T, err error)
}{
	{
		name:    "ready prompt",
		content: "claude ready\n❯ ",
		check: func(t *testing.T, err error) {
			if err != nil {
				t.Fatalf("a pane showing the ready prompt must be READY, got %v", err)
			}
		},
	},
	{
		name: "session gone",
		err:  fmt.Errorf("capture: %w", tmux.ErrSessionGone),
		check: func(t *testing.T, err error) {
			if !errors.Is(err, tmux.ErrSessionGone) {
				t.Fatalf("a vanished session must surface ErrSessionGone, got %v", err)
			}
		},
	},
	{
		name:    "usage-limit banner",
		content: "Claude usage limit reached. Your limit will reset at 2pm (UTC)",
		check: func(t *testing.T, err error) {
			var limitErr *LimitReachedError
			if !errors.As(err, &limitErr) {
				t.Fatalf("a usage-limit banner must park with *LimitReachedError, got %v", err)
			}
		},
	},
}

// TestWaitForReadyFirstProbeMatchesTickerProbe pins #4464: the readiness loop
// looks at the pane once BEFORE its first tick. It used to look only when the
// ticker fired, so every wait — even against a pane that was ready, gone, or
// limit-parked from the start — cost at least one full poll interval (500ms).
//
// The virtual clock never fires on its own, so a result in the "before the
// first tick" rows is proof the look happened without one. The "at a tick" rows
// start from a booting pane and pin that the ticker branch still answers the same
// way; they pass before and after the change.
func TestWaitForReadyFirstProbeMatchesTickerProbe(t *testing.T) {
	defer setWaitLimitForTest(NewLimitDetector(nil), time.Now)()
	for _, oc := range firstProbeOutcomes {
		for _, atTick := range []bool{false, true} {
			name := oc.name + "/before the first tick"
			if atTick {
				name = oc.name + "/at a tick"
			}
			t.Run(name, func(t *testing.T) {
				clock := newObservedReadinessClock()
				var captures atomic.Int32
				inst := newPreviewInstance(t, func() (string, error) {
					if captures.Add(1) == 1 && atTick {
						return "booting…", nil
					}
					return oc.content, oc.err
				})
				done := make(chan error, 1)
				go func() {
					done <- waitForReadyOn(context.Background(), instanceReadinessTarget{inst: inst}, clock.clock())
				}()
				receiveReadinessEvent(t, clock.timers, "initial readiness timer")
				receiveReadinessEvent(t, clock.tickerStarted, "readiness ticker")

				var err error
				if atTick {
					err = awaitWithTicks(t, clock, done)
				} else {
					err = receiveReadinessEvent(t, done, "a result without any tick")
				}
				oc.check(t, err)

				want := int32(1)
				if atTick {
					want = 2
				}
				if got := captures.Load(); got != want {
					t.Fatalf("pane captures = %d, want %d", got, want)
				}
			})
		}
	}
}

// awaitWithTicks fires virtual ticks until the loop answers, so a test can pin
// the ticker branch without depending on how many looks precede the first tick.
func awaitWithTicks(t *testing.T, clock *observedReadinessClock, done <-chan error) error {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-done:
			return err
		case clock.ticks <- time.Time{}:
		case <-deadline:
			t.Fatal("WaitForReady never answered while ticks were firing")
			return nil
		}
	}
}

// TestWaitForReadyNotReadyAtFirstProbeKeepsPolling pins the other half: a look
// before the first tick that finds nothing conclusive — a booting pane or a
// transient capture failure — must fall through to the ticker, not end the wait.
func TestWaitForReadyNotReadyAtFirstProbeKeepsPolling(t *testing.T) {
	defer setWaitLimitForTest(NewLimitDetector(nil), time.Now)()
	for name, first := range map[string]func() (string, error){
		"booting pane":              func() (string, error) { return "booting…", nil },
		"transient capture failure": func() (string, error) { return "", errors.New("capture-pane: resource busy") },
	} {
		t.Run(name, func(t *testing.T) {
			clock := newObservedReadinessClock()
			var captures atomic.Int32
			inst := newPreviewInstance(t, func() (string, error) {
				if captures.Add(1) == 1 {
					return first()
				}
				return "claude ready\n❯ ", nil
			})
			done := make(chan error, 1)
			go func() {
				done <- waitForReadyOn(context.Background(), instanceReadinessTarget{inst: inst}, clock.clock())
			}()
			receiveReadinessEvent(t, clock.timers, "initial readiness timer")
			receiveReadinessEvent(t, clock.tickerStarted, "readiness ticker")

			// Let the first look run before offering a tick; if it wrongly ended
			// the wait, the answer is already here.
			for deadline := time.Now().Add(5 * time.Second); captures.Load() < 1; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("no look at the pane before the first tick")
				}
			}
			select {
			case err := <-done:
				t.Fatalf("an inconclusive first look ended the wait: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			clock.ticks <- time.Time{}
			if err := receiveReadinessEvent(t, done, "readiness after a tick"); err != nil {
				t.Fatalf("WaitForReady: %v", err)
			}
			if got := captures.Load(); got != 2 {
				t.Fatalf("pane captures = %d, want the first look plus one tick", got)
			}
		})
	}
}

// TestWaitForReadyCancelledBeforeFirstProbeCapturesNothing keeps the cancellation
// check where it was: an already-abandoned wait returns without starting a
// capture, the look before the first tick included.
func TestWaitForReadyCancelledBeforeFirstProbeCapturesNothing(t *testing.T) {
	var captures atomic.Int32
	inst := newPreviewInstance(t, func() (string, error) {
		captures.Add(1)
		return "claude ready\n❯ ", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForReadyOn(ctx, instanceReadinessTarget{inst: inst}, newObservedReadinessClock().clock())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait must return context.Canceled, got %v", err)
	}
	if got := captures.Load(); got != 0 {
		t.Fatalf("a cancelled wait started %d capture(s), want none", got)
	}
}
