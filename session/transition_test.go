package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The session test binary panics on illegal transitions (#1195 Phase 2c): a
// mis-ordered lifecycle transition must be a loud red failure in tests.
// Production leaves the hook nil (soft error only). Tests that assert the
// soft-error prod path swap it out via swapIllegalHook.
func init() {
	onIllegalTransition = func(msg string) { panic(msg) }
}

// swapIllegalHook temporarily replaces the illegal-transition hook for one test
// and restores it on cleanup.
func swapIllegalHook(t *testing.T, h func(string)) {
	t.Helper()
	prev := onIllegalTransition
	onIllegalTransition = h
	t.Cleanup(func() { onIllegalTransition = prev })
}

// TestTransitionTable_EveryKindHasSpec is the table-exhaustiveness guard: every
// transitionKind must have a fully-populated edge spec. A new event added
// without a table row fails here instead of silently no-op'ing at runtime.
func TestTransitionTable_EveryKindHasSpec(t *testing.T) {
	for k := transitionKind(0); k < numTransitionKinds; k++ {
		spec, ok := transitionTable[k]
		require.Truef(t, ok, "transition kind %s has no table entry", k)
		require.NotNilf(t, spec.allowedFrom, "transition kind %s: nil allowedFrom", k)
		require.NotNilf(t, spec.target, "transition kind %s: nil target", k)
	}
}

// TestTransition_LegalEdgesApply drives one canonical legal transition per event
// (so every event constructor + table row is exercised) and asserts the
// resulting (liveness, op, started).
func TestTransition_LegalEdgesApply(t *testing.T) {
	cases := []struct {
		name        string
		from        stateAxes
		started     bool
		userKilled  bool
		ev          TransitionEvent
		wantL       Liveness
		wantOp      InFlightOp
		wantStarted bool
	}{
		{"BeginCreate", stateAxes{LiveReady, OpNone}, true, false, BeginCreate(), LiveReady, OpCreating, true},
		{"ConfirmLive from creating", stateAxes{LiveReady, OpCreating}, true, false, ConfirmLive(), LiveRunning, OpNone, true},
		{"ConfirmLive from restoring", stateAxes{LiveLost, OpRestoring}, true, false, ConfirmLive(), LiveRunning, OpNone, true},
		// ConfirmLive YIELDS to a teardown op: a completing spawn must not
		// resurrect a session a kill/archive owns — no-op, no error.
		{"ConfirmLive yields to killing", stateAxes{LiveRunning, OpKilling}, true, false, ConfirmLive(), LiveRunning, OpKilling, true},
		{"ConfirmLive yields to archiving", stateAxes{LiveRunning, OpArchiving}, true, false, ConfirmLive(), LiveRunning, OpArchiving, true},
		// ObserveLiveness is unconditional truth: sets liveness, PRESERVES the op
		// fence (a mid-archive row still receives the terminal liveness — #1187).
		{"ObserveLiveness sets liveness, keeps op", stateAxes{LiveRunning, OpArchiving}, true, false, ObserveLiveness(LiveLost), LiveLost, OpArchiving, true},
		{"ObserveLiveness on idle row", stateAxes{LiveRunning, OpNone}, true, false, ObserveLiveness(LiveReady), LiveReady, OpNone, true},
		{"BeginKill from idle", stateAxes{LiveRunning, OpNone}, true, false, BeginKill(), LiveRunning, OpKilling, true},
		// BeginKill is always legal — a kill supersedes any in-flight op.
		{"BeginKill supersedes archiving", stateAxes{LiveRunning, OpArchiving}, true, false, BeginKill(), LiveRunning, OpKilling, true},
		{"RevertKill", stateAxes{LiveRunning, OpKilling}, true, false, RevertKill(), LiveRunning, OpNone, true},
		{"BeginArchive from Ready", stateAxes{LiveReady, OpNone}, true, false, BeginArchive(), LiveReady, OpArchiving, true},
		{"BeginArchive from Running", stateAxes{LiveRunning, OpNone}, true, false, BeginArchive(), LiveRunning, OpArchiving, true},
		// A Lost / LimitReached session is archivable too (shelving it), matching
		// the daemon guards — BeginArchive gates on the op/Archived, not liveness.
		{"BeginArchive from Lost", stateAxes{LiveLost, OpNone}, true, false, BeginArchive(), LiveLost, OpArchiving, true},
		{"BeginArchive from LimitReached", stateAxes{LiveLimitReached, OpNone}, true, false, BeginArchive(), LiveLimitReached, OpArchiving, true},
		{"CommitArchive clears started", stateAxes{LiveRunning, OpArchiving}, true, false, CommitArchive(), LiveArchived, OpNone, false},
		{"AbortArchiveToLost", stateAxes{LiveRunning, OpArchiving}, true, false, AbortArchiveToLost(), LiveLost, OpNone, true},
		// BeginRestore SETS started=true on the archived (started=false) row,
		// mirroring RestoreFromArchive — else Recover's !Started() gate would
		// short-circuit and restore would never start (Greptile #1314).
		{"BeginRestore from Archived sets started", stateAxes{LiveArchived, OpNone}, false, false, BeginRestore(), LiveLost, OpRestoring, true},
		{"AbortRestoreToLost", stateAxes{LiveLost, OpRestoring}, true, false, AbortRestoreToLost(), LiveLost, OpNone, true},
		// MarkRestoring: optimistic restore overlay — OpRestoring, liveness KEPT
		// Archived (unlike BeginRestore which flips to Lost), started untouched.
		{"MarkRestoring keeps liveness", stateAxes{LiveArchived, OpNone}, false, false, MarkRestoring(), LiveArchived, OpRestoring, false},
		// ClearOp: drop any optimistic overlay to None, liveness untouched.
		{"ClearOp from archiving", stateAxes{LiveReady, OpArchiving}, true, false, ClearOp(), LiveReady, OpNone, true},
		{"ClearOp from restoring keeps liveness", stateAxes{LiveArchived, OpRestoring}, false, false, ClearOp(), LiveArchived, OpNone, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := &Instance{liveness: tc.from.liveness, inFlightOp: tc.from.op, started: tc.started, userKilled: tc.userKilled}
			require.NoError(t, i.Transition(tc.ev))
			assert.Equal(t, tc.wantL, i.liveness, "liveness")
			assert.Equal(t, tc.wantOp, i.inFlightOp, "op")
			assert.Equal(t, tc.wantStarted, i.started, "started")
		})
	}
}

// I1 (tombstone-before-teardown) is intentionally NOT a chokepoint edge —
// BeginKill is an unconstrained optimistic overlay (see its doc and
// TestTransition_LegalEdgesApply's "BeginKill supersedes archiving"). I1 is
// enforced by the daemon KillSession ordering instead.

// TestTransition_I2_CommitArchiveWithoutArchivingPanics: CommitArchive is
// reachable only from the OpArchiving fence — flipping Archived without it would
// mark a session inert whose worktree never moved.
func TestTransition_I2_CommitArchiveWithoutArchivingPanics(t *testing.T) {
	i := &Instance{liveness: LiveRunning, inFlightOp: OpNone}
	assert.Panics(t, func() { _ = i.Transition(CommitArchive()) })
	assert.Equal(t, LiveRunning, i.liveness, "a rejected transition must not mutate state")
	assert.Equal(t, OpNone, i.inFlightOp)
}

// TestTransition_I3_BeginRestoreFromRunningPanics: a restore may begin only from
// Archived — restoring a live session is rejected.
func TestTransition_I3_BeginRestoreFromRunningPanics(t *testing.T) {
	i := &Instance{liveness: LiveRunning, inFlightOp: OpNone}
	assert.Panics(t, func() { _ = i.Transition(BeginRestore()) })
	assert.Equal(t, OpNone, i.inFlightOp, "a rejected transition must not mutate state")
}

// TestTransition_DoubleRestorePanics: BeginRestore while a restore is already in
// flight (OpRestoring) is rejected — no double-restore.
func TestTransition_DoubleRestorePanics(t *testing.T) {
	i := &Instance{liveness: LiveLost, inFlightOp: OpRestoring}
	assert.Panics(t, func() { _ = i.Transition(BeginRestore()) })
	assert.Equal(t, OpRestoring, i.inFlightOp, "a rejected transition must not mutate state")
}

// TestTransition_I4_BeginArchiveAlreadyArchivedPanics: an archive may not begin
// on an already-archived session (I4 — the fence is raised once). Also covers
// the busy case (op in flight) implicitly via allowedFrom.
func TestTransition_I4_BeginArchiveAlreadyArchivedPanics(t *testing.T) {
	i := &Instance{liveness: LiveArchived, inFlightOp: OpNone}
	assert.Panics(t, func() { _ = i.Transition(BeginArchive()) })
	assert.Equal(t, LiveArchived, i.liveness, "a rejected transition must not mutate state")
	assert.Equal(t, OpNone, i.inFlightOp)
}

// TestTransition_IllegalReturnsSoftErrorInProd pins the production semantics:
// with no hook installed (prod), an illegal edge returns an error and leaves the
// state untouched rather than panicking — a user's daemon must not crash on a
// racing illegal transition.
func TestTransition_IllegalReturnsSoftErrorInProd(t *testing.T) {
	swapIllegalHook(t, nil)
	i := &Instance{liveness: LiveRunning, inFlightOp: OpNone}
	err := i.Transition(BeginRestore())
	require.Error(t, err, "an illegal edge must return a soft error in production")
	assert.Equal(t, LiveRunning, i.liveness, "state must be untouched on rejection")
	assert.Equal(t, OpNone, i.inFlightOp)
}
