package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInstanceDataCarriesTaskProvenance pins the association the watch-task
// concurrency limit counts by (#1892). A task's cap is enforced by counting the
// sessions it spawned, so the task id has to survive the trip onto disk and back:
// a restarted daemon rebuilds its instances from exactly these rows, and a
// dropped task_id would make every pre-restart session uncountable and leave the
// cap open.
func TestInstanceDataCarriesTaskProvenance(t *testing.T) {
	i, err := NewInstance(InstanceOptions{
		Title:   "dlq-triage",
		TaskID:  "abc12345",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)

	data := i.ToInstanceData()
	require.Equal(t, "abc12345", data.TaskID)

	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, "abc12345", back.TaskID, "task provenance must survive the on-disk round-trip")
}

// TestInstanceDataOmitsEmptyTaskProvenance: a user-created session carries no
// task id, and omitempty keeps the key out of its record entirely — the additive
// rollforward every other #1195-era field follows.
func TestInstanceDataOmitsEmptyTaskProvenance(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "mine", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	raw, err := json.Marshal(i.ToInstanceData())
	require.NoError(t, err)
	require.NotContains(t, string(raw), "task_id", "a user-created session must not carry a task_id key")
}

// TestClassifyActivity covers the projection both `af sessions watch` and the
// watch-task concurrency limit read (#1892). Pending means "holds a concurrency
// slot / keep watching"; idle and terminal both release it.
func TestClassifyActivity(t *testing.T) {
	for _, tc := range []struct {
		name string
		data InstanceData
		want Activity
	}{
		// An in-flight op wins over the liveness axis: this is what makes a session
		// count against a cap from the moment its create begins — before any
		// liveness exists and while its post-worktree hooks still run.
		{"creating", InstanceData{InFlightOp: OpCreating}, ActivityPending},
		{"creating outranks a ready liveness", InstanceData{InFlightOp: OpCreating, Liveness: LiveReady}, ActivityPending},
		{"killing", InstanceData{InFlightOp: OpKilling, Liveness: LiveRunning}, ActivityPending},
		{"archiving", InstanceData{InFlightOp: OpArchiving, Liveness: LiveRunning}, ActivityPending},
		{"restoring", InstanceData{InFlightOp: OpRestoring, Liveness: LiveLost}, ActivityPending},

		{"running", InstanceData{Liveness: LiveRunning}, ActivityPending},
		// Parked at a usage limit the daemon auto-resumes (#1146): still the task's
		// work, so it keeps its slot rather than freeing one for a session that
		// would hit the same wall.
		{"usage-limit parked", InstanceData{Liveness: LiveLimitReached}, ActivityPending},
		{"ready", InstanceData{Liveness: LiveReady}, ActivityIdle},
		{"lost", InstanceData{Liveness: LiveLost}, ActivityTerminal},
		{"dead", InstanceData{Liveness: LiveDead}, ActivityTerminal},
		{"archived", InstanceData{Liveness: LiveArchived}, ActivityTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ClassifyActivity(tc.data)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestClassifyActivityLegacyRecords covers the pre-#1195 fallback, which is
// load-bearing rather than vestigial: LivenessForStatus maps the transient
// Loading/Deleting to LiveReady, so resolving a legacy record through the
// liveness axis alone would call a mid-create session idle — freeing a
// concurrency slot it should hold, and telling `sessions watch` a session is
// ready before it started.
func TestClassifyActivityLegacyRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		data InstanceData
		want Activity
	}{
		{"legacy loading holds its slot", InstanceData{Status: Loading}, ActivityPending},
		{"legacy deleting holds its slot", InstanceData{Status: Deleting}, ActivityPending},
		{"legacy running", InstanceData{Status: Running}, ActivityPending},
		{"legacy ready", InstanceData{Status: Ready}, ActivityIdle},
		{"legacy lost", InstanceData{Status: Lost}, ActivityTerminal},
		{"legacy dead", InstanceData{Status: Dead}, ActivityTerminal},
		{"legacy archived", InstanceData{Status: Archived}, ActivityTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, LivenessUnset, tc.data.Liveness, "fixture must exercise the legacy fallback")
			got, _ := ClassifyActivity(tc.data)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestClassifyActivityTerminalReasons: a terminal outcome must explain itself —
// `af sessions watch` exits non-zero with this clause, and it is the only thing
// telling a user their session is recoverable.
func TestClassifyActivityTerminalReasons(t *testing.T) {
	for _, lv := range []Liveness{LiveLost, LiveDead, LiveArchived} {
		got, reason := ClassifyActivity(InstanceData{Liveness: lv})
		require.Equal(t, ActivityTerminal, got)
		require.NotEmpty(t, reason, "a terminal outcome must carry a reason")
	}
}

// TestLifecycleViewActivity: the live-instance entry point must agree with the
// record one. It exists to avoid serializing every session under the daemon's
// manager lock, and an accessor-vs-record disagreement would mean a session that
// holds a concurrency slot as an instance but frees it as a record (or the
// reverse) — the drift the shared state machine exists to prevent.
func TestLifecycleViewActivity(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "live", TaskID: "t1", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	// A fresh instance is idle (NewInstance starts at LiveReady).
	require.Equal(t, ActivityIdle, i.LifecycleView().Activity())
	fromRecord, _ := ClassifyActivity(i.ToInstanceData())
	require.Equal(t, fromRecord, i.LifecycleView().Activity(), "the live and record paths must agree")

	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)))
	require.Equal(t, ActivityPending, i.LifecycleView().Activity(), "a working agent holds its slot")
	fromRecord, _ = ClassifyActivity(i.ToInstanceData())
	require.Equal(t, fromRecord, i.LifecycleView().Activity(), "the live and record paths must agree")

	require.NoError(t, i.Transition(ObserveLiveness(LiveReady)))
	require.Equal(t, ActivityIdle, i.LifecycleView().Activity(), "an idle agent releases its slot")
}

// TestLifecycleViewIsInternallyConsistent: the whole point of the view is that
// every field describes the SAME instant. A caller reaching a verdict from two
// accessor calls can have a concurrent restore land between them and see a state
// that never existed — Lost on one read, Running on the next — which is how the
// #1892 cap undercounted. Pin that the composed Status and the axes agree, since
// those are the two forms the cap and the restore loop read.
func TestLifecycleViewIsInternallyConsistent(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "view", TaskID: "t1", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetStartedForTest(true)

	for _, lv := range []Liveness{LiveRunning, LiveReady, LiveLost} {
		require.NoError(t, i.Transition(ObserveLiveness(lv)))
		v := i.LifecycleView()
		require.Equal(t, lv, v.Liveness)
		require.Equal(t, i.GetStatus(), v.Status, "the composed status must match the axes it was composed from")
		require.True(t, v.Started)
		require.Equal(t, "view", v.Title)
		require.Equal(t, "t1", v.TaskID)
		require.True(t, v.Recoverable, "a local session is recoverable in place")
	}
}

// TestLifecycleViewRaceFree drives LifecycleView against a concurrent mutator: the
// -race detector must see no torn read, and every snapshot must be a state the
// session actually passed through rather than a mix of two.
func TestLifecycleViewRaceFree(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "racy", TaskID: "t1", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetStartedForTest(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 0; n < 500; n++ {
			_ = i.Transition(ObserveLiveness(LiveLost))
			_ = i.Transition(ObserveLiveness(LiveRunning))
		}
	}()
	for n := 0; n < 500; n++ {
		v := i.LifecycleView()
		require.Equal(t, composeStatus(v.Liveness, v.InFlightOp), v.Status,
			"a snapshot must never pair a status with axes it does not compose from")
	}
	<-done
}
