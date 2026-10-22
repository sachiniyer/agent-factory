package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func moveLiveness(i *Instance, lv Liveness) {
	i.mu.Lock()
	defer i.mu.Unlock()
	before, op, reset := i.lifecycleStateLocked()
	i.liveness = lv
	i.noteStateChangeLocked(before, op, reset)
}

func TestAttentionClearsOnlyWhenWorkStartsAfterIdle(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("  pick A or B  ", time.Unix(100, 0))
	require.NoError(t, err)
	require.Equal(t, "pick A or B", i.Attention().Reason)

	moveLiveness(i, LiveRunning)
	require.True(t, i.HasAttention(), "no liveness change keeps the request")

	moveLiveness(i, LiveReady)
	require.True(t, i.HasAttention(), "going idle is the agent waiting, not an answer")
	require.True(t, i.Attention().SeenIdle)

	moveLiveness(i, LiveRunning)
	require.False(t, i.HasAttention(), "work after idle is the user's answer")
}

func TestAttentionClearsWhenTheAgentStops(t *testing.T) {
	for _, lv := range []Liveness{LiveLost, LiveDead, LiveArchived, LiveLimitReached} {
		i := &Instance{Title: "w", liveness: LiveRunning}
		_, err := i.SetAttention("", time.Unix(100, 0))
		require.NoError(t, err)
		moveLiveness(i, lv)
		require.False(t, i.HasAttention(), "liveness %v", lv)
	}
}

func TestAttentionRoundTripsThroughStorage(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("need a token", time.Unix(100, 0))
	require.NoError(t, err)
	data := i.ToInstanceData().ForStorage()
	require.NotNil(t, data.Attention)
	require.Equal(t, "need a token", data.Attention.Reason)

	previous, cleared := i.ClearAttention(time.Time{})
	require.True(t, cleared)
	i.RestoreAttention(previous)
	require.Equal(t, "need a token", i.Attention().Reason)
}

func TestNormalizeAttentionReasonCapsLength(t *testing.T) {
	long := strings.Repeat("é", MaxAttentionReasonLen+10)
	require.Len(t, []rune(NormalizeAttentionReason(long)), MaxAttentionReasonLen)
}

func TestSetAttentionRefusesAStoppedAgent(t *testing.T) {
	for _, lv := range []Liveness{LiveLost, LiveDead, LiveArchived, LiveLimitReached} {
		i := &Instance{Title: "w", liveness: lv}
		_, err := i.SetAttention("", time.Unix(100, 0))
		require.ErrorIs(t, err, ErrAttentionNotLive, "liveness %v", lv)
		require.False(t, i.HasAttention(), "liveness %v", lv)
	}
}

func TestSetAttentionFencesEarlierStatusObservations(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, before := i.InFlightOpAndEpoch()
	_, err := i.SetAttention("", time.Unix(100, 0))
	require.NoError(t, err)
	_, after := i.InFlightOpAndEpoch()
	require.NotEqual(t, before, after)
}

func TestClearAttentionWithSetAtLeavesANewerRequest(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("A", time.Unix(100, 0))
	require.NoError(t, err)
	_, err = i.SetAttention("B", time.Unix(200, 0))
	require.NoError(t, err)

	_, cleared := i.ClearAttention(time.Unix(100, 0))
	require.False(t, cleared)
	require.Equal(t, "B", i.Attention().Reason)

	_, cleared = i.ClearAttention(time.Unix(200, 0))
	require.True(t, cleared)
	require.False(t, i.HasAttention())
}
