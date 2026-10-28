package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// #3586. The manual Lost/Dead restore holds the restore fence for its WHOLE
// operation — including the two network phases that run before the backend call
// — so recovery has to be reachable with the fence already up. These pin the
// shape that makes that possible without reopening #3555.
//
// The property #3555 established is that the recover precondition is validated
// and the fence raised in ONE critical section. Moving the raise earlier must not
// split them, and must not weaken the PUBLIC admission question either: whether a
// row may have a restore started on it is still "Lost, started, and no op in
// flight". The continuation is a separately named action instead.

// heldFenceProbe reports the lifecycle state recovery ran under, and can be told
// to fail so the failure-side ownership of the fence is observable.
type heldFenceProbe struct {
	*FakeBackend
	fail         error
	recoverCalls int
	opDuring     InFlightOp
	livenessNow  Liveness
}

func newHeldFenceProbe() *heldFenceProbe {
	return &heldFenceProbe{FakeBackend: NewFakeBackend()}
}

// killableLostInstance is lostInstance with a stable ID, because canKillFor
// refuses an id-less legacy row outright — it cannot address the destructive API
// — and the Kill affordance is the whole subject here. Without the ID every
// CanKill assertion below would pass for the wrong reason.
func killableLostInstance(t *testing.T, backend Backend) *Instance {
	t.Helper()
	i := lostInstance(t, backend)
	i.ID = "inst-3586"
	require.True(t, i.CanKill(), "setup: an unfenced Lost row advertises Kill")
	return i
}

func (b *heldFenceProbe) Recover(i *Instance) error {
	b.recoverCalls++
	b.opDuring = i.GetInFlightOp()
	b.livenessNow = i.GetLiveness()
	if b.fail != nil {
		return b.fail
	}
	_ = i.Transition(ConfirmLive())
	return nil
}

// The whole point of the split: the caller raises once, up front, and the backend
// still runs under exactly the fence the archived-restore path projects.
func TestRecoverHeldFencedRunsUnderTheCallersFence(t *testing.T) {
	probe := newHeldFenceProbe()
	i := killableLostInstance(t, probe)

	require.NoError(t, i.BeginRecoverFence())
	require.Equal(t, OpRestoring, i.GetInFlightOp(), "the caller now owns the fence")
	require.False(t, i.CanKill(), "and Kill is hidden from that instant, not from the backend call")

	require.NoError(t, i.RecoverHeldFencedWithLiveBoundary(nil))
	require.Equal(t, 1, probe.recoverCalls)
	require.Equal(t, OpRestoring, probe.opDuring, "the backend still runs fenced")
	require.Equal(t, LiveLost, probe.livenessNow)
	require.Equal(t, OpNone, i.GetInFlightOp(), "ConfirmLive clears the fence on success")
	require.False(t, i.EndRecoverFence(), "so the caller's deferred release is a no-op, not a duplicate")
}

// Without the fence the held form refuses and never reaches the backend. A
// re-entrant flag would have made this silently do the self-fencing thing
// instead; naming the two orderings keeps the mistake loud.
func TestRecoverHeldFencedRefusesWithoutTheFence(t *testing.T) {
	probe := newHeldFenceProbe()
	i := lostInstance(t, probe)

	err := i.RecoverHeldFencedWithLiveBoundary(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "restore fence")
	require.Zero(t, probe.recoverCalls)
	require.Equal(t, OpNone, i.GetInFlightOp(), "and nothing was raised on the way out")
}

// The caller that raised the fence owns lowering it. Unlike
// RecoverFencedWithLiveBoundary, which raises and therefore releases, this form
// leaves a failed recovery fenced so the caller's own bookkeeping runs before the
// row is published as free.
func TestRecoverHeldFencedLeavesTheFenceToItsOwnerOnFailure(t *testing.T) {
	probe := newHeldFenceProbe()
	probe.fail = errors.New("spawn refused")
	i := killableLostInstance(t, probe)

	require.NoError(t, i.BeginRecoverFence())
	require.Error(t, i.RecoverHeldFencedWithLiveBoundary(nil))
	require.Equal(t, OpRestoring, i.GetInFlightOp(), "the fence is still the caller's to lower")

	require.True(t, i.EndRecoverFence(), "and lowering it reports an effective release")
	require.Equal(t, OpNone, i.GetInFlightOp())
	require.True(t, i.CanKill(), "a failed restore must leave the row removable, not permanently busy")
}

// The self-fencing form keeps its own release, because it raised.
func TestRecoverFencedStillReleasesItsOwnFenceOnFailure(t *testing.T) {
	probe := newHeldFenceProbe()
	probe.fail = errors.New("spawn refused")
	i := killableLostInstance(t, probe)

	require.Error(t, i.RecoverFencedWithLiveBoundary(nil))
	require.Equal(t, OpNone, i.GetInFlightOp(), "it raised the fence, so it lowers it")
	require.True(t, i.CanKill())
}

// The public admission question is UNWEAKENED. A row whose restore is already in
// flight is not a row a second restore may start, which is what
// RuntimeActionRecoverLost answers for the automatic loop
// (lostSessionWantsRestore) and every other caller deciding whether to begin one.
func TestRecoverLostStillRefusesARowThatIsAlreadyRestoring(t *testing.T) {
	fenced := LifecycleView{Title: "fenced", Liveness: LiveLost, Started: true, InFlightOp: OpRestoring}

	require.Error(t, fenced.ValidateRuntimeAction(RuntimeActionRecoverLost),
		"accepting OpRestoring here would tell every caller that a restore in flight is one they may start")
	require.NoError(t, fenced.ValidateRuntimeAction(RuntimeActionRecoverFenced),
		"the continuation is the separately named action")

	idle := LifecycleView{Title: "idle", Liveness: LiveLost, Started: true}
	require.NoError(t, idle.ValidateRuntimeAction(RuntimeActionRecoverLost))
	require.Error(t, idle.ValidateRuntimeAction(RuntimeActionRecoverFenced),
		"and the continuation is not an alias for the admission question")
}

// Validating through the shared ledger rather than a bare op comparison is what
// carries the universal vetoes onto this path too.
func TestRecoverHeldFencedInheritsTheUniversalVetoes(t *testing.T) {
	fenced := LifecycleView{Title: "fenced", Liveness: LiveLost, Started: true, InFlightOp: OpRestoring}

	killed := fenced
	killed.UserKilled = true
	require.ErrorContains(t, killed.ValidateRuntimeAction(RuntimeActionRecoverFenced), "pending kill")

	unknown := fenced
	unknown.StartupStateUnknown = true
	require.ErrorContains(t, unknown.ValidateRuntimeAction(RuntimeActionRecoverFenced), "unknown startup state")

	notLost := fenced
	notLost.Liveness = LiveRunning
	require.Error(t, notLost.ValidateRuntimeAction(RuntimeActionRecoverFenced))

	notStarted := fenced
	notStarted.Started = false
	require.Error(t, notStarted.ValidateRuntimeAction(RuntimeActionRecoverFenced))
}

// EndRecoverFence must never clear an op it does not own. A kill supersedes any
// in-flight op (tkBeginKill is allowed-from-always), and the restore's deferred
// release runs afterwards — clearing it there would drop a teardown overlay on
// the floor.
func TestEndRecoverFenceLeavesASupersedingOverlayAlone(t *testing.T) {
	i := lostInstance(t, newHeldFenceProbe())
	require.NoError(t, i.BeginRecoverFence())
	require.NoError(t, i.Transition(BeginKill()), "a kill supersedes the restore")

	require.False(t, i.EndRecoverFence(), "no effective release to announce")
	require.Equal(t, OpKilling, i.GetInFlightOp(), "and the kill overlay survives the restore's release")
}

func TestEndRecoverFenceIsSafeWithNoFenceRaised(t *testing.T) {
	i := lostInstance(t, newHeldFenceProbe())
	require.False(t, i.EndRecoverFence())
	require.Equal(t, OpNone, i.GetInFlightOp())
}

// #3555 is not reopened by exporting the raise: an observation that wins the lock
// outright is still REFUSED rather than fenced over, because BeginRecoverFence
// validates and raises under the one lock exactly as beginRecoverFence did.
func TestBeginRecoverFenceRefusesAnObservationThatWinsTheLock(t *testing.T) {
	probe := newHeldFenceProbe()
	i := lostInstance(t, probe)

	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)), "the poll's observation lands first")

	require.Error(t, i.BeginRecoverFence(), "a session the poll just saw running is not lost")
	require.Equal(t, OpNone, i.GetInFlightOp(), "and no fence was raised on the way out")
	require.Zero(t, probe.recoverCalls)
}

// And the same interleaving the #3555 test drives, aimed at the exported raise:
// an observation delivered from inside the validation's own critical section is
// blocked until after the raise, where the epoch guard drops it as superseded.
func TestBeginRecoverFenceRefusesAnObservationThatLandsDuringValidation(t *testing.T) {
	probe := newRecoverFenceProbe()
	i := lostInstance(t, probe)

	op, epoch := i.InFlightOpAndEpoch()
	require.Equal(t, OpNone, op)

	hook, join := arriveDuringValidation(i, ObserveLiveness(LiveRunning).AtEpoch(epoch))
	probe.duringValidate = hook

	err := i.BeginRecoverFence()
	join()

	require.NoError(t, err)
	require.Equal(t, LiveLost, i.GetLiveness(),
		"the raise must not fence a liveness an observation clobbered inside its own critical section")
	require.Equal(t, OpRestoring, i.GetInFlightOp())
}
