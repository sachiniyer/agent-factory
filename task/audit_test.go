package task

import (
	"encoding/json"
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
		_, err := UpdateTaskStatus(id, &ran, "started")
		require.NoError(t, err)
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

// TestAddTask_ClampsAFutureCreatedAt is the sanitized audit trail's twin through
// a different field: `scheduleReference` trusts CreatedAt for a task that has
// never run, so a stamp dated next year switches overdue detection off until
// then. Clamped rather than rejected — no client agrees with this clock to the
// second, and refusing an add over a second of skew is a worse bargain than
// correcting a value that is never legitimately in the future.
func TestAddTask_ClampsAFutureCreatedAt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	created, err := AddTaskChecked(Task{
		ID: "future01", Name: "From the future", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,
		CreatedAt: time.Now().Add(365 * 24 * time.Hour),
	}, ActorAPI, nil)
	require.NoError(t, err)
	assert.False(t, created.CreatedAt.After(time.Now().Add(time.Minute)),
		"the stamp was pulled back to the store's own clock")

	stored, err := GetTask("future01")
	require.NoError(t, err)
	assert.True(t, DeriveScheduleHealth(*stored, stored.CreatedAt.Add(3*time.Hour)).Overdue,
		"and overdue detection reaches the task again")
}

// TestAddTask_DiscardsClientSuppliedRunHistory: run status and dropped-event
// history are daemon-owned by contract, and `scheduleReference` prefers a nonzero
// LastRunAt over CreatedAt — so a create carrying a future run time claims the
// task just ran and switches overdue detection off until that date. Third
// variant of one defect: the request carries a whole task.Task, so the store
// resets everything it owns rather than the field last reported.
func TestAddTask_DiscardsClientSuppliedRunHistory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	forged := time.Now().Add(365 * 24 * time.Hour)
	created, err := AddTaskChecked(Task{
		ID: "history1", Name: "Forged history", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,
		LastRunAt: &forged, LastRunStatus: "started", DroppedEvents: 99,
	}, ActorAPI, nil)
	require.NoError(t, err)
	assert.Nil(t, created.LastRunAt, "a task that has never run has no run time")
	assert.Empty(t, created.LastRunStatus)
	assert.Zero(t, created.DroppedEvents)

	stored, err := GetTask("history1")
	require.NoError(t, err)
	require.Nil(t, stored.LastRunAt)
	assert.True(t, DeriveScheduleHealth(*stored, stored.CreatedAt.Add(3*time.Hour)).Overdue,
		"and the derivation reaches the task instead of waiting a year")
}

// TestAddTask_ResetsEveryStoreOwnedField is the class rather than its members:
// three review rounds found the same defect through three different fields, so
// the create path resets all of them together.
func TestAddTask_ResetsEveryStoreOwnedField(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	future := time.Now().Add(48 * time.Hour)
	next := time.Now().Add(time.Hour)
	created, err := AddTaskChecked(Task{
		ID: "kitchen1", Name: "Everything at once", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,
		CreatedAt: future, LastRunAt: &future, LastRunStatus: "started",
		DroppedEvents: 99,
		Audit:         []AuditEntry{{At: future, Actor: ActorCLI, Action: AuditEnabled}},
		Overdue:       true, MissedOccurrences: 99, MissedOccurrencesCapped: true,
		Unschedulable: true, Arming: ArmingArmed, NextRunAt: &next,
	}, ActorAPI, nil)
	require.NoError(t, err)

	assert.False(t, created.CreatedAt.After(time.Now().Add(time.Minute)), "created_at clamped")
	assert.Nil(t, created.LastRunAt)
	assert.Empty(t, created.LastRunStatus)
	assert.Zero(t, created.DroppedEvents)
	require.Len(t, created.Audit, 1, "only the store's own create entry")
	assert.Equal(t, AuditCreated, created.Audit[0].Action)
	assert.False(t, created.Overdue, "the response must not echo a health verdict the client invented")
	assert.Zero(t, created.MissedOccurrences)
	assert.False(t, created.MissedOccurrencesCapped)
	assert.False(t, created.Unschedulable)
	assert.Empty(t, created.Arming)
	assert.Nil(t, created.NextRunAt)
}

// TestAudit_ValidatorBackfilledRepoIDIsRecorded: the daemon resolves a legacy
// row's binding as a side effect of someone else's patch, and that write is
// durable. It is recorded here or nowhere — changedFields covers only patchable
// fields, and once RepoID is set the stable-binding loader (which writes this
// same daemon-upgrade entry on the path it owns) skips the row forever.
func TestAudit_ValidatorBackfilledRepoIDIsRecorded(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	// A no-op patch: the caller changed nothing, and the backfill still happens.
	_, err := UpdateTaskChecked(id, TaskUpdate{}, ProjectExpectation{}, ActorCLI,
		func(Task) (string, error) { return "resolved-repo-id", nil })
	require.NoError(t, err)

	stored, err := GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "resolved-repo-id", stored.RepoID)
	require.Len(t, stored.Audit, 2, "the create, and the daemon's own backfill")
	assert.Equal(t, ActorDaemonUpgrade, stored.Audit[1].Actor,
		"nobody asked for it, so it is not attributed to the caller")
	assert.Equal(t, []string{"repo_id"}, stored.Audit[1].Fields)
}

// TestAudit_ValidatorRepoIDThatChangesNothingRecordsNothing keeps the entry
// meaning "a binding was resolved" rather than "a validator ran".
func TestAudit_ValidatorRepoIDThatChangesNothingRecordsNothing(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)
	resolve := func(Task) (string, error) { return "resolved-repo-id", nil }

	// The first patch backfills and records. The second sees the SAME resolved id
	// on a row that already carries it — a validator running, not a binding being
	// resolved — and must add nothing.
	_, err := UpdateTaskChecked(id, TaskUpdate{}, ProjectExpectation{}, ActorCLI, resolve)
	require.NoError(t, err)
	require.Len(t, auditOf(t, id), 2)

	_, err = UpdateTaskChecked(id, TaskUpdate{}, ProjectExpectation{}, ActorCLI, resolve)
	require.NoError(t, err)
	assert.Len(t, auditOf(t, id), 2, "a validator that changed nothing recorded nothing")
}

// TestAudit_SamePathBackfillIsRecorded: `af tasks update <id> --project-path
// <the path it already has>` resolves and persists the RepoID before the
// validator ever sees a difference to report — so keying the audit on the
// validator's branch missed it, and changedFields sees no project_path change
// either. A durable write with no trace, which is the one thing this trail
// cannot do.
func TestAudit_SamePathBackfillIsRecorded(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	// Written while the path is not yet a repository, so RepoID starts empty.
	require.NoError(t, AddTask(Task{
		ID: "samepath", Name: "Legacy", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: repo, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}))
	require.NoError(t, exec.Command("git", "init", repo).Run())

	same := repo
	_, err := UpdateTaskChecked("samepath", TaskUpdate{ProjectPath: &same}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("samepath")
	require.NoError(t, err)
	require.NotEmpty(t, stored.RepoID, "precondition: the backfill happened")
	require.Len(t, stored.Audit, 2, "the create, and the binding the daemon resolved")
	assert.Equal(t, ActorDaemonUpgrade, stored.Audit[1].Actor)
	assert.Equal(t, []string{"repo_id"}, stored.Audit[1].Fields)
}

// TestAudit_ARealRebindIsTheUsersChange: moving a task to another repository
// also changes RepoID, but the user asked for that and project_path is already
// in their entry. A second daemon-upgrade line there would be noise.
func TestAudit_ARealRebindIsTheUsersChange(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	from := filepath.Join(t.TempDir(), "from")
	to := filepath.Join(t.TempDir(), "to")
	for _, dir := range []string{from, to} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, exec.Command("git", "init", dir).Run())
	}
	require.NoError(t, AddTask(Task{
		ID: "rebind01", Name: "Mover", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: from, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}))

	_, err := UpdateTaskChecked("rebind01", TaskUpdate{ProjectPath: &to}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "rebind01")
	require.Len(t, trail, 2, "the create, and the user's move — not a third for the derived id")
	assert.Equal(t, ActorCLI, trail[1].Actor)
	assert.Equal(t, []string{"project_path"}, trail[1].Fields)
}

// handEditedStore plants a v1 envelope of exactly these tasks on a scratch path
// and pins getTasksPath at it. It is how a non-canonical row reaches the audit
// diff in the first place: every in-process writer canonicalizes on write
// (AddTaskChecked, apply), so on_complete="Archive" or target_session="   " can
// only land on disk through a hand-edit or a dotfiles import of tasks.json — the
// reachable shape the existing canonical-only fixtures never exercised. The
// returned rewriter overwrites the same path so a test can reset the disk
// between independent probes.
func handEditedStore(t *testing.T, tasks []Task) (path string, rewrite func([]Task)) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, tasksFileName)
	write := func(tasks []Task) {
		envelope := struct {
			SchemaVersion int    `json:"schema_version"`
			Tasks         []Task `json:"tasks"`
		}{SchemaVersion: TasksSchemaVersion, Tasks: tasks}
		data, err := json.Marshal(envelope)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0644))
	}
	write(tasks)
	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })
	return path, write
}

// handEditedCronTask is a minimal enabled cron task with the requested
// on_complete and target_session, in exactly the byte shape a hand-edit would
// plant. The two are mutually exclusive in these fixtures: a non-keep lifecycle
// with a target session is a shape ValidateTrigger rejects, so the two
// canonicalization paths are seeded on separate rows.
func handEditedCronTask(id, onComplete, targetSession string) Task {
	return Task{
		ID:            id,
		Name:          "x",
		Prompt:        "orig",
		CronExpr:      "0 9 * * *",
		ProjectPath:   "/tmp",
		Program:       "claude",
		Enabled:       true,
		CreatedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		OnComplete:    onComplete,
		TargetSession: targetSession,
	}
}

// TestAudit_CanonicalizationIsNotAttributedToTheCaller is the headline regression
// for the misattribution: apply canonicalizes on_complete/target_session on every
// write, and changedFields must not fold that store normalization into the
// caller's entry. A hand-edited "Archive" row, patched through an empty
// TaskUpdate{} or a prompt-only patch, must NOT record on_complete as a field the
// caller moved. The byte-change is recorded separately as ActorDaemonUpgrade,
// mirroring the repo_id backfill — the store wrote bytes the caller never asked
// for, and the trail says so under the store's own actor.
func TestAudit_CanonicalizationIsNotAttributedToTheCaller(t *testing.T) {
	_, rewrite := handEditedStore(t, []Task{handEditedCronTask("hand0001", "Archive", "")})

	// Empty patch — a write the caller did not request. apply canonicalizes
	// "Archive"→"archive" and writeTasks persists it; the audit must attribute
	// that byte-change to the store, not to the undeclared (ActorUnknown) caller.
	_, err := UpdateTask("hand0001", TaskUpdate{}, ProjectExpectation{})
	require.NoError(t, err)

	trail := auditOf(t, "hand0001")
	require.Len(t, trail, 1, "the empty patch changed only a store-normalized byte")
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor,
		"the canonicalization is the store's repair, not a field the caller moved")
	assert.Equal(t, AuditUpdated, trail[0].Action)
	assert.Equal(t, []string{"on_complete"}, trail[0].Fields)
	stored, err := GetTask("hand0001")
	require.NoError(t, err)
	assert.Equal(t, OnCompleteArchive, stored.OnComplete,
		"the canonicalization really did land on disk — a real byte-change was recorded")

	// Prompt-only patch — the CLI moved prompt, not on_complete. Reset the disk to
	// the non-canonical "Archive" so the canonicalization fires again, then prove
	// the CLI's entry names prompt only, and on_complete is a separate
	// daemon-upgrade line the CLI never asked for.
	rewrite([]Task{handEditedCronTask("hand0001", "Archive", "")})
	np := "changed"
	_, err = UpdateTaskChecked("hand0001", TaskUpdate{Prompt: &np}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail = auditOf(t, "hand0001")
	require.Len(t, trail, 2,
		"the CLI's move, and the store's canonicalization — not one entry attributing both to the CLI")
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor, "the store's canonicalization is recorded first, as repo_id is")
	assert.Equal(t, []string{"on_complete"}, trail[0].Fields)
	assert.Equal(t, ActorCLI, trail[1].Actor, "the CLI's entry is the prompt it moved")
	assert.Equal(t, []string{"prompt"}, trail[1].Fields,
		"on_complete is the store's canonicalization, not the CLI's change — an operator chasing an on_complete regression must not be sent to a CLI edit that never touched it")
}

// TestAudit_OnCompleteCanonicalizationVariants covers the normalizations
// CanonicalOnComplete performs (lowercase, trim, and the keep-stored-as-empty
// rule). Each is a canonical-equivalent byte-change on a hand-edited row, so
// each must record exactly one ActorDaemonUpgrade entry and never attribute the
// field to the caller.
func TestAudit_OnCompleteCanonicalizationVariants(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantDisk string
	}{
		{"capitalized archive", "Archive", OnCompleteArchive},
		{"capitalized kill", "Kill", OnCompleteKill},
		{"padded archive", "  archive  ", OnCompleteArchive},
		{"capitalized keep stored as empty", "Keep", ""},
		{"whitespace keep stored as empty", "   ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handEditedStore(t, []Task{handEditedCronTask("hand01", tc.raw, "")})

			_, err := UpdateTask("hand01", TaskUpdate{}, ProjectExpectation{})
			require.NoError(t, err)

			trail := auditOf(t, "hand01")
			require.Len(t, trail, 1, "a store canonicalization and nothing else")
			assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor)
			assert.Equal(t, AuditUpdated, trail[0].Action)
			assert.Equal(t, []string{"on_complete"}, trail[0].Fields)

			stored, err := GetTask("hand01")
			require.NoError(t, err)
			assert.Equal(t, tc.wantDisk, stored.OnComplete, "the canonicalization landed on disk")
		})
	}
}

// TestAudit_TargetSessionCanonicalizationIsNotAttributedToTheCaller: a
// whitespace-only target_session canonicalizes to "" (no target session) on
// every write. A hand-edited row holding "   " must record that byte-change as
// ActorDaemonUpgrade, not as a field the caller moved — the same rule as
// on_complete, for the other canonicalizing field.
func TestAudit_TargetSessionCanonicalizationIsNotAttributedToTheCaller(t *testing.T) {
	_, rewrite := handEditedStore(t, []Task{handEditedCronTask("hand0002", "", "   ")})

	_, err := UpdateTask("hand0002", TaskUpdate{}, ProjectExpectation{})
	require.NoError(t, err)

	trail := auditOf(t, "hand0002")
	require.Len(t, trail, 1, "the empty patch changed only a store-normalized byte")
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor)
	assert.Equal(t, []string{"target_session"}, trail[0].Fields)
	stored, err := GetTask("hand0002")
	require.NoError(t, err)
	assert.Empty(t, stored.TargetSession, "the whitespace target was canonicalized to no target on disk")

	// An unrelated patch on the same hand-edited row: the caller moved prompt, and
	// the whitespace-target canonicalization is a separate daemon-upgrade line.
	rewrite([]Task{handEditedCronTask("hand0002", "", "   ")})
	np := "changed"
	_, err = UpdateTaskChecked("hand0002", TaskUpdate{Prompt: &np}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	trail = auditOf(t, "hand0002")
	require.Len(t, trail, 2)
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor)
	assert.Equal(t, []string{"target_session"}, trail[0].Fields)
	assert.Equal(t, ActorCLI, trail[1].Actor)
	assert.Equal(t, []string{"prompt"}, trail[1].Fields,
		"the CLI moved prompt; the target_session canonicalization is not its change")
}

// TestAudit_GenuineOnCompleteMoveIsAttributedToTheCaller guards the other half:
// canonical-to-canonical diffing must not swallow a real policy change. A CLI
// patch from keep to kill canonical-differs, so on_complete is in the CLI's
// entry, and no daemon-upgrade line is written.
func TestAudit_GenuineOnCompleteMoveIsAttributedToTheCaller(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	kill := OnCompleteKill
	_, err := UpdateTaskChecked(id, TaskUpdate{OnComplete: &kill}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, id)
	require.Len(t, trail, 2, "the create, and the CLI's move — no daemon-upgrade line for a real policy change")
	assert.Equal(t, ActorCLI, trail[1].Actor)
	assert.Equal(t, []string{"on_complete"}, trail[1].Fields,
		"a keep→kill move is the caller's change, recorded as such")
	for _, e := range trail {
		assert.NotEqual(t, ActorDaemonUpgrade, e.Actor,
			"a genuine policy move is not a store normalization")
	}
}

// TestAudit_GenuineTargetSessionMoveIsAttributedToTheCaller: a real retarget
// (no target → "real") canonical-differs, so target_session is the caller's
// field and no daemon-upgrade line is written.
func TestAudit_GenuineTargetSessionMoveIsAttributedToTheCaller(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	target := "real"
	_, err := UpdateTaskChecked(id, TaskUpdate{TargetSession: &target}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, id)
	require.Len(t, trail, 2, "the create, and the CLI's retarget — no daemon-upgrade line")
	assert.Equal(t, ActorCLI, trail[1].Actor)
	assert.Equal(t, []string{"target_session"}, trail[1].Fields)
	for _, e := range trail {
		assert.NotEqual(t, ActorDaemonUpgrade, e.Actor)
	}
}

// TestAudit_CanonicalRowNoOpAddsNoDaemonUpgradeEntry: on a row that is already
// canonical, the daemon-upgrade condition (canonical-equal AND raw-differ) does
// not fire — there is no byte-change to record. A no-op patch leaves the trail
// at the create, with neither a caller entry nor a daemon-upgrade line.
func TestAudit_CanonicalRowNoOpAddsNoDaemonUpgradeEntry(t *testing.T) {
	id := seedAuditTask(t, ActorCLI)

	// An explicit patch to the same canonical keep the row already stores ("").
	keep := ""
	_, err := UpdateTaskChecked(id, TaskUpdate{OnComplete: &keep}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	// And an empty patch, which apply canonicalizes to exactly the stored bytes.
	_, err = UpdateTaskChecked(id, TaskUpdate{}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, id)
	require.Len(t, trail, 1, "a canonical row with no-op patches gains no entry of any kind")
	assert.Equal(t, AuditCreated, trail[0].Action)
}

// handEditedCronTaskWithCap is handEditedCronTask plus a stale positive cap — a
// shape ValidateTrigger rejects (a cap on a cron task), reachable only via a
// hand-edit of tasks.json. Seeded separately because the canonicalization
// fixtures deliberately keep the two invalid shapes on different rows.
func handEditedCronTaskWithCap(id string, cap int) Task {
	t := handEditedCronTask(id, "", "")
	t.MaxConcurrentRuns = cap
	return t
}

// TestAudit_CapStoreRepairIsAttributedToTheStore is the headline regression for
// the clear-repair misattribution: a hand-edited cron task carrying a stale
// max_concurrent_runs, patched through an empty TaskUpdate{}, has its cap
// repaired to 0 by clearInapplicableCap. The caller never touched the cap, so
// the repair is recorded as ActorDaemonUpgrade — not folded into the caller's
// entry the way a genuine retarget's cap-clear is.
func TestAudit_CapStoreRepairIsAttributedToTheStore(t *testing.T) {
	handEditedStore(t, []Task{handEditedCronTaskWithCap("handcap01", 5)})

	_, err := UpdateTaskChecked("handcap01", TaskUpdate{}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "handcap01")
	require.Len(t, trail, 1, "the empty patch changed only a store-repaired cap")
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor,
		"a store repair of a stale cap on a hand-edited row is the store's change")
	assert.Equal(t, AuditUpdated, trail[0].Action)
	assert.Equal(t, []string{"max_concurrent_runs"}, trail[0].Fields)
	stored, err := GetTask("handcap01")
	require.NoError(t, err)
	assert.Equal(t, 0, stored.MaxConcurrentRuns, "the cap really was repaired on disk")
}

// TestAudit_OnCompleteStoreRepairIsAttributedToTheStore: a hand-edited cron task
// carrying on_complete="kill" AND a target_session (a shape ValidateTrigger
// rejects), patched through an empty TaskUpdate{}, has its on_complete repaired
// to keep by clearInapplicableOnComplete. The caller never touched on_complete,
// so the repair is recorded as ActorDaemonUpgrade, not the caller's move.
func TestAudit_OnCompleteStoreRepairIsAttributedToTheStore(t *testing.T) {
	handEditedStore(t, []Task{handEditedCronTask("handoc01", "kill", "my-sess")})

	_, err := UpdateTaskChecked("handoc01", TaskUpdate{}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "handoc01")
	require.Len(t, trail, 1, "the empty patch changed only a store-repaired on_complete")
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor,
		"a store repair of a contradictory on_complete is the store's change")
	assert.Equal(t, []string{"on_complete"}, trail[0].Fields)
	stored, err := GetTask("handoc01")
	require.NoError(t, err)
	assert.Equal(t, "", stored.OnComplete, "on_complete was repaired to keep on disk")
	assert.Equal(t, "my-sess", stored.TargetSession, "target_session is untouched; only on_complete was repaired")
}

// TestAudit_CapStoreRepairAndCallerChangeCoexist: a hand-edited invalid cron
// with a stale cap, patched with a prompt-only change. The store repairs the
// cap (one daemon-upgrade line), and the caller's entry names ONLY the prompt —
// the carve-out excluded the cap from the caller's diff the same way
// canonical-to-canonical diffing excludes a byte-canonicalization. An operator
// chasing a cap regression is not sent to a CLI edit that never touched it.
func TestAudit_CapStoreRepairAndCallerChangeCoexist(t *testing.T) {
	handEditedStore(t, []Task{handEditedCronTaskWithCap("handcap02", 5)})

	np := "changed"
	_, err := UpdateTaskChecked("handcap02", TaskUpdate{Prompt: &np}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "handcap02")
	require.Len(t, trail, 2,
		"the store's repair and the CLI's move — not one entry attributing both to the CLI")
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor, "the store's repair is recorded first, as repo_id is")
	assert.Equal(t, []string{"max_concurrent_runs"}, trail[0].Fields)
	assert.Equal(t, ActorCLI, trail[1].Actor, "the CLI's entry is the prompt it moved")
	assert.Equal(t, []string{"prompt"}, trail[1].Fields,
		"the cap-clear is the store's repair, not the CLI's change")
}

// TestAudit_OnCompleteStoreRepairAndCallerChangeCoexist: the on_complete twin of
// the cap test above — a prompt-only patch on a row holding a contradictory
// on_complete plus a target session. The store's on_complete repair is a
// daemon-upgrade line; the CLI's entry names only the prompt.
func TestAudit_OnCompleteStoreRepairAndCallerChangeCoexist(t *testing.T) {
	handEditedStore(t, []Task{handEditedCronTask("handoc02", "kill", "my-sess")})

	np := "changed"
	_, err := UpdateTaskChecked("handoc02", TaskUpdate{Prompt: &np}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "handoc02")
	require.Len(t, trail, 2)
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor)
	assert.Equal(t, []string{"on_complete"}, trail[0].Fields)
	assert.Equal(t, ActorCLI, trail[1].Actor)
	assert.Equal(t, []string{"prompt"}, trail[1].Fields)
}

// TestAudit_BothStoreRepairsFireOnOneRow: a single hand-edited row invalid for
// BOTH fields — a cron task with a stale cap AND a target_session-bearing
// on_complete=kill (cap + target_session is itself invalid, and so is kill +
// target_session). An empty patch repairs both; each is its own daemon-upgrade
// line, and no caller entry is written.
func TestAudit_BothStoreRepairsFireOnOneRow(t *testing.T) {
	row := handEditedCronTask("handboth", OnCompleteKill, "my-sess")
	row.MaxConcurrentRuns = 5
	handEditedStore(t, []Task{row})

	_, err := UpdateTaskChecked("handboth", TaskUpdate{}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "handboth")
	require.Len(t, trail, 2, "one daemon-upgrade line per store-repaired field")
	assert.Equal(t, ActorDaemonUpgrade, trail[0].Actor)
	assert.Equal(t, []string{"max_concurrent_runs"}, trail[0].Fields)
	assert.Equal(t, ActorDaemonUpgrade, trail[1].Actor)
	assert.Equal(t, []string{"on_complete"}, trail[1].Fields)
	for _, e := range trail {
		assert.NotEqual(t, ActorCLI, e.Actor, "the empty patch moved nothing on the caller's behalf")
	}
	stored, err := GetTask("handboth")
	require.NoError(t, err)
	assert.Equal(t, 0, stored.MaxConcurrentRuns, "cap repaired")
	assert.Equal(t, "", stored.OnComplete, "on_complete repaired to keep")
	assert.Equal(t, "my-sess", stored.TargetSession, "target_session untouched")
}

// TestAudit_ExplicitCapClearOnInvalidRowIsTheCallersChange guards the
// update.MaxConcurrentRuns == nil gate on the carve-out. When the caller
// EXPLICITLY clears a stale cap (MaxConcurrentRuns: &0) on a hand-edited invalid
// row, the clear is the caller's change, not a store repair: the carve-out stays
// out (the patch set the field), and the trail must NOT drop the write ("cannot
// miss one that did"), so the cap appears in the caller's entry — never as a
// daemon-upgrade line.
func TestAudit_ExplicitCapClearOnInvalidRowIsTheCallersChange(t *testing.T) {
	handEditedStore(t, []Task{handEditedCronTaskWithCap("handcap03", 5)})

	zero := 0
	_, err := UpdateTaskChecked("handcap03", TaskUpdate{MaxConcurrentRuns: &zero}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "handcap03")
	require.Len(t, trail, 1, "the explicit clear is a single caller entry")
	assert.Equal(t, ActorCLI, trail[0].Actor,
		"an explicit cap clear is the caller's change, even on an already-invalid row")
	assert.Equal(t, []string{"max_concurrent_runs"}, trail[0].Fields)
	for _, e := range trail {
		assert.NotEqual(t, ActorDaemonUpgrade, e.Actor, "the store did not repair what the caller cleared")
	}
}

// TestAudit_ExplicitOnCompleteClearOnInvalidRowIsTheCallersChange: the on_complete
// twin — the caller explicitly reverts a contradictory on_complete to keep on a
// hand-edited invalid row. The carve-out stays out (the patch set the field), so
// the move is the caller's, recorded as such, and not dropped or mis-attributed
// to the store.
func TestAudit_ExplicitOnCompleteClearOnInvalidRowIsTheCallersChange(t *testing.T) {
	handEditedStore(t, []Task{handEditedCronTask("handoc03", "kill", "my-sess")})

	keep := OnCompleteKeep
	_, err := UpdateTaskChecked("handoc03", TaskUpdate{OnComplete: &keep}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "handoc03")
	require.Len(t, trail, 1, "the explicit revert is a single caller entry")
	assert.Equal(t, ActorCLI, trail[0].Actor)
	assert.Equal(t, []string{"on_complete"}, trail[0].Fields)
	for _, e := range trail {
		assert.NotEqual(t, ActorDaemonUpgrade, e.Actor)
	}
}

// TestAudit_CapStoreRepairFiresAtMostOnce guards the "fires at most once" property
// the comment on the canonicalization carves claims: the triggering write
// repairs the on-disk value, so a second empty patch on the now-valid row has
// nothing left to repair and writes no daemon-upgrade line.
func TestAudit_CapStoreRepairFiresAtMostOnce(t *testing.T) {
	handEditedStore(t, []Task{handEditedCronTaskWithCap("handcap04", 5)})

	_, err := UpdateTaskChecked("handcap04", TaskUpdate{}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	require.Len(t, auditOf(t, "handcap04"), 1, "first write repaired and recorded")

	_, err = UpdateTaskChecked("handcap04", TaskUpdate{}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	assert.Len(t, auditOf(t, "handcap04"), 1, "a now-valid row has nothing left to repair")
}

// TestAudit_GenuineRetargetCapClearStaysAttributedToTheCaller is the case the
// fix must NOT regress: a VALID watch task the caller retargets to cron has its
// cap cleared as a consequence of the caller's own trigger change. The existing
// row had capApplies()==true, so the carve-out does not fire, and the cap-clear
// stays in the caller's entry alongside the trigger fields it moved.
func TestAudit_GenuineRetargetCapClearStaysAttributedToTheCaller(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seed := Task{ID: "retarget01", Name: "capped", WatchCmd: "tail -f x", MaxConcurrentRuns: 3, Enabled: true}
	require.NoError(t, AddTask(seed))

	edited := seed
	edited.CronExpr = "0 9 * * *"
	edited.WatchCmd = ""
	edited.Prompt = "run it"
	patch := DiffTask(seed, edited)
	require.Nil(t, patch.MaxConcurrentRuns, "a TUI/API edit never patches the cap")

	_, err := UpdateTaskChecked("retarget01", patch, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	trail := auditOf(t, "retarget01")
	require.Len(t, trail, 2, "the create, and the CLI's retarget — no daemon-upgrade line")
	assert.Equal(t, ActorCLI, trail[1].Actor)
	assert.Equal(t, []string{"prompt", "cron_expr", "watch_cmd", "max_concurrent_runs"}, trail[1].Fields,
		"the cap-clear is a consequence of the caller's retarget and stays attributed to the caller")
	for _, e := range trail {
		assert.NotEqual(t, ActorDaemonUpgrade, e.Actor, "a genuine retarget is not a store repair")
	}
}
