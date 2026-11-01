package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func backlogRunSession(t *testing.T) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{
		Title:       "B-7 item",
		Path:        t.TempDir(),
		Program:     "claude",
		BacklogItem: 7,
	})
	require.NoError(t, err)
	inst.SetBackend(NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(Running)
	return inst
}

func TestBacklogRunArmedAtCreate(t *testing.T) {
	inst := backlogRunSession(t)
	require.True(t, inst.BacklogRunActive())
	require.False(t, inst.TaskRunActive())
	v := inst.LifecycleView()
	require.Equal(t, 7, v.BacklogItem)
	require.True(t, v.BacklogRunActive)
}

func TestBacklogRunEndsOnIdleEdgeAfterTurn(t *testing.T) {
	inst := backlogRunSession(t)
	attemptedAt := time.Now().Add(-10 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.BacklogRunActive(), "an idle edge before the turn is held")

	require.True(t, inst.RecordTaskRunTurn(attemptedAt.Add(2*time.Second)))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.BacklogRunActive(), "the held edge ends the run once the turn was observed")
}

func TestBacklogRunReArmsOnReadySession(t *testing.T) {
	inst := backlogRunSession(t)
	first := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, first))
	require.True(t, inst.RecordTaskRunTurn(first.Add(time.Second)))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.BacklogRunActive(), "precondition: the first run ended")

	armedAt := time.Now().Add(-10 * time.Second)
	require.True(t, inst.ArmBacklogRun(armedAt))
	require.True(t, inst.BacklogRunActive())

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.BacklogRunActive(), "a Ready → Ready tick before the turn does not end the re-armed run")

	require.True(t, inst.RecordTaskRunTurn(armedAt.Add(3*time.Second)))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.BacklogRunActive(), "the held edge ends the run on a Ready → Ready tick after the turn")
}

func TestBacklogRunArmRefusedOutsideItemSession(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "plain", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.False(t, inst.ArmBacklogRun(time.Now()))
	require.False(t, inst.BacklogRunActive())
}

func TestBacklogRunClearedByCommitArchive(t *testing.T) {
	inst := backlogRunSession(t)
	require.NoError(t, inst.Transition(BeginArchive()))
	require.NoError(t, inst.Transition(CommitArchive()))
	require.False(t, inst.BacklogRunActive())
}

func TestBacklogRunDisarm(t *testing.T) {
	inst := backlogRunSession(t)
	require.True(t, inst.DisarmBacklogRun())
	require.False(t, inst.BacklogRunActive())
	require.False(t, inst.DisarmBacklogRun())
}

func TestBacklogRunSurvivesRoundTrip(t *testing.T) {
	inst := backlogRunSession(t)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now()))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

	data := inst.ToInstanceData()
	require.Equal(t, 7, data.BacklogItem)
	require.True(t, data.BacklogRunActive)
	require.False(t, data.TaskRunActive)
	require.True(t, data.TaskRunTurnGateHeld, "the gate fields persist for a backlog run")
	require.False(t, data.TaskRunPromptAttemptAt.IsZero())

	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.Equal(t, 7, reloaded.BacklogItem)
	require.True(t, reloaded.BacklogRunActive())
	require.True(t, reloaded.taskRunTurnGateHeld)
}

func TestBacklogRunMarkerIgnoredWithoutItem(t *testing.T) {
	inst := backlogRunSession(t)
	data := inst.ToInstanceData()
	data.BacklogItem = 0
	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.False(t, reloaded.BacklogRunActive())
}

func TestReconcileBacklogRunState(t *testing.T) {
	inst := backlogRunSession(t)
	require.False(t, inst.ReconcileBacklogRunState(true))
	require.True(t, inst.ReconcileBacklogRunState(false))
	require.False(t, inst.BacklogRunActive())
}

func TestMarkStartupStateUnknownEndsBacklogRun(t *testing.T) {
	inst := backlogRunSession(t)
	inst.MarkStartupStateUnknown()
	require.False(t, inst.BacklogRunActive())
}

func TestBacklogRunEndRecordsFenceBaseline(t *testing.T) {
	inst := backlogRunSession(t)
	require.True(t, inst.BacklogRunEndedAt().IsZero())
	attemptedAt := time.Now().Add(-10 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
	require.True(t, inst.RecordTaskRunTurn(attemptedAt.Add(time.Second)))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.BacklogRunActive())
	require.False(t, inst.BacklogRunEndedAt().IsZero(), "the idle edge records when the run ended")
	require.Equal(t, inst.AdoptionDeliveries(), inst.AdoptionDeliveriesAtRunEnd(), "the run end takes the adoption baseline")

	require.True(t, inst.ArmBacklogRun(time.Now()))
	require.True(t, inst.BacklogRunEndedAt().IsZero(), "a new run clears the end time")
}

// Disarming a continuation that never started puts the previous run's end back
// (Codex P2 on #6294): arming cleared it, and pane churn since would otherwise
// read as "changed after its run ended" and block the on_done archive forever.
func TestDisarmRestoresThePreviousRunEnd(t *testing.T) {
	inst := backlogRunSession(t)
	inst.EndBacklogRunForTest()
	ended := inst.BacklogRunEndedAt()
	require.False(t, ended.IsZero())

	require.True(t, inst.ArmBacklogRun(time.Now()))
	require.True(t, inst.BacklogRunEndedAt().IsZero(), "an armed run has no end yet")
	require.True(t, inst.DisarmBacklogRun())
	require.True(t, inst.BacklogRunEndedAt().Equal(ended), "the failed continuation must leave the previous run's end alone")

	// A re-arm while a run is already active keeps the ORIGINAL previous end.
	require.True(t, inst.ArmBacklogRun(time.Now()))
	require.True(t, inst.ArmBacklogRun(time.Now()))
	require.True(t, inst.DisarmBacklogRun())
	require.True(t, inst.BacklogRunEndedAt().Equal(ended))
}

// The owed archive is filed before the archive runs, so it must come back from
// disk; a new run drops it, and a session that is not an item session owes none.
func TestOwedArchiveSurvivesRoundTripAndANewRunDropsIt(t *testing.T) {
	inst := backlogRunSession(t)
	filed := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	inst.SetOwedArchive("ask", filed)

	data := inst.ToInstanceData()
	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.Equal(t, &BacklogArchiveData{Reason: "ask", FiledAt: filed}, reloaded.OwedArchive())

	data.BacklogItem = 0
	plain, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.Nil(t, plain.OwedArchive())

	require.True(t, reloaded.ArmBacklogRun(time.Now()))
	require.Nil(t, reloaded.OwedArchive(), "a new run drops the archive owed to the one before")
	require.Nil(t, reloaded.ToInstanceData().BacklogArchive)
}

// Once af archived the session for its question the record is no longer owed:
// it marks the archived row as waiting on the user, survives a round trip, and
// goes when the session leaves the archive.
func TestArchivedForAnswerMarksTheRowUntilItLeavesTheArchive(t *testing.T) {
	inst := backlogRunSession(t)
	filed := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	inst.SetOwedArchive("ask", filed)
	require.NoError(t, inst.Transition(BeginArchive()))
	require.NoError(t, inst.Transition(CommitArchive()))
	require.False(t, inst.AwaitsAnswer(), "an archive still owed does not mark the row")
	inst.MarkArchivedForAnswer("ask", filed.Add(time.Minute))
	require.True(t, inst.AwaitsAnswer())
	require.Nil(t, inst.OwedArchive(), "a done archive is not owed")
	require.Equal(t, filed, inst.ToInstanceData().BacklogArchive.FiledAt)

	data := inst.ToInstanceData()
	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.True(t, reloaded.AwaitsAnswer())

	require.NoError(t, inst.Transition(BeginRestore()))
	require.False(t, inst.AwaitsAnswer())
	require.Nil(t, inst.ToInstanceData().BacklogArchive, "a session that left the archive no longer waits there")

	require.True(t, reloaded.ClearBacklogArchive())
	require.False(t, reloaded.AwaitsAnswer())
	require.False(t, reloaded.ClearBacklogArchive())
}

func TestReconcileBacklogArchiveSnapshot(t *testing.T) {
	inst := backlogRunSession(t)
	rec := &BacklogArchiveData{Reason: "ask", FiledAt: time.Unix(1, 0), ArchivedAt: time.Unix(2, 0)}
	require.True(t, inst.ReconcileBacklogArchiveSnapshot(rec))
	require.False(t, inst.ReconcileBacklogArchiveSnapshot(&BacklogArchiveData{Reason: "ask", FiledAt: time.Unix(1, 0), ArchivedAt: time.Unix(2, 0)}))
	rec.Reason = "changed"
	require.Equal(t, "ask", inst.ToInstanceData().BacklogArchive.Reason, "the row keeps its own copy")
	require.True(t, inst.ReconcileBacklogArchiveSnapshot(nil))
	require.False(t, inst.ReconcileBacklogArchiveSnapshot(nil))
}
