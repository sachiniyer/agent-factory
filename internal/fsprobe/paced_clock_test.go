package fsprobe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A manual clock's timer fires when Advance reaches its deadline, never
// before, and the fired value is waiting when Advance returns.
func TestManualClock_FiresWhenAdvanceReachesTheDeadline(t *testing.T) {
	clock := NewManualClock(time.Now())
	timer := clock.NewTimer(50 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("the timer fired before its deadline")
	default:
	}
	clock.Advance(49 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("the timer fired before its deadline")
	default:
	}
	clock.Advance(time.Millisecond)
	select {
	case <-timer.C():
	default:
		t.Fatal("the timer did not fire at its deadline")
	}
}

// A fired timer re-arms on Reset from the clock's now, and Stop silences an
// armed one.
func TestManualClock_RearmsAndStops(t *testing.T) {
	clock := NewManualClock(time.Now())
	timer := clock.NewTimer(10 * time.Millisecond)
	clock.Advance(10 * time.Millisecond)
	select {
	case <-timer.C():
	default:
		t.Fatal("the timer did not fire at its deadline")
	}
	timer.Reset(10 * time.Millisecond)
	clock.Advance(9 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("the re-armed timer fired early")
	default:
	}
	timer.Stop()
	clock.Advance(2 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("the stopped timer fired")
	default:
	}
}

// Under a manual clock, a flight's verdict is what the clock declares: work
// whose steps each advance less than the bound answers, and a step still out
// when a whole bound has passed is the bounded non-answer (#7456).
func TestPaced_TheBoundRunsOnTheInstalledClock(t *testing.T) {
	clock := NewManualClock(time.Now())
	restoreClock := SetPacedClockForTest(clock)
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		restoreTimeout()
		restoreClock()
	}()

	got, err := Paced("question", t.Name()+"-answered", func(step func()) (int, error) {
		steps := 0
		for i := 0; i < 30; i++ {
			step()
			clock.Advance(10 * time.Millisecond)
			steps++
		}
		return steps, nil
	})
	require.NoError(t, err, "six bounds of steps that each answered within the bound")
	require.Equal(t, 30, got)

	release := make(chan struct{})
	_, err = Paced("question", t.Name()+"-stalled", func(step func()) (int, error) {
		step()
		clock.Advance(50 * time.Millisecond)
		<-release
		return 0, nil
	})
	require.True(t, errors.Is(err, ErrProbeTimedOut), "a step still out at the bound is the non-answer: %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	close(release)
	awaitPacedFlightsGone(t)
}
