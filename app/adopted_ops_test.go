package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

func liveInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
	return inst
}

// A row carrying a DAEMON-adopted op must not veto the reconcile (#3005).
//
// The two guards exist for LOCAL optimistic ops, whose completion handler owns
// the identity transition by pointer identity (#808/#844). An op adopted from a
// snapshot has no such handler: its only release is a later OpNone snapshot for
// the SAME identity, which never arrives once the session is killed — or killed
// and recreated under the same title. Vetoing on it leaves a corpse on the rail
// that no snapshot can remove and only a TUI restart clears.
func TestAdoptedOps_DaemonOpDoesNotVetoReconcile(t *testing.T) {
	inst := liveInstance(t, "worker")
	m := &home{adoptedSnapshotOps: adoptedOps{}}

	// The daemon projects a handoff; the TUI mirrors it.
	require.True(t, m.reconcileSnapshotOp(inst, session.OpReplacing, session.LiveRunning))
	require.Equal(t, session.OpReplacing, inst.GetInFlightOp())

	require.False(t, m.adoptedSnapshotOps.vetoesReconcile(inst),
		"an adopted daemon op has no local completion handler waiting on it, so it must not "+
			"stop the reconcile from swapping or removing the row — the release it is waiting for "+
			"only ever arrives as an OpNone snapshot for this identity, and a killed or recreated "+
			"session never produces one")
}

// The other half, and the reason this is provenance rather than a blanket
// removal of the guard: a LOCAL op must still veto. Its completion handler owns
// the transition, and letting the reconcile swap or remove the row underneath it
// orphans the handshake and leaves two same-title rows (#808).
func TestAdoptedOps_LocalOpStillVetoesReconcile(t *testing.T) {
	inst := liveInstance(t, "worker")
	m := &home{adoptedSnapshotOps: adoptedOps{}}

	// An optimistic local kill — no snapshot involved.
	require.NoError(t, inst.Transition(session.BeginKill()))

	require.True(t, m.adoptedSnapshotOps.vetoesReconcile(inst),
		"a locally owned op must still veto: instanceKilledMsg owns this row's transition")
}

// Provenance is a (pointer, op) PAIR, and this is what that buys: a local
// transition that moves the row to a DIFFERENT op leaves the recorded value no
// longer matching, so the row reads as locally owned again — without every local
// call site having to remember to clear anything.
func TestAdoptedOps_LocalTransitionSupersedesAdoptedProvenance(t *testing.T) {
	inst := liveInstance(t, "worker")
	m := &home{adoptedSnapshotOps: adoptedOps{}}

	require.True(t, m.reconcileSnapshotOp(inst, session.OpReplacing, session.LiveRunning))
	require.False(t, m.adoptedSnapshotOps.vetoesReconcile(inst))

	// The user now kills it locally. The row's op changes out from under the
	// recorded provenance.
	require.NoError(t, inst.Transition(session.ClearOp()))
	require.NoError(t, inst.Transition(session.BeginKill()))

	require.True(t, m.adoptedSnapshotOps.vetoesReconcile(inst),
		"a stale adoption record must not speak for an op a local transition has since replaced")
}

// A same-title successor is a different row and must not inherit its
// predecessor's provenance — the same pointer-identity rule the completion
// handlers use.
func TestAdoptedOps_SuccessorDoesNotInheritProvenance(t *testing.T) {
	old := liveInstance(t, "worker")
	m := &home{adoptedSnapshotOps: adoptedOps{}}
	require.True(t, m.reconcileSnapshotOp(old, session.OpReplacing, session.LiveRunning))

	successor := liveInstance(t, "worker")
	require.NoError(t, successor.Transition(session.BeginKill()))

	require.True(t, m.adoptedSnapshotOps.vetoesReconcile(successor),
		"the successor's own local op must veto; provenance is keyed by pointer, not by title")
}

// An op the daemon reports for a row that already carries it must still be
// recorded. Otherwise a row adopted before this tracking existed — or on an
// earlier reconcile pass — stays permanently unrecorded and keeps vetoing, which
// is the original bug with extra steps.
func TestAdoptedOps_AlreadyCarriedOpIsStillRecorded(t *testing.T) {
	inst := liveInstance(t, "worker")
	m := &home{adoptedSnapshotOps: adoptedOps{}}

	// The row carries OpReplacing with nothing recorded, as it would after a
	// reconcile that predates the provenance map.
	require.NoError(t, inst.Transition(session.BeginHandoff()))
	require.True(t, m.adoptedSnapshotOps.vetoesReconcile(inst))

	// The daemon reports the same op again.
	m.reconcileSnapshotOp(inst, session.OpReplacing, session.LiveRunning)

	require.False(t, m.adoptedSnapshotOps.vetoesReconcile(inst),
		"a snapshot reporting the op this row already carries is still evidence the daemon owns it")
}

// Provenance must not outlive the rows it describes. The key is a pointer, so a
// leaked entry does not merely waste a map slot — it keeps the instance
// reachable, making this map the thing that prevents collection of the corpses it
// was added to help remove.
func TestAdoptedOps_PruneDropsRowsThatLeftTheStore(t *testing.T) {
	kept := liveInstance(t, "kept")
	gone := liveInstance(t, "gone")
	a := adoptedOps{}
	a.note(kept, session.OpReplacing)
	a.note(gone, session.OpArchiving)

	a.pruneTo([]*session.Instance{kept})

	require.Len(t, a, 1, "an entry whose row left the store must be dropped whatever route it left by")
	_, stillThere := a[gone]
	require.False(t, stillThere)
	_, survives := a[kept]
	require.True(t, survives, "a live row keeps its provenance")
}

// A row built directly FROM a snapshot can already carry a daemon-owned op — the
// cold-start path. It is just as adopted as one mirrored onto an existing row,
// and without recording it there the very first reconcile after launch treats it
// as local and vetoes forever.
func TestAdoptedOps_NoteRecordsAnOpCarriedFromConstruction(t *testing.T) {
	inst := liveInstance(t, "cold-start")
	require.NoError(t, inst.Transition(session.BeginHandoff()))

	a := adoptedOps{}
	a.note(inst, inst.GetInFlightOp())

	require.False(t, a.vetoesReconcile(inst),
		"an op a snapshot-built row arrived carrying is adopted, not local")
}
