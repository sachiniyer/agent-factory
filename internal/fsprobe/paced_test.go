package fsprobe

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func awaitPacedFlightsGone(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		pacedFlights.Lock()
		defer pacedFlights.Unlock()
		return len(pacedFlights.byKey) == 0
	}, 5*time.Second, time.Millisecond)
}

// A step that does not answer is the bounded non-answer, never a hang, and a
// retry of the same question about the same key is refused while it is stuck.
// Another question about that key, or the same question about another key,
// is judged on its own (#7383).
func TestPaced_BoundsAStepThatDoesNotAnswer(t *testing.T) {
	release := make(chan struct{})
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		close(release)
		awaitPacedFlightsGone(t)
		restoreTimeout()
	}()
	stuck := func(step func()) (string, error) {
		step()
		<-release
		return "late", nil
	}

	got, err := Paced("question a", "key", stuck)
	require.Empty(t, got)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "a step that does not answer is the bounded non-answer: %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	require.Contains(t, err.Error(), "question a")

	start := time.Now()
	_, err = Paced("question a", "key", func(func()) (string, error) { return "fresh", nil })
	require.True(t, errors.Is(err, ErrProbeTimedOut), "a retry while it is stuck is refused: %v", err)
	require.Less(t, time.Since(start), 40*time.Millisecond, "the refusal does not wait out another bound")

	got, err = Paced("question b", "key", func(func()) (string, error) { return "b", nil })
	require.NoError(t, err, "another question about the key is judged on its own")
	require.Equal(t, "b", got)
	got, err = Paced("question a", "another key", func(func()) (string, error) { return "a", nil })
	require.NoError(t, err, "the question about another key is judged on its own")
	require.Equal(t, "a", got)
}

// The bound is per step. Work whose every step answers well within the bound
// answers, however many bounds its whole run takes. The steps' cost is
// declared on a manual clock (#7456), so no scheduler can stretch one past
// the bound.
func TestPaced_AnswersWhateverItsTotalTimeWhenEveryStepAnswers(t *testing.T) {
	clock := NewManualClock(time.Now())
	restoreClock := SetPacedClockForTest(clock)
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		restoreTimeout()
		restoreClock()
	}()
	start := clock.Now()
	got, err := Paced("question", t.Name(), func(step func()) (int, error) {
		steps := 0
		for clock.Since(start) < 250*time.Millisecond {
			step()
			clock.Advance(10 * time.Millisecond)
			steps++
		}
		return steps, nil
	})
	require.NoError(t, err, "five bounds of steps that each answered within the bound")
	require.Greater(t, got, 10)
}

// Work that answers returns its answer and its error as they are.
func TestPaced_ReportsTheAnswer(t *testing.T) {
	got, err := Paced("question", t.Name(), func(func()) (int, error) { return 7, nil })
	require.NoError(t, err)
	require.Equal(t, 7, got)
	_, err = Paced("question", t.Name(), func(func()) (int, error) { return 0, os.ErrNotExist })
	require.True(t, os.IsNotExist(err), "the work's own error reaches the caller unwrapped: %v", err)
}
