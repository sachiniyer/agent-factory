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

	require.Equal(t, OpRespawning, probe.opDuring,
		"the poll's skip is keyed on the instance-visible op, so a long respawn must advertise one")
	require.Equal(t, LiveLimitReached, probe.livenessDuring,
		"raising the fence must not disturb the liveness the resume path depends on")
	require.Equal(t, OpNone, i.GetInFlightOp(), "ConfirmLive clears the fence on the success path")
	require.Equal(t, LiveRunning, i.GetLiveness(), "and the respawned runtime settles live")
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

	require.Equal(t, OpRespawning, probe.opDuring, "the fence was up while the work ran")
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

// #3004 review finding 1 claimed the fence only makes LATER polls skip, leaving a
// window where a poll that already passed the skip applies its observation anyway.
// It does not: the fence transition itself advances the state epoch, and the poll's
// applies are epoch-scoped (daemon/manager_status.go:536, daemon/remoteloss.go:326
// — both .AtEpoch(epoch) taken BEFORE the probe), so a decision drawn before the
// fence is dropped at the chokepoint rather than applied. This is #2135 doing the
// job it was built for; the test exists so nobody has to re-derive that.
func TestRespawnFenceDropsAPollObservationDecidedBeforeItWasRaised(t *testing.T) {
	probe := newRespawnProbe()
	i := limitBlockedInstance(t, probe)

	// The poll captures the epoch, then probes — slowly, against a remote sandbox.
	pollEpoch := i.StateEpoch()

	// The resume raises the fence while that probe is still in flight.
	require.NoError(t, i.Transition(BeginRespawn()))
	require.NotEqual(t, pollEpoch, i.StateEpoch(), "raising the fence must advance the epoch")

	// The probe now comes back and settles the death it saw — which was OUR teardown.
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost).AtEpoch(pollEpoch)))

	require.Equal(t, LiveLimitReached, i.GetLiveness(),
		"a stale observation must not clobber the liveness the running resume depends on")
	require.Equal(t, OpRespawning, i.GetInFlightOp(), "and the fence still stands")
	i.clearRespawnFence()
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionResumeLimit),
		"once the fence is down the resume can be retried")
}

// The same poll observation is NOT dropped when it is genuinely current — the guard
// is an epoch check, not a blanket refusal, so real deaths still land.
func TestAnUnstaleObservationStillLands(t *testing.T) {
	i := limitBlockedInstance(t, newRespawnProbe())
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost).AtEpoch(i.StateEpoch())))
	require.Equal(t, LiveLost, i.GetLiveness())
}

// #3004 review finding 2, asserted where the harm actually happens rather than via
// composeStatus: SaveInstances drops ordinary Loading/Deleting rows, so a fence that
// composed to Loading would make a shutdown checkpoint erase an established
// session's only on-disk record — and the checkpoint is wholesale per repo, so any
// OTHER started session in the same repo triggers it.
func TestSaveInstances_KeepsARespawningRowAlongsideStartedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	sibling := &Instance{Title: "sibling", Path: repoPath, started: true, liveness: LiveRunning}
	respawning := &Instance{ID: "respawn-id", Title: "respawning", Path: repoPath, Program: "claude", started: true, liveness: LiveReady}
	require.NoError(t, respawning.Transition(ObserveLiveness(LiveLimitReached)))
	require.NoError(t, respawning.Transition(BeginRespawn()))

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{sibling, respawning}))

	for _, row := range readDisk(t, state, repoPath) {
		if row.Title == respawning.Title {
			require.NotEqual(t, Loading, row.Status,
				"a respawn fence must not present as Loading — that is the value the retention skip drops")
			return
		}
	}
	t.Fatal("the shutdown checkpoint dropped the respawning row; the next daemon start would orphan its workspace")
}

// #3004 review finding 4 (P2): the fence has to gate ACTIONS, not just the poll.
// The TUI re-derives RuntimeActionHandoff on its own instance (app/handle_handoff.go)
// while the daemon re-validates on its authoritative one (daemon/handoff.go), so a
// fence the TUI does not mirror means the picker opens and the daemon then refuses
// the selected handoff. This pins the session-side contract that agreement rests on:
// the fence refuses a competing runtime action, and lowering it restores the action.
// The mirroring itself is app/sync.go's reconcile, covered by CI.
func TestRespawnFenceRefusesACompetingRuntimeAction(t *testing.T) {
	i := limitBlockedInstance(t, newRespawnProbe())
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionHandoff),
		"a limit-parked session is handoff-able before the resume starts")

	require.NoError(t, i.Transition(BeginRespawn()))
	require.Error(t, i.ValidateRuntimeAction(RuntimeActionHandoff),
		"a session mid-respawn must refuse a handoff, on whichever side asks")

	i.clearRespawnFence()
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionHandoff),
		"and the action comes back once the fence is down")
}

// #3004 review finding 5 (P1): the one window the epoch guard genuinely cannot
// close. An observation landing BETWEEN the precondition check and the fence is
// CURRENT, not stale, so nothing drops it — and the raw fence edge does not re-check
// liveness (next test), so a validate that ran under an earlier, already-released
// lock cannot protect it. beginRespawnFence holds i.mu across both.
func TestBeginRespawnFenceRefusesAClobberedPrecondition(t *testing.T) {
	i := limitBlockedInstance(t, newRespawnProbe())
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)), "the poll's observation lands")

	require.Error(t, i.beginRespawnFence(), "a session the daemon just declared Lost is not resumable")
	require.Equal(t, OpNone, i.GetInFlightOp(), "and no fence was raised on the way out")
}

// Why that check has to live inside the fence's own critical section rather than
// ahead of it: tkBeginRespawn is keyed on the OP axis alone, so the edge itself will
// happily raise the fence over a clobbered liveness. This test is the reason
// beginRespawnFence exists; if the edge is ever given a liveness precondition
// instead, note that a legitimate loss of this race would then fire the
// illegal-transition hook (a panic under test), which is why the guard is a
// validating chokepoint and not an allowedFrom predicate.
func TestTheBareRespawnEdgeDoesNotCheckLiveness(t *testing.T) {
	i := limitBlockedInstance(t, newRespawnProbe())
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)))

	require.NoError(t, i.Transition(BeginRespawn()), "the bare edge checks only the op axis")
	require.Equal(t, LiveLost, i.GetLiveness(), "so it fences a session with no limit to resume from")
}

// And the composite: Respawn refuses without ever reaching the backend.
func TestRespawnDoesNotReachTheBackendOnAClobberedPrecondition(t *testing.T) {
	probe := newRespawnProbe()
	i := limitBlockedInstance(t, probe)
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)))

	require.Error(t, i.Respawn())
	require.Equal(t, LivenessUnset, probe.livenessDuring, "the backend must never have been called")
	require.Equal(t, OpNone, i.GetInFlightOp())
}

// #3004 review findings 6 and 9 (P2): the #2500 defect class — a fenced row still
// offering a verb that its own fence refuses on press. Both verbs apply. Kill looks
// like an exception because tkBeginKill is allowedFrom-always, but the daemon cannot
// REACH that transition during a respawn: it waits 30s for the per-session op lock
// the resume holds and then returns errKillBusy. See canKillFor.
func TestRespawningRowOffersNeitherArchiveNorKill(t *testing.T) {
	i := limitBlockedInstance(t, newRespawnProbe())
	i.ID = "respawn-gate-id"
	require.Equal(t, LifecycleActionArchive, i.LifecycleAction(),
		"a limit-parked row archives before the resume starts")

	require.NoError(t, i.Transition(BeginRespawn()))

	require.Equal(t, LifecycleActionNone, i.LifecycleAction(),
		"Archive would tear down a worktree the respawn is provisioning into")
	require.False(t, i.CanKill(),
		"Kill cannot supersede this fence — KillSession times out on the op lock the "+
			"resume holds, so advertising it promises a supersede it cannot perform")

	i.clearRespawnFence()
	require.Equal(t, LifecycleActionArchive, i.LifecycleAction(), "and the verb comes back")
	require.True(t, i.CanKill(), "as does Kill")
}
