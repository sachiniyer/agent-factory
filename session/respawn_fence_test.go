package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// respawnProbeBackend records what the instance looked like DURING Respawn, which
// is the only moment the fence matters — the poll runs concurrently with the
// backend call, not before or after it.
type respawnProbeBackend struct {
	*FakeBackend
	opDuring       InFlightOp
	livenessDuring Liveness
	err            error
	confirmLive    bool
}

func newRespawnProbe() *respawnProbeBackend {
	return &respawnProbeBackend{FakeBackend: NewFakeBackend()}
}

func (b *respawnProbeBackend) Respawn(i *Instance) error {
	b.opDuring = i.GetInFlightOp()
	b.livenessDuring = i.GetLiveness()
	if b.err != nil {
		return b.err
	}
	if b.confirmLive {
		// What a real backend does on success, and what clears the fence for free.
		_ = i.Transition(ConfirmLive())
	}
	return nil
}

func limitBlockedInstance(t *testing.T, backend Backend) *Instance {
	t.Helper()
	i := &Instance{liveness: LiveReady, Title: "fenced", started: true, backend: backend}
	require.NoError(t, i.Transition(ObserveLiveness(LiveLimitReached)))
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionResumeLimit))
	return i
}

// The regression #2997 finding 1 describes: a remote respawn pushes and
// re-provisions, which outlasts the poll interval. The poll skips a session with an
// op in flight, so the fence has to be UP for the whole backend call — otherwise the
// poll reads the teardown this call is performing as an independent death and
// overwrites LiveLimitReached, destroying the precondition of the resume that is
// still running.
func TestRespawn_FencesTheOperationAgainstTheStatusPoll(t *testing.T) {
	probe := newRespawnProbe()
	probe.confirmLive = true
	i := limitBlockedInstance(t, probe)

	require.NoError(t, i.Respawn())

	require.Equal(t, OpCreating, probe.opDuring,
		"the poll's skip is keyed on the instance-visible op, so a long respawn must advertise one")
	require.Equal(t, LiveLimitReached, probe.livenessDuring,
		"raising the fence must not disturb the liveness the resume path depends on")
	require.Equal(t, OpNone, i.GetInFlightOp(), "ConfirmLive clears the fence on the success path")
}

// The other half, and the more dangerous one to get wrong: a failed respawn must not
// leave the fence standing. A permanently busy session is skipped by the poll forever
// and refused by every runtime action — a worse outcome than the clobber being fixed.
func TestRespawn_LowersTheFenceWhenTheBackendFails(t *testing.T) {
	boom := errors.New("provision failed")
	probe := newRespawnProbe()
	probe.err = boom
	i := limitBlockedInstance(t, probe)

	require.ErrorIs(t, i.Respawn(), boom)

	require.Equal(t, OpCreating, probe.opDuring, "the fence was up while the work ran")
	require.Equal(t, OpNone, i.GetInFlightOp(), "and is down again once it failed")
	require.Equal(t, LiveLimitReached, i.GetLiveness(),
		"a failed respawn leaves the session where it was — still resumable")
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionResumeLimit),
		"the user must be able to retry the resume after a failure")
}

// A backend that returns success without reaching ConfirmLive must not strand the
// fence either — the cleanup is not conditional on which path the backend took.
func TestRespawn_LowersTheFenceWhenTheBackendSkipsConfirmLive(t *testing.T) {
	probe := newRespawnProbe()
	i := limitBlockedInstance(t, probe)

	require.NoError(t, i.Respawn())
	require.Equal(t, OpNone, i.GetInFlightOp())
}

// The precondition is checked BEFORE the fence goes up, because the validation
// itself requires OpNone — a fence raised first would reject the call it protects.
func TestRespawn_RefusesWhenAnotherOpAlreadyOwnsTheSession(t *testing.T) {
	probe := newRespawnProbe()
	i := limitBlockedInstance(t, probe)
	require.NoError(t, i.Transition(BeginKill()))

	require.Error(t, i.Respawn(), "a session another op owns is not respawnable")
	require.Equal(t, OpKilling, i.GetInFlightOp(), "and the owner's op is untouched")
	require.Equal(t, InFlightOp(0), probe.opDuring, "the backend was never reached")
}

// Why the fence is needed at all, stated as an executable fact rather than a
// comment: the poll's liveness edge is allowedFrom-always and keeps the op it
// finds, so if it lands mid-respawn it overwrites LiveLimitReached with whatever
// the half-torn-down session looks like — and that is the exact precondition the
// in-flight resume validated against. The resume then cannot be retried.
func TestObserveLivenessClobbersALimitBlockedSessionThatIsNotFenced(t *testing.T) {
	i := limitBlockedInstance(t, newRespawnProbe())

	// What refreshInstanceStatus applies when it does NOT skip.
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)))

	err := i.ValidateRuntimeAction(RuntimeActionResumeLimit)
	require.Error(t, err, "the resume's own precondition is gone")
	require.Contains(t, err.Error(), "not blocked on a usage limit")
}
