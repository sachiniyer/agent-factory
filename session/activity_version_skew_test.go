package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// unknownOp is an InFlightOp value this build has no name for — what a NEWER
// remote daemon sends over a wire format that accepts any int. It is the whole
// subject of this file, so it is named once rather than repeated as a literal.
const unknownOp = InFlightOp(9999)

// knownOps is every operation constant this build defines. The skew tests below
// assert that an unknown value behaves exactly as these do, so the list has to be
// the real set: a new op added without appending it here would silently narrow
// what "every known op still classifies as pending" covers.
var knownOps = []InFlightOp{
	OpCreating, OpKilling, OpArchiving, OpRestoring, OpReplacing, OpRespawning,
}

// TestClassifyActivity_UnknownInFlightOpIsNotIdle is the reported defect (#3450).
// A newer daemon reports an operation this build cannot name; the liveness axis
// alongside it still reads LiveReady because liveness is stale precisely while an
// operation runs. Classifying by liveness returns idle, and idle is the one
// verdict that makes `af sessions watch` exit 0 — telling automation a
// mid-operation session is ready for review.
func TestClassifyActivity_UnknownInFlightOpIsNotIdle(t *testing.T) {
	activity, _ := ClassifyActivity(InstanceData{Liveness: LiveReady, InFlightOp: unknownOp})
	require.Equal(t, ActivityPending, activity,
		"an operation this build cannot name is still an operation — it must never classify as idle")
}

// TestClassifyActivity_UnknownInFlightOpWinsOverEveryLiveness: the op axis wins
// over liveness for known ops, and an unknown op is not a weaker op. Every
// liveness value is exercised, including the two that would otherwise produce a
// definite answer a caller acts on immediately — LiveReady (watch exits 0) and
// the terminal trio (watch exits non-zero and stops).
func TestClassifyActivity_UnknownInFlightOpWinsOverEveryLiveness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		liveness Liveness
		status   Status
	}{
		{"ready", LiveReady, Running},
		{"running", LiveRunning, Running},
		{"limit reached", LiveLimitReached, Running},
		{"lost", LiveLost, Running},
		{"dead", LiveDead, Running},
		{"archived", LiveArchived, Running},
		// LivenessUnset routes through the legacy Status fallback, whose Ready arm
		// is the same fabricated-idle by another road.
		{"legacy record with a ready status", LivenessUnset, Ready},
		{"legacy record with an archived status", LivenessUnset, Archived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := InstanceData{Liveness: tc.liveness, Status: tc.status, InFlightOp: unknownOp}

			activity, _ := ClassifyActivity(data)
			require.Equal(t, ActivityPending, activity,
				"an unknown op must hold the session pending regardless of the liveness beneath it")

			// The same fixture with a KNOWN op is the control: whatever the unknown
			// value does here, it must match what the ops this build understands do.
			for _, op := range knownOps {
				known := data
				known.InFlightOp = op
				got, _ := ClassifyActivity(known)
				require.Equal(t, activity, got,
					"unknown op %d and known op %s must reach the same verdict", int(unknownOp), opLabel(op))
			}
		})
	}
}

// TestClassifyActivity_KnownOpsStillPending is the unchanged half of the
// contract. Widening the op check to "anything non-zero" must not disturb the
// enumerated set it replaces.
func TestClassifyActivity_KnownOpsStillPending(t *testing.T) {
	for _, op := range knownOps {
		t.Run(opLabel(op), func(t *testing.T) {
			activity, _ := ClassifyActivity(InstanceData{Liveness: LiveReady, InFlightOp: op})
			require.Equal(t, ActivityPending, activity)
		})
	}
}

// TestClassifyActivity_OpNoneStillClassifiesByLiveness is the regression guard on
// the other side: a widened op check that mis-read the zero value would make
// every settled session permanently busy — `af sessions watch` would never
// return and the #1892 concurrency cap would never free a slot. OpNone must keep
// deferring to the liveness axis exactly as before.
func TestClassifyActivity_OpNoneStillClassifiesByLiveness(t *testing.T) {
	for _, tc := range []struct {
		name string
		data InstanceData
		want Activity
	}{
		{"ready is still idle", InstanceData{Liveness: LiveReady}, ActivityIdle},
		{"running is still pending", InstanceData{Liveness: LiveRunning}, ActivityPending},
		{"lost is still terminal", InstanceData{Liveness: LiveLost}, ActivityTerminal},
		{"archived is still terminal", InstanceData{Liveness: LiveArchived}, ActivityTerminal},
		{"legacy ready record is still idle", InstanceData{Status: Ready}, ActivityIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, OpNone, tc.data.InFlightOp, "fixture must exercise the no-op path")
			got, _ := ClassifyActivity(tc.data)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestClassifyActivity_UnknownOpDoesNotResurrectTerminalGuards: the two fences
// ahead of the op axis are settled outcomes that no operation marker may reopen.
// A committed kill can legitimately carry a stale op, and a failed create whose
// runtime identity was never confirmed is blocked rather than busy — so widening
// the op check must not turn either into "keep waiting", which would leave watch
// polling a session nothing will ever settle.
func TestClassifyActivity_UnknownOpDoesNotResurrectTerminalGuards(t *testing.T) {
	t.Run("user killed stays terminal", func(t *testing.T) {
		activity, reason := ClassifyActivity(InstanceData{
			Liveness: LiveRunning, InFlightOp: unknownOp, UserKilled: true,
		})
		require.Equal(t, ActivityTerminal, activity)
		require.NotEmpty(t, reason, "a terminal outcome must explain itself")
	})

	t.Run("startup state unknown stays terminal", func(t *testing.T) {
		activity, reason := ClassifyActivity(InstanceData{
			Liveness: LiveReady, InFlightOp: unknownOp, StartupStateUnknown: true,
		})
		require.Equal(t, ActivityTerminal, activity)
		require.NotEmpty(t, reason, "a terminal outcome must explain itself")
	})

	t.Run("a pending handoff mission is still pending", func(t *testing.T) {
		activity, _ := ClassifyActivity(InstanceData{
			Liveness: LiveReady, InFlightOp: unknownOp, PendingHandoffMission: "continue inherited work",
		})
		require.Equal(t, ActivityPending, activity)
	})
}

// TestLifecycleViewActivity_UnknownInFlightOp: the live-instance entry point runs
// the same state machine, and the #1892 cap reads THIS one. A view that freed a
// slot while the record held one is the drift the shared classifier exists to
// prevent, so the skew rule has to hold on both paths.
func TestLifecycleViewActivity_UnknownInFlightOp(t *testing.T) {
	t.Run("unknown op is pending", func(t *testing.T) {
		activity := (LifecycleView{Liveness: LiveReady, InFlightOp: unknownOp}).Activity()
		require.Equal(t, ActivityPending, activity)
	})

	t.Run("agrees with the record path", func(t *testing.T) {
		for _, lv := range []Liveness{LiveReady, LiveRunning, LiveLost, LiveArchived} {
			view := LifecycleView{Liveness: lv, InFlightOp: unknownOp}
			fromRecord, _ := ClassifyActivity(InstanceData{Liveness: lv, InFlightOp: unknownOp})
			require.Equal(t, fromRecord, view.Activity(),
				"the live and record paths must agree under version skew too")
		}
	})

	t.Run("a killed view stays terminal", func(t *testing.T) {
		activity := (LifecycleView{Liveness: LiveReady, InFlightOp: unknownOp, UserKilled: true}).Activity()
		require.Equal(t, ActivityTerminal, activity)
	})

	t.Run("an unconfirmed startup stays terminal", func(t *testing.T) {
		activity := (LifecycleView{
			Liveness: LiveReady, InFlightOp: unknownOp, StartupStateUnknown: true,
		}).Activity()
		require.Equal(t, ActivityTerminal, activity)
	})
}
