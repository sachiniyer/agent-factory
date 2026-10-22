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

// fakeAttentionClock pins instanceNow and returns a function that moves it.
func fakeAttentionClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Unix(1_000, 0)
	previous := instanceNow
	instanceNow = func() time.Time { return now }
	t.Cleanup(func() { instanceNow = previous })
	return func(d time.Duration) { now = now.Add(d) }
}

func TestAttentionClearsOnlyWhenWorkStartsAfterIdle(t *testing.T) {
	advance := fakeAttentionClock(t)
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("  pick A or B  ", time.Unix(100, 0))
	require.NoError(t, err)
	require.Equal(t, "pick A or B", i.Attention().Reason)

	moveLiveness(i, LiveRunning)
	require.True(t, i.HasAttention(), "no liveness change keeps the request")

	moveLiveness(i, LiveReady)
	require.True(t, i.HasAttention(), "going idle is the agent waiting, not an answer")
	require.True(t, i.Attention().SeenIdle)

	advance(attentionAnswerIdle)
	moveLiveness(i, LiveRunning)
	require.False(t, i.HasAttention(), "work after idle is the user's answer")
}

// The agent can go still for one poll after `attention --self` and then print
// its closing message. That output is the same turn, not the user's answer.
func TestAttentionSurvivesAShortStillSpellInsideTheAgentsTurn(t *testing.T) {
	advance := fakeAttentionClock(t)
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("pick A or B", time.Unix(1_000, 0))
	require.NoError(t, err)

	moveLiveness(i, LiveReady)
	advance(time.Second)
	moveLiveness(i, LiveRunning)
	require.True(t, i.HasAttention(), "the closing message must not clear the request")
	require.False(t, i.Attention().SeenIdle, "the short spell does not count as idle")

	moveLiveness(i, LiveReady)
	advance(attentionAnswerIdle)
	moveLiveness(i, LiveRunning)
	require.False(t, i.HasAttention(), "work after the turn really ended clears it")
}

func TestAttentionWithoutIdleSinceKeepsTheOldClearRule(t *testing.T) {
	fakeAttentionClock(t)
	i := &Instance{Title: "w", liveness: LiveReady, attention: &Attention{SetAt: time.Unix(1, 0), SeenIdle: true}}
	moveLiveness(i, LiveRunning)
	require.False(t, i.HasAttention())
}

func TestAttentionDoesNotSurviveALoadIntoAStoppedState(t *testing.T) {
	inst, err := FromInstanceData(InstanceData{
		ID:          "id",
		Title:       "s",
		Path:        t.TempDir(),
		Branch:      "root/s",
		BackendType: "docker",
		Liveness:    LiveArchived,
		Attention:   &Attention{SetAt: time.Unix(100, 0)},
	})
	require.NoError(t, err)
	require.False(t, inst.HasAttention())
}

func TestAttentionDoesNotSurviveASandboxLoadedLost(t *testing.T) {
	inst, err := FromInstanceData(InstanceData{
		ID:          "id",
		Title:       "s",
		Path:        t.TempDir(),
		Branch:      "root/s",
		BackendType: "docker",
		Liveness:    LiveRunning,
		Attention:   &Attention{SetAt: time.Unix(100, 0)},
	})
	require.NoError(t, err)
	require.Equal(t, LiveLost, inst.GetLiveness())
	require.False(t, inst.HasAttention())
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
