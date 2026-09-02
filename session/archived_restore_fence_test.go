package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// #3596. The daemon's LOCAL archived-restore route holds the restore fence across
// its worktree relocate, so the re-spawn has to be reachable with the fence
// already up. These pin the shape that makes that possible without weakening the
// guard that makes a double-restore impossible.
//
// The strict edge stays strict: tkBeginRestore's `op == OpNone` is I3, and
// widening it to admit OpRestoring would admit ANY restore in flight, not merely
// this operation's own fence. BeginRestoreUnderHeldFence is a separate entry with
// the identical target.

type archivedFenceBackend struct {
	*FakeBackend
	fail         error
	recoverCalls int
	livenessNow  Liveness
	opDuring     InFlightOp
}

func newArchivedFenceBackend() *archivedFenceBackend {
	return &archivedFenceBackend{FakeBackend: NewFakeBackend()}
}

func (b *archivedFenceBackend) Recover(i *Instance) error {
	b.recoverCalls++
	b.livenessNow = i.GetLiveness()
	b.opDuring = i.GetInFlightOp()
	if b.fail != nil {
		return b.fail
	}
	_ = i.Transition(ConfirmLive())
	return nil
}

// archivedInstance is a settled archived row: liveness Archived, started false —
// the state CommitArchive leaves behind. The ID matters because canKillFor refuses
// an id-less row outright, and the Kill affordance is the subject here.
func archivedInstance(t *testing.T, backend Backend) *Instance {
	t.Helper()
	i := &Instance{ID: "inst-3596", Title: "shelved", liveness: LiveArchived, started: false, backend: backend}
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionRestoreArchived))
	require.True(t, i.CanKill(), "setup: a settled archived row advertises Kill")
	require.True(t, i.ShownArchived(), "setup: it renders in the Archived section")
	return i
}

// The fence the daemon holds across the relocate: op axis only, and the two
// predicates move in opposite directions on purpose.
func TestBeginArchivedRestoreFenceKeepsLivenessAndReHomesTheRow(t *testing.T) {
	i := archivedInstance(t, newArchivedFenceBackend())

	require.NoError(t, i.BeginArchivedRestoreFence())
	require.Equal(t, OpRestoring, i.GetInFlightOp(), "the caller now owns the fence")
	require.False(t, i.CanKill(), "and Kill is hidden from that instant, not from the re-spawn")

	require.Equal(t, LiveArchived, i.GetLiveness(),
		"MarkRestoring must leave liveness alone: the snapshot reconcile keys its Archived->live rebuild "+
			"on that exact transition, so an eager flip makes it see live->live and SKIP the rebuild (#1203)")
	require.False(t, i.ShownArchived(),
		"the row must yield to the live Instances section the moment a restore starts (#1210)")
	require.True(t, i.IsArchived(),
		"IsArchived reads the liveness axis alone, so the inert-state gates it guards (tab spawn, web tab "+
			"serve) stay closed while the worktree is in motion")
	require.False(t, i.Started(), "and the fence does not start the session; BeginRestore still does that")
}

// The re-spawn under the held fence: same destination as the unfenced form.
func TestRestoreFromArchiveHeldFencedRunsUnderTheCallersFence(t *testing.T) {
	backend := newArchivedFenceBackend()
	i := archivedInstance(t, backend)

	require.NoError(t, i.BeginArchivedRestoreFence())
	require.NoError(t, i.RestoreFromArchiveHeldFenced())

	require.Equal(t, 1, backend.recoverCalls)
	require.Equal(t, LiveLost, backend.livenessNow,
		"the Archived->Lost edge still happens here, after the worktree is home")
	require.Equal(t, OpRestoring, backend.opDuring, "and the re-spawn still runs fenced")
	require.Equal(t, LiveRunning, i.GetLiveness())
	require.Equal(t, OpNone, i.GetInFlightOp(), "ConfirmLive clears the fence on success")
	require.False(t, i.EndArchivedRestoreFence(), "so the caller's deferred release is a no-op")
}

// Failure keeps the #1108 hand-off exactly as the unfenced form does.
func TestRestoreFromArchiveHeldFencedAbortsToLostOnFailure(t *testing.T) {
	backend := newArchivedFenceBackend()
	backend.fail = errors.New("spawn refused")
	i := archivedInstance(t, backend)

	require.NoError(t, i.BeginArchivedRestoreFence())
	require.Error(t, i.RestoreFromArchiveHeldFenced())

	require.Equal(t, LiveLost, i.GetLiveness(),
		"a re-spawn that failed after the worktree came home must hand the row to the #1108 loop, not "+
			"leave it shelved with its worktree in place")
	require.Equal(t, OpNone, i.GetInFlightOp(), "AbortRestoreToLost drops the fence")
	require.False(t, i.EndArchivedRestoreFence(), "so the caller's release finds nothing to lower")
	require.True(t, i.CanKill(), "and the row is removable")
}

// Without the fence the held form refuses and never reaches the backend.
func TestRestoreFromArchiveHeldFencedRefusesWithoutTheFence(t *testing.T) {
	backend := newArchivedFenceBackend()
	i := archivedInstance(t, backend)

	err := i.RestoreFromArchiveHeldFenced()
	require.Error(t, err)
	require.Contains(t, err.Error(), "restore fence")
	require.Zero(t, backend.recoverCalls)
	require.Equal(t, LiveArchived, i.GetLiveness(), "and nothing moved on the way out")
	require.Equal(t, OpNone, i.GetInFlightOp())
}

// The strict edge stays strict. This is the guard the triage said not to weaken:
// tkBeginRestore is I3 — no double-restore — and it must keep refusing a row whose
// restore is already in flight, whoever raised that fence.
func TestBeginRestoreStillRefusesAFencedRow(t *testing.T) {
	i := archivedInstance(t, newArchivedFenceBackend())
	require.NoError(t, i.BeginArchivedRestoreFence())

	// Illegal edges are a LOUD failure in test builds (the panicking hook), which is
	// the point: this must never quietly become legal.
	require.Panics(t, func() { _ = i.Transition(BeginRestore()) },
		"widening BeginRestore's OpNone guard would admit ANY restore in flight, not just this "+
			"operation's own fence")
	require.Equal(t, LiveArchived, i.GetLiveness(), "and the refused edge moved nothing")
	require.Equal(t, OpRestoring, i.GetInFlightOp())

	// The unfenced public entry point refuses earlier, at the shared ledger, so it
	// never reaches the edge at all.
	require.ErrorContains(t, i.RestoreFromArchive(), "busy")
}

// And the held entry is not an alias for the strict one: it refuses an unfenced
// row, so a caller cannot reach it without having raised the fence.
func TestBeginRestoreUnderHeldFenceRefusesAnUnfencedRow(t *testing.T) {
	i := archivedInstance(t, newArchivedFenceBackend())
	require.Panics(t, func() { _ = i.Transition(BeginRestoreUnderHeldFence()) })
	require.Equal(t, LiveArchived, i.GetLiveness())
	require.Equal(t, OpNone, i.GetInFlightOp())
}

// The public admission question is UNWEAKENED, and the continuation is separately
// named — the same split #3597 made for Recover.
func TestRestoreArchivedStillRefusesARowThatIsAlreadyRestoring(t *testing.T) {
	fenced := LifecycleView{Title: "fenced", Liveness: LiveArchived, InFlightOp: OpRestoring}
	require.Error(t, fenced.ValidateRuntimeAction(RuntimeActionRestoreArchived),
		"accepting OpRestoring here would answer yes for a row whose restore is already running")
	require.NoError(t, fenced.ValidateRuntimeAction(RuntimeActionRestoreArchivedFenced))

	idle := LifecycleView{Title: "idle", Liveness: LiveArchived}
	require.NoError(t, idle.ValidateRuntimeAction(RuntimeActionRestoreArchived))
	require.Error(t, idle.ValidateRuntimeAction(RuntimeActionRestoreArchivedFenced),
		"and the continuation is not an alias for the admission question")
}

// Validating through the shared ledger rather than a bare op comparison carries
// the universal vetoes onto this path too.
func TestRestoreArchivedFencedInheritsTheUniversalVetoes(t *testing.T) {
	fenced := LifecycleView{Title: "fenced", Liveness: LiveArchived, InFlightOp: OpRestoring}

	killed := fenced
	killed.UserKilled = true
	require.ErrorContains(t, killed.ValidateRuntimeAction(RuntimeActionRestoreArchivedFenced), "pending kill")

	unknown := fenced
	unknown.StartupStateUnknown = true
	require.ErrorContains(t, unknown.ValidateRuntimeAction(RuntimeActionRestoreArchivedFenced), "unknown startup state")

	notArchived := fenced
	notArchived.Liveness = LiveLost
	require.Error(t, notArchived.ValidateRuntimeAction(RuntimeActionRestoreArchivedFenced))
}

// #3555's property, on this raise: validation and the raise share one critical
// section, so an observation that wins the lock is REFUSED rather than fenced over.
func TestBeginArchivedRestoreFenceRefusesARowThatIsNoLongerArchived(t *testing.T) {
	i := archivedInstance(t, newArchivedFenceBackend())
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)), "the poll's observation lands first")

	require.Error(t, i.BeginArchivedRestoreFence(), "a session that is not archived has no archive to restore")
	require.Equal(t, OpNone, i.GetInFlightOp(), "and no fence was raised on the way out")
}

// EndArchivedRestoreFence must never clear an op it does not own.
func TestEndArchivedRestoreFenceLeavesASupersedingOverlayAlone(t *testing.T) {
	i := archivedInstance(t, newArchivedFenceBackend())
	require.NoError(t, i.BeginArchivedRestoreFence())
	require.NoError(t, i.Transition(BeginKill()), "a kill supersedes the restore")

	require.False(t, i.EndArchivedRestoreFence(), "no effective release to announce")
	require.Equal(t, OpKilling, i.GetInFlightOp(), "and the kill overlay survives the restore's release")
}

// Lowering the fence on a refusal returns the row to the Archived section — the
// other half of #1210's contract, and what every daemon refusal branch relies on.
func TestEndArchivedRestoreFenceReturnsTheRowToTheArchivedSection(t *testing.T) {
	i := archivedInstance(t, newArchivedFenceBackend())
	require.NoError(t, i.BeginArchivedRestoreFence())
	require.False(t, i.ShownArchived())

	require.True(t, i.EndArchivedRestoreFence(), "an effective release reports itself")
	require.True(t, i.ShownArchived(), "a restore that failed drops the row back into the Archived section")
	require.Equal(t, LiveArchived, i.GetLiveness(), "still shelved")
	require.True(t, i.CanKill(), "and removable again")
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionRestoreArchived), "and restorable again")
}
