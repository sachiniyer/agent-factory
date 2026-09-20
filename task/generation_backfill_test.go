package task

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A row written before generation_id existed must gain one on the daemon's
// durable task load. Without it every session the row spawns is stamped with the
// empty generation, which the lifecycle refuses to reap, so a declared
// on_complete: archive|kill would silently become keep for good (#4224 review).
func TestStableLoadBackfillsGenerationOntoPreFieldRows(t *testing.T) {
	path := setupTestTasks(t, nil)
	require.NoError(t, os.WriteFile(path, []byte(`[
  {"id": "legacy01", "name": "nightly", "prompt": "p", "cron_expr": "0 3 * * *",
   "project_path": "", "program": "claude", "enabled": true, "on_complete": "archive",
   "created_at": "2025-01-01T00:00:00Z", "last_run_status": "started"},
  {"id": "minted01", "name": "minted", "prompt": "p", "cron_expr": "0 4 * * *",
   "project_path": "", "program": "claude", "enabled": true,
   "created_at": "2025-01-01T00:00:00Z", "generation_id": "0123456789abcdef0123456789abcdef"}
]`), 0o644))

	authoritative, updated, err := LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)
	require.Len(t, authoritative, 2)
	legacy := authoritative[0]
	require.Equal(t, "legacy01", legacy.ID)
	assert.True(t, IsBackfilledGeneration(legacy.GenerationID),
		"a pre-field row must leave the load with a backfilled generation, got %q", legacy.GenerationID)
	assert.Equal(t, OnCompleteArchive, legacy.SessionLifecycle(),
		"the backfill must not disturb the row's declared lifecycle")
	assert.Equal(t, "started", legacy.LastRunStatus, "the backfill touches identity only")
	assert.Equal(t, "0123456789abcdef0123456789abcdef", authoritative[1].GenerationID,
		"a row that already has a generation keeps it")
	assert.False(t, IsBackfilledGeneration(authoritative[1].GenerationID))

	require.Len(t, updated, 1, "only the backfilled row is republished")
	assert.Equal(t, "legacy01", updated[0].ID)
	assert.Equal(t, legacy.GenerationID, updated[0].GenerationID)

	stored, err := GetTask("legacy01")
	require.NoError(t, err)
	assert.Equal(t, legacy.GenerationID, stored.GenerationID, "the backfill is durable")
	require.Len(t, stored.Audit, 1)
	assert.Equal(t, ActorDaemonUpgrade, stored.Audit[0].Actor)
	assert.Equal(t, []string{"generation_id"}, stored.Audit[0].Fields)

	// It happens once. A second load neither re-mints nor re-records.
	again, updatedAgain, err := LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)
	assert.Empty(t, updatedAgain)
	assert.Equal(t, legacy.GenerationID, again[0].GenerationID)
	assert.Len(t, auditOf(t, "legacy01"), 1)
}

// The per-repo lifecycle load shares the stable-binding transaction, so it
// backfills too. A row lacking both identities records both in one entry.
func TestRepoScopedStableLoadBackfillsGenerationWithBinding(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := mkScopeRepo(t, "project")
	setupTestTasks(t, []Task{ordinalRow("both0001", "both", "0 9 * * *", repo)})

	repoID := config.ResolveProjectPath(repo).ID
	require.NotEmpty(t, repoID, "precondition: the fixture is a repository")
	tasks, updated, err := LoadTasksForRepoIDWithBindingUpdates(repoID)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Len(t, updated, 1)
	assert.True(t, IsBackfilledGeneration(tasks[0].GenerationID))
	assert.NotEmpty(t, tasks[0].RepoID)

	stored, err := GetTask("both0001")
	require.NoError(t, err)
	require.Len(t, stored.Audit, 1, "one backfill write is one trail entry")
	assert.Equal(t, []string{"generation_id", "repo_id"}, stored.Audit[0].Fields)
}

// Only the backfill mints the marked form. An add, including one that reuses a
// backfilled row's id, mints an ordinary generation, so nothing that trusts the
// marker can mistake a replacement task for the pre-field row it replaced.
func TestAddNeverMintsABackfilledGeneration(t *testing.T) {
	path := setupTestTasks(t, nil)
	require.NoError(t, os.WriteFile(path, []byte(`[{"id": "reuse001", "name": "old", "prompt": "p",
  "cron_expr": "0 3 * * *", "project_path": "", "program": "claude", "enabled": true,
  "created_at": "2025-01-01T00:00:00Z"}]`), 0o644))
	loaded, _, err := LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)
	require.True(t, IsBackfilledGeneration(loaded[0].GenerationID))

	require.NoError(t, RemoveTask("reuse001", ProjectExpectation{}))
	added, err := AddTaskChecked(Task{
		ID: "reuse001", Name: "new", Prompt: "p", CronExpr: "0 3 * * *",
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
		GenerationID: loaded[0].GenerationID,
	}, ActorAPI, nil)
	require.NoError(t, err)
	assert.False(t, IsBackfilledGeneration(added.GenerationID),
		"a client cannot carry the backfilled marker onto a new incarnation")
	assert.NotEqual(t, loaded[0].GenerationID, added.GenerationID)
	assert.False(t, strings.HasPrefix(added.GenerationID, backfilledGenerationPrefix))
}

// An operation that read a pre-field row just before a load backfilled it
// still names that row: the backfill is not a replacement. The CI witness was a
// hand-written tasks.json whose first `af tasks trigger` was refused with "was
// replaced before its run was admitted".
//
// At 28a6f0fa1 the first write below is refused (applied=false).
func TestReadFromBeforeBackfillStillNamesTheRow(t *testing.T) {
	path := setupTestTasks(t, nil)
	require.NoError(t, os.WriteFile(path, []byte(`[{"id": "inflight", "name": "n", "prompt": "p",
  "cron_expr": "0 3 * * *", "project_path": "", "program": "claude", "enabled": true,
  "created_at": "2025-01-01T00:00:00Z"}]`), 0o644))
	before, err := GetTask("inflight")
	require.NoError(t, err)
	require.Empty(t, before.GenerationID, "precondition: the caller read the pre-field row")
	loaded, _, err := LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)
	require.True(t, IsBackfilledGeneration(loaded[0].GenerationID), "precondition: the row was backfilled meanwhile")

	sentAt := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	written, applied, err := UpdateTaskStatusForGeneration("inflight", before.GenerationID, &sentAt, "sent")
	require.NoError(t, err)
	require.True(t, applied, "a task-wide status from a pre-backfill read must land")
	assert.Equal(t, loaded[0].GenerationID, written.GenerationID, "the write keeps the backfilled generation")

	runAt := sentAt.Add(time.Minute)
	_, applied, err = BeginTaskRun("inflight", "", "session-a", 1, written.LastRunRevision, runAt, RunStatusStarted)
	require.NoError(t, err)
	require.True(t, applied, "a run admitted against the pre-field row still publishes")
	_, applied, err = UpdateTaskRunOutcome("inflight", "", "session-a", "interrupted: agent runtime lost")
	require.NoError(t, err)
	assert.True(t, applied)

	// The legacy claim stays exact: there, an empty generation may be a removed
	// pre-field row's session.
	current, err := GetTask("inflight")
	require.NoError(t, err)
	_, applied, err = ClaimUnidentifiedTaskRunOutcome("inflight", "session-b", current.LastRunAt,
		current.LastRunStatus, current.LastRunSessionID, current.LastRunSequence,
		current.LastRunRevision, "", runAt, 2, "interrupted: agent runtime lost")
	require.NoError(t, err)
	assert.False(t, applied)
}

// A row an add minted is a new incarnation. An empty generation never names it.
func TestEmptyGenerationDoesNotNameAMintedRow(t *testing.T) {
	setupTestTasks(t, nil)
	added, err := AddTaskChecked(Task{
		ID: "minted02", Name: "n", Prompt: "p", CronExpr: "0 3 * * *",
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}, ActorAPI, nil)
	require.NoError(t, err)
	_, applied, err := UpdateTaskStatusForGeneration(added.ID, "", nil, "stopped")
	require.NoError(t, err)
	assert.False(t, applied)

	assert.True(t, GenerationStillNames("abc", "abc"))
	assert.True(t, GenerationStillNames("", ""))
	assert.True(t, GenerationStillNames(backfilledGenerationPrefix+"abc", ""))
	assert.False(t, GenerationStillNames("abc", ""))
	assert.False(t, GenerationStillNames("", backfilledGenerationPrefix+"abc"),
		"a backfilled read never names a row that has lost its generation")
	assert.False(t, GenerationStillNames(backfilledGenerationPrefix+"abc", backfilledGenerationPrefix+"def"))
}

// Run admission backfills one row at a time for a row no stable load has read.
func TestEnsureTaskGenerationBackfillsOnlyAPreFieldRow(t *testing.T) {
	path := setupTestTasks(t, nil)
	require.NoError(t, os.WriteFile(path, []byte(`[{"id": "handadd1", "name": "n", "prompt": "p",
  "cron_expr": "0 3 * * *", "project_path": "", "program": "claude", "enabled": true,
  "created_at": "2025-01-01T00:00:00Z"}]`), 0o644))

	backfilled, applied, err := EnsureTaskGeneration("handadd1")
	require.NoError(t, err)
	require.True(t, applied)
	assert.True(t, IsBackfilledGeneration(backfilled.GenerationID))
	stored, err := GetTask("handadd1")
	require.NoError(t, err)
	assert.Equal(t, backfilled.GenerationID, stored.GenerationID, "the backfill is durable")
	require.Len(t, stored.Audit, 1)
	assert.Equal(t, ActorDaemonUpgrade, stored.Audit[0].Actor)
	assert.Equal(t, []string{"generation_id"}, stored.Audit[0].Fields)

	_, applied, err = EnsureTaskGeneration("handadd1")
	require.NoError(t, err)
	assert.False(t, applied, "a row that has a generation keeps it")
	again, err := GetTask("handadd1")
	require.NoError(t, err)
	assert.Equal(t, backfilled.GenerationID, again.GenerationID)
	assert.Len(t, again.Audit, 1)

	_, _, err = EnsureTaskGeneration("missing1")
	assert.True(t, IsTaskNotFound(err))
}
