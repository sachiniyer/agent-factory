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

// A sandbox row loads provisionally Lost and the daemon then reattaches it to the
// agent that is still waiting, so the request must survive the load; it goes only
// when the reattach attempt leaves the row not live.
func TestAttentionSurvivesASandboxLoadedLostUntilReattachSettles(t *testing.T) {
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
	require.True(t, inst.HasAttention(), "a reattachable row keeps its request through the provisional Lost")

	inst.SettleAttentionAfterReattach()
	require.False(t, inst.HasAttention(), "a reattach that left the row Lost has no agent to answer")

	live, err := FromInstanceData(InstanceData{
		ID: "id2", Title: "s2", Path: t.TempDir(), Branch: "root/s2", BackendType: "docker",
		Liveness: LiveRunning, Attention: &Attention{SetAt: time.Unix(100, 0)},
	})
	require.NoError(t, err)
	live.mu.Lock()
	live.liveness = LiveRunning // what a successful ReattachSandbox sets
	live.mu.Unlock()
	live.SettleAttentionAfterReattach()
	require.True(t, live.HasAttention(), "a reattached row stays live and keeps its request")
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

func TestRestoreAttentionKeepsAStoppedSessionClear(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("A", time.Unix(100, 0))
	require.NoError(t, err)
	previous, cleared := i.ClearAttention(time.Time{})
	require.True(t, cleared)
	moveLiveness(i, LiveLost)
	i.RestoreAttention(previous)
	require.False(t, i.HasAttention(), "a rollback must not put a request on a Lost session")
}

func TestRestoreAttentionDoesNotUndoALiveTransition(t *testing.T) {
	fakeAttentionClock(t)
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("A", time.Unix(100, 0))
	require.NoError(t, err)
	moveLiveness(i, LiveReady)
	undo, cleared := i.ClearAttention(time.Time{})
	require.True(t, cleared)
	moveLiveness(i, LiveRunning)
	i.RestoreAttention(undo)
	require.False(t, i.HasAttention(), "a rollback must not bring back a request a transition already moved past")
}

func TestSetAttentionRefusesAKillTombstone(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveRunning, userKilled: true}
	_, err := i.SetAttention("", time.Unix(100, 0))
	require.ErrorIs(t, err, ErrAttentionNotLive)
	require.False(t, i.HasAttention())
}

func TestMarkUserKilledClearsAttention(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("", time.Unix(100, 0))
	require.NoError(t, err)
	i.MarkUserKilled()
	require.False(t, i.HasAttention())
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

// A typed draft repaints the composer, so the daemon reads the pane as churning
// and moves the session to Running. For an agent that publishes its own in-turn
// indicator that churn is not work: it must not answer the request, however long
// the agent sat idle first.
func TestChurnOnlyRunningKeepsTheRequest(t *testing.T) {
	advance := fakeAttentionClock(t)
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("pick A or B", time.Unix(1_000, 0))
	require.NoError(t, err)
	moveLiveness(i, LiveReady)
	advance(attentionAnswerIdle)
	require.True(t, i.Attention().SeenIdle)

	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning).ChurnOnly()))
	require.Equal(t, LiveRunning, i.GetLiveness(), "the churn still moves the liveness")
	require.True(t, i.HasAttention(), "a repainted composer is not the user's answer")
	require.True(t, i.Attention().SeenIdle, "and it leaves the idle record alone")

	// Control: the same edge without ChurnOnly is the old rule and clears.
	moveLiveness(i, LiveReady)
	advance(attentionAnswerIdle)
	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)))
	require.False(t, i.HasAttention(), "an observation that is not churn-only still answers")
}

func TestAttentionAnsweredByWorkNeedsAnIdleSpellFirst(t *testing.T) {
	advance := fakeAttentionClock(t)
	i := &Instance{Title: "w", liveness: LiveRunning}
	_, err := i.SetAttention("pick A or B", time.Unix(1_000, 0))
	require.NoError(t, err)

	_, ok := i.AttentionAnsweredByWork(i.stateEpoch)
	require.False(t, ok, "work seen before the agent ever went idle is its own turn")

	moveLiveness(i, LiveReady)
	advance(time.Second)
	_, ok = i.AttentionAnsweredByWork(i.stateEpoch)
	require.False(t, ok, "a short still spell is the agent pausing inside its turn")
	require.False(t, i.Attention().SeenIdle, "and it resets the idle record")

	moveLiveness(i, LiveRunning)
	moveLiveness(i, LiveReady)
	advance(attentionAnswerIdle)
	setAt, ok := i.AttentionAnsweredByWork(i.stateEpoch)
	require.True(t, ok)
	require.True(t, setAt.Equal(time.Unix(1_000, 0)), "it names the request it answers, for a keyed clear")
	require.True(t, i.HasAttention(), "naming it does not clear it; the caller clears under the repo lock")
}

// A frame captured before the agent raised its request cannot answer it
// (Codex P2 on #5581): a request raised at Ready inherits a satisfied idle
// spell, so only the observation epoch tells the stale frame apart.
func TestAttentionAnsweredByWorkIgnoresAnObservationFromBeforeTheRequest(t *testing.T) {
	fakeAttentionClock(t)
	i := &Instance{Title: "w", liveness: LiveReady}
	observed := i.stateEpoch
	_, err := i.SetAttention("pick A or B", time.Unix(1_000, 0))
	require.NoError(t, err)
	require.True(t, i.Attention().SeenIdle, "precondition: the request inherits the idle spell")

	_, ok := i.AttentionAnsweredByWork(observed)
	require.False(t, ok, "work seen before the request existed is not its answer")
	_, ok = i.AttentionAnsweredByWork(i.stateEpoch)
	require.True(t, ok, "control: the same frame observed after it does answer")
}

// ReconcileAttention is the client-projection mirror (#5461): a snapshot poll
// must move the request both ways — appear and clear — without a rebuild and
// without touching the local epoch, which is unrelated to the daemon's.
func TestReconcileAttentionMirrorsBothTransitions(t *testing.T) {
	i := &Instance{Title: "w", liveness: LiveReady}
	epoch := i.stateEpoch

	request := &Attention{Reason: "pick A or B", SetAt: time.Unix(1_000, 0)}
	require.True(t, i.ReconcileAttention(request))
	require.Equal(t, "pick A or B", i.Attention().Reason)
	require.Equal(t, epoch, i.stateEpoch, "a client's epoch is unrelated to the daemon's")

	require.False(t, i.ReconcileAttention(request),
		"the same request re-polled is a no-op, not a change")

	// The bookkeeping fields mirror too, so a SeenIdle advance lands silently.
	idle := &Attention{Reason: "pick A or B", SetAt: time.Unix(1_000, 0), SeenIdle: true}
	require.True(t, i.ReconcileAttention(idle))
	require.True(t, i.Attention().SeenIdle)

	require.True(t, i.ReconcileAttention(nil), "the daemon's clear mirrors")
	require.False(t, i.HasAttention())
	require.False(t, i.ReconcileAttention(nil), "an absent request stays absent")
}
