package task

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAuditTask puts one enabled cron task in a scratch store and returns its id.
func seedAuditTask(t *testing.T, actor Actor) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	_, err := AddTaskChecked(Task{
		ID:          "audit001",
		Name:        "Master Health Watch",
		Prompt:      "sweep",
		CronExpr:    "20 * * * *",
		ProjectPath: dir,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}, actor, nil)
	require.NoError(t, err)
	return "audit001"
}

func auditOf(t *testing.T, id string) []AuditEntry {
	t.Helper()
	stored, err := GetTask(id)
	require.NoError(t, err)
	return stored.Audit
}

// TestAudit_CreateRecordsTheSurface: the trail starts at the create, and it
// names which surface made it.
func TestAudit_CreateRecordsTheSurface(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	trail := auditOf(t, id)
	require.Len(t, trail, 1)
	assert.Equal(t, AuditCreated, trail[0].Action)
	assert.Equal(t, ActorCLI, trail[0].Actor)
	assert.Empty(t, trail[0].Fields, "a create changed everything; naming fields would say nothing")
	assert.False(t, trail[0].At.IsZero(), "an entry with no timestamp answers half the question")
}

// TestAudit_DisableAndEnableRecordTheDIRECTION is the question the whole trail
// exists to answer. #3623's leading explanation — an operator disabled these
// tasks during a fleet pause and re-enabled them 18 days later — was
// unfalsifiable from the box. An entry that only said the enabled field "changed"
// would leave it just as unfalsifiable.
func TestAudit_DisableAndEnableRecordTheDIRECTION(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	off := false
	_, err := UpdateTaskChecked(id, TaskUpdate{Enabled: &off}, ProjectExpectation{}, ActorTUI, nil)
	require.NoError(t, err)
	on := true
	_, err = UpdateTaskChecked(id, TaskUpdate{Enabled: &on}, ProjectExpectation{}, ActorAPI, nil)
	require.NoError(t, err)

	trail := auditOf(t, id)
	require.Len(t, trail, 3)
	assert.Equal(t, AuditDisabled, trail[1].Action)
	assert.Equal(t, ActorTUI, trail[1].Actor)
	assert.Equal(t, []string{"enabled"}, trail[1].Fields)
	assert.Equal(t, AuditEnabled, trail[2].Action)
	assert.Equal(t, ActorAPI, trail[2].Actor)
}

// TestAudit_UpdateNamesTheFieldsThatMoved, and only those: the trail is derived
// from the difference between the stored record and the one that replaced it, so
// it cannot describe a change that did not happen.
func TestAudit_UpdateNamesTheFieldsThatMoved(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	prompt := "sweep harder"
	expr := "35 * * * *"
	_, err := UpdateTaskChecked(id, TaskUpdate{Prompt: &prompt, CronExpr: &expr}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, id)
	require.Len(t, trail, 2)
	assert.Equal(t, AuditUpdated, trail[1].Action)
	assert.Equal(t, []string{"prompt", "cron_expr"}, trail[1].Fields)
}

// TestAudit_NoOpPatchRecordsNothing: an empty or value-preserving patch is a
// well-formed no-op, and a trail full of "updated: nothing" would push the
// entries that matter out of the bounded window.
func TestAudit_NoOpPatchRecordsNothing(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	same := "sweep"
	_, err := UpdateTaskChecked(id, TaskUpdate{Prompt: &same}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	_, err = UpdateTaskChecked(id, TaskUpdate{}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	assert.Len(t, auditOf(t, id), 1, "neither patch changed a stored value")
}

// TestAudit_BoundedAtTwentyEntries: a task edited in a loop must not grow
// tasks.json without limit, and the entries that survive must be the RECENT
// ones — the trail is read to explain the state in front of you.
func TestAudit_BoundedAtTwentyEntries(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("Master Health Watch %d", i)
		_, err := UpdateTaskChecked(id, TaskUpdate{Name: &name}, ProjectExpectation{}, ActorCLI, nil)
		require.NoError(t, err)
	}

	trail := auditOf(t, id)
	require.Len(t, trail, AuditLimit)
	assert.Equal(t, AuditUpdated, trail[0].Action,
		"the create fell off the front once 26 mutations had happened")
	for i := 1; i < len(trail); i++ {
		assert.False(t, trail[i].At.Before(trail[i-1].At), "entries stay in commit order")
	}
}

// TestAudit_UnknownActorIsRecordedAsSuch: an undeclared or unrecognized surface
// is stored as the explicit "unknown" rather than blank or verbatim. Blank reads
// as "no entry" to anyone scanning the trail, and a verbatim label would later be
// read as a surface that exists.
func TestAudit_UnknownActorIsRecordedAsSuch(t *testing.T) {
	id := seedAuditTask(t, Actor("some-future-client"))
	assert.Equal(t, ActorUnknown, auditOf(t, id)[0].Actor)

	off := false
	_, err := UpdateTaskChecked(id, TaskUpdate{Enabled: &off}, ProjectExpectation{}, "", nil)
	require.NoError(t, err)
	assert.Equal(t, ActorUnknown, auditOf(t, id)[1].Actor)
}

// TestAudit_RejectedMutationLeavesNoEntry: the entry is stamped inside the same
// locked operation that commits, so a validator refusal cannot leave a record of
// a change that never landed.
func TestAudit_RejectedMutationLeavesNoEntry(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	off := false
	_, err := UpdateTaskChecked(id, TaskUpdate{Enabled: &off}, ProjectExpectation{}, ActorCLI,
		func(Task) (string, error) { return "", fmt.Errorf("refused") })
	require.Error(t, err)

	trail := auditOf(t, id)
	require.Len(t, trail, 1, "only the create happened")
	stored, err := GetTask(id)
	require.NoError(t, err)
	assert.True(t, stored.Enabled, "and the refusal really did leave the task alone")
}

// TestAudit_StatusUpdatesAreNotAudited: every run bumps LastRunAt/LastRunStatus,
// and auditing those would evict the enable/disable entries the bounded window
// exists to keep.
func TestAudit_StatusUpdatesAreNotAudited(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	ran := time.Now()
	for i := 0; i < 30; i++ {
		require.NoError(t, UpdateTaskStatus(id, &ran, "started"))
	}

	assert.Len(t, auditOf(t, id), 1)
}

// TestAudit_DaemonBackfillIsRecorded: the RepoID backfill is a write nobody
// asked for, which is precisely the class of change #3623 says a user cannot
// otherwise distinguish from one they made. It happens at most once per legacy
// row, so it cannot crowd the bounded trail.
func TestAudit_DaemonBackfillIsRecorded(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	// Written while the path is NOT yet a repository, so RepoID stays empty —
	// the legacy shape, produced without hand-writing tasks.json.
	require.NoError(t, AddTask(Task{
		ID: "legacy01", Name: "Legacy", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: repo, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}))
	stored, err := GetTask("legacy01")
	require.NoError(t, err)
	require.Empty(t, stored.RepoID, "precondition: the row has no retained binding to start with")

	require.NoError(t, exec.Command("git", "init", repo).Run())
	_, _, err = LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)

	stored, err = GetTask("legacy01")
	require.NoError(t, err)
	require.NotEmpty(t, stored.RepoID, "precondition: the backfill actually happened")
	require.Len(t, stored.Audit, 2)
	assert.Equal(t, ActorDaemonUpgrade, stored.Audit[1].Actor)
	assert.Equal(t, []string{"repo_id"}, stored.Audit[1].Fields)

	// And it does not repeat: the row now has a RepoID, so the backfill skips it.
	_, _, err = LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)
	assert.Len(t, auditOf(t, "legacy01"), 2)
}

// TestAudit_ClientSuppliedTrailIsDiscarded: the audit trail is store-owned
// history, and AddTaskRequest carries a whole task.Task — so without this a
// client could persist changes that never happened. Worse, since lateness is
// measured from the most recent enable in that trail, a forged FUTURE entry
// would push the reference point forward and switch overdue detection off for
// that task indefinitely.
func TestAudit_ClientSuppliedTrailIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	forged := time.Now().Add(100 * 24 * time.Hour)
	_, err := AddTaskChecked(Task{
		ID: "forged01", Name: "Forged", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true, CreatedAt: time.Now(),
		Audit: []AuditEntry{
			{At: forged, Actor: ActorCLI, Action: AuditEnabled, Fields: []string{"enabled"}},
			{At: time.Now(), Actor: ActorAPI, Action: AuditUpdated, Fields: []string{"prompt"}},
		},
	}, ActorAPI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "forged01")
	require.Len(t, trail, 1, "a create has exactly one entry, and the store writes it")
	assert.Equal(t, AuditCreated, trail[0].Action)
	assert.True(t, trail[0].At.Before(forged), "no future timestamp survived into the record")
}

// TestAddTask_StampsCreatedAt: an HTTP client can omit created_at, and a record
// with neither a run nor a creation time is exactly the never-fired task the
// health derivation is meant to catch — it would have reported healthy forever.
// The store cannot leave that to the caller.
func TestAddTask_StampsCreatedAt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	before := time.Now()
	created, err := AddTaskChecked(Task{
		ID: "nostamp1", Name: "No stamp", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,
	}, ActorAPI, nil)
	require.NoError(t, err)
	assert.False(t, created.CreatedAt.IsZero(), "the returned record carries the stamp")
	assert.False(t, created.CreatedAt.Before(before))

	stored, err := GetTask("nostamp1")
	require.NoError(t, err)
	require.False(t, stored.CreatedAt.IsZero(), "and so does the stored one")

	// And the derivation can therefore measure it: two occurrences after the
	// stamp with no run is overdue, where before it was silently underivable.
	future := stored.CreatedAt.Add(3 * time.Hour)
	assert.True(t, DeriveScheduleHealth(*stored, future).Overdue)
}

// TestAddTask_KeepsAnExplicitCreatedAt: the stamp is a floor for records that
// have none, not an override — a caller migrating a task must keep its history.
func TestAddTask_KeepsAnExplicitCreatedAt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	want := time.Now().Add(-72 * time.Hour)
	created, err := AddTaskChecked(Task{
		ID: "hasstamp", Name: "Has stamp", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true, CreatedAt: want,
	}, ActorCLI, nil)
	require.NoError(t, err)
	assert.True(t, want.Equal(created.CreatedAt))
}
