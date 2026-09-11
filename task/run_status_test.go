package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskRunOutcomeClosesIdentifiedRunAfterArmingStatusClears(t *testing.T) {
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	setupTestTasks(t, []Task{{ID: "w1", WatchCmd: "tail -f x", Enabled: true}})
	_, _, err := BeginTaskRun("w1", "", "session-a", 1, 0, runAt, RunStatusStarted)
	require.NoError(t, err)
	_, err = UpdateTaskStatus("w1", nil, "errored: not armed: target unavailable")
	require.NoError(t, err)
	_, err = UpdateTaskStatus("w1", nil, "")
	require.NoError(t, err)

	_, applied, err := UpdateTaskRunOutcome("w1", "", "session-a", "interrupted: agent runtime lost")
	require.NoError(t, err)
	assert.True(t, applied,
		"clearing a temporary arming refusal must not make the identified active run uncloseable")
	got, err := GetTask("w1")
	require.NoError(t, err)
	assert.Equal(t, "interrupted: agent runtime lost", got.LastRunStatus)
	assert.Equal(t, "session-a", got.LastRunSessionID)
}

func TestTaskRunPublicationDoesNotOverwriteWatcherTerminationAfterAdmission(t *testing.T) {
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	setupTestTasks(t, []Task{{ID: "w1", WatchCmd: "tail -f x", Enabled: true}})

	// Session creation admitted this run against the blank row, then blocked in
	// provisioning long enough for the watch process to terminate.
	admitted, err := GetTask("w1")
	require.NoError(t, err)
	_, err = UpdateTaskStatus("w1", nil, "errored: watcher exited")
	require.NoError(t, err)

	_, applied, err := BeginTaskRun(
		"w1", admitted.GenerationID, "session-a", admitted.LastRunSequence+1,
		admitted.LastRunRevision, runAt, RunStatusStarted)
	require.NoError(t, err)
	assert.False(t, applied,
		"publication admitted before watcher termination must not overwrite that later terminal observation")
	got, err := GetTask("w1")
	require.NoError(t, err)
	assert.Equal(t, "errored: watcher exited", got.LastRunStatus)

	// A later admission observes the terminal write's revision and may start a
	// genuinely newer run; the CAS fences only stale publication, not the status.
	_, applied, err = BeginTaskRun(
		"w1", got.GenerationID, "session-b", 2, got.LastRunRevision, runAt.Add(time.Minute), RunStatusStarted)
	require.NoError(t, err)
	assert.True(t, applied)
}

func TestConcurrentTaskRunPublicationsUseSequenceWithinOneAdmissionRevision(t *testing.T) {
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	setupTestTasks(t, []Task{{ID: "w1", WatchCmd: "tail -f x", Enabled: true}})
	admitted, err := GetTask("w1")
	require.NoError(t, err)

	_, applied, err := BeginTaskRun(
		"w1", admitted.GenerationID, "session-a", 1, admitted.LastRunRevision, runAt, RunStatusStarted)
	require.NoError(t, err)
	require.True(t, applied)
	_, applied, err = BeginTaskRun(
		"w1", admitted.GenerationID, "session-b", 2, admitted.LastRunRevision,
		runAt.Add(time.Second), RunStatusStarted)
	require.NoError(t, err)
	assert.True(t, applied,
		"a higher-sequence run admitted against the same row must supersede the earlier publication")
	got, err := GetTask("w1")
	require.NoError(t, err)
	assert.Equal(t, "session-b", got.LastRunSessionID)
	assert.Equal(t, uint64(2), got.LastRunSequence)
}

func TestTaskStatusForGenerationRefusesReplacementRow(t *testing.T) {
	runAt := time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC)
	setupTestTasks(t, []Task{{
		ID: "w1", GenerationID: "replacement-generation", WatchCmd: "tail -f x", Enabled: true,
	}})

	_, applied, err := UpdateTaskStatusForGeneration(
		"w1", "removed-generation", &runAt, "sent")
	require.NoError(t, err)
	require.False(t, applied)
	got, err := GetTask("w1")
	require.NoError(t, err)
	require.Empty(t, got.LastRunStatus)
	require.Nil(t, got.LastRunAt)

	_, applied, err = UpdateTaskStatusForGeneration(
		"w1", "replacement-generation", &runAt, "sent")
	require.NoError(t, err)
	require.True(t, applied)
	got, err = GetTask("w1")
	require.NoError(t, err)
	require.Equal(t, "sent", got.LastRunStatus)
	require.NotNil(t, got.LastRunAt)
	require.True(t, got.LastRunAt.Equal(runAt))
}
