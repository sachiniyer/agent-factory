package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// An on_complete=archive run in its ordinary order (#4853). The lifecycle
// worker's archive commits Archived and persists the row without the marker,
// but the worker clears the in-memory copy only once the archive call returns.
// Any session lookup refreshes, and a refresh inside that window used to read
// the archived row as inert and log a WARNING — about 46 a day on one box, one
// per archived task run. It is not a fault: settle it below WARNING.
func TestArmOwedTaskLifecycle_ArchivedRowSettlesBelowWarning(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const title = "health-sweep-run"
	inst, _ := registerArchivable(t, manager, repoID, repoPath, title)
	inst.SetOwedOnComplete(&session.PendingOnCompleteData{TaskID: "health-sweep", FiledAt: time.Now().Add(-time.Minute)})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.True(t, inst.IsArchived())
	require.NotNil(t, inst.OwedOnComplete(),
		"precondition: the archive leaves the in-memory marker for the lifecycle worker to clear")
	assert.Nil(t, persistedOwedOnComplete(t, repoID, title),
		"the archive's own persist already discharged the obligation on disk")

	require.NoError(t, manager.RefreshInstances())
	manager.backgroundMutationWG.Wait()

	assert.NotContains(t, logs.warnings.String(), "on_complete",
		"the routine archive ordering must not log a warning")
	assert.Contains(t, logs.info.String(), `session "health-sweep-run" is archived; its on_complete obligation filed at`)
	assert.Nil(t, inst.OwedOnComplete(), "the stale in-memory marker is settled")
}

// The inert case is still a warning, and says what the operator should do: a
// marked row whose runtime af cannot confirm never gets its teardown.
func TestArmOwedTaskLifecycle_InertRowWarnsWithRemedy(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const title = "never-started"
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	seedDiskInstance(t, repoID, title, repoPath)
	inst.SetOwedOnComplete(&session.PendingOnCompleteData{TaskID: "nightly", FiledAt: time.Now().Add(-time.Minute)})
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, title)] = inst
	manager.armOwedTaskLifecyclesLocked()
	manager.mu.Unlock()
	manager.backgroundMutationWG.Wait()

	warnings := logs.warnings.String()
	assert.Contains(t, warnings, `session "never-started" is owed an on_complete teardown filed at`)
	assert.Contains(t, warnings, "archive or kill it by hand")
	assert.Nil(t, inst.OwedOnComplete(), "the inert row's obligation is still settled")
}

func persistedOwedOnComplete(t *testing.T, repoID, title string) *session.PendingOnCompleteData {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var rows []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &rows))
	for _, row := range rows {
		if row.Title == title {
			return row.PendingOnComplete
		}
	}
	t.Fatalf("no persisted row for %q", title)
	return nil
}
