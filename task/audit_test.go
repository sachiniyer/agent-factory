package task

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"

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

// These pin the destructive inverse of TestAudit_SamePathBackfillIsRecorded: a
// same-path ProjectPath reassertion over a path that has SINCE stopped resolving
// must not erase the retained RepoID. The recompute stays — it backs the legacy
// backfill where the retained id is "" and the path resolves — but an empty
// re-resolution over a retained, non-empty id strands the task from its own
// project's scope, the exact harm Task.RepoID exists to prevent. See
// Task.RepoID's PURPOSE and the contract contemplation of subdirectory and
// linked-worktree bindings at task/task.go:103-108.

// bindMainWithLinkedWorktree creates a main git repo and a linked worktree at a
// SIBLING path, the contract-contemplated binding whose owning repo root is not
// a directory-tree ancestor of the recorded path. Returns the symlink-resolved
// main root and worktree path. A commit is made so `git worktree add` can branch.
func bindMainWithLinkedWorktree(t *testing.T) (mainRoot, worktree string) {
	t.Helper()
	base := t.TempDir()
	mainRoot = filepath.Join(base, "main")
	require.NoError(t, os.MkdirAll(mainRoot, 0o755))
	require.NoError(t, exec.Command("git", "init", mainRoot).Run())
	for _, args := range [][]string{
		{"-C", mainRoot, "config", "user.email", "test@example.com"},
		{"-C", mainRoot, "config", "user.name", "Test User"},
		{"-C", mainRoot, "commit", "--allow-empty", "-m", "init"},
	} {
		require.NoError(t, exec.Command("git", args...).Run(), "git %v", args)
	}
	wtRaw := filepath.Join(base, "linked")
	out, err := exec.Command("git", "-C", mainRoot, "worktree", "add", "-b", "feature", wtRaw).CombinedOutput()
	require.NoError(t, err, "git worktree add: %s", out)
	worktree, err = filepath.EvalSymlinks(wtRaw)
	require.NoError(t, err)
	mainRoot, err = filepath.EvalSymlinks(mainRoot)
	require.NoError(t, err)
	return mainRoot, worktree
}

// bindMainWithSubdir creates a main git repo and a subdirectory inside it, the
// other contract-contemplated binding: the TUI records the subdirectory the user
// typed. Returns the symlink-resolved main root and the subdirectory path.
func bindMainWithSubdir(t *testing.T) (mainRoot, sub string) {
	t.Helper()
	base := t.TempDir()
	mainRoot = filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(mainRoot, 0o755))
	require.NoError(t, exec.Command("git", "init", mainRoot).Run())
	sub = filepath.Join(mainRoot, "services", "dlq")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	var err error
	mainRoot, err = filepath.EvalSymlinks(mainRoot)
	require.NoError(t, err)
	sub, err = filepath.EvalSymlinks(sub)
	require.NoError(t, err)
	return mainRoot, sub
}

// TestAudit_SamePathDeadPatchRetainsRepoID is the simplest shape: a plain `git
// init` whose recorded path IS its own root. This shape does NOT surface a scope
// divergence (the retained id and the dead-path fallback hash the same cleaned
// path), so this test pins only that the field is NOT erased and that NO
// daemon-upgrade audit entry fires for the destructive direction — the inverse
// of TestAudit_SamePathBackfillIsRecorded, where a same-path patch FILLS a
// retained id and the daemon-upgrade entry correctly fires.
func TestAudit_SamePathDeadPatchRetainsRepoID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, exec.Command("git", "init", repo).Run())
	resolved, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	repo = resolved

	created, err := AddTaskChecked(Task{
		ID: "own00001", Name: "Bound", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: repo, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, created.RepoID, "precondition: bind-time resolution stamped a RepoID")
	retained := created.RepoID

	require.NoError(t, os.RemoveAll(filepath.Join(repo, ".git")))
	require.Empty(t, repoIDForPath(repo), "precondition: the path no longer resolves to a repo")

	same := repo
	_, err = UpdateTaskChecked("own00001", TaskUpdate{ProjectPath: &same}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("own00001")
	require.NoError(t, err)
	assert.Equal(t, retained, stored.RepoID, "a dead-path same-path re-patch must not erase the retained RepoID")
	assert.Len(t, stored.Audit, 1, "no daemon-upgrade entry fires: the destructive clear is neither performed nor recorded")
}

// TestAudit_SamePathDeadPatchRetainsWorktreeBinding: the sibling-worktree shape
// the Task.RepoID contract names directly. The recorded path is a worktree at a
// sibling of the main repo, so once its .git dies an ancestor walk from the
// recorded path never crosses the lateral main repo and re-derivation invents an
// id that matches nothing. Erasing RepoID on a same-path re-patch strandts the
// task from its own project's scoped list; the fix retains the id.
func TestAudit_SamePathDeadPatchRetainsWorktreeBinding(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	mainRoot, worktree := bindMainWithLinkedWorktree(t)

	created, err := AddTaskChecked(Task{
		ID: "wtd00001", Name: "worktree-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: worktree, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, created.RepoID, "precondition: bind-time resolution stamped a RepoID")
	retained := created.RepoID

	vis, err := LoadTasksForRepo(mainRoot)
	require.NoError(t, err)
	require.Len(t, vis, 1, "precondition: the worktree task is visible in its project's scoped list")

	// Kill the worktree's .git: the recorded path no longer resolves, and the
	// lateral main repo is unreachable by the ancestor walk (the stranding shape).
	require.NoError(t, os.Remove(filepath.Join(worktree, ".git")))
	require.Empty(t, repoIDForPath(worktree), "precondition: the worktree path no longer resolves to a repo")
	dead := config.ResolveProjectPath(worktree)
	require.Empty(t, dead.Root, "precondition: no surviving ancestor — the loader's known-gate cannot restore")
	require.NotEqual(t, retained, dead.ID, "precondition: re-derivation diverges from the retained id (the stranding condition)")

	same := worktree
	_, err = UpdateTaskChecked("wtd00001", TaskUpdate{ProjectPath: &same}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("wtd00001")
	require.NoError(t, err)
	assert.Equal(t, retained, stored.RepoID, "a dead-path same-path re-patch must not erase the retained RepoID")
	assert.Len(t, stored.Audit, 1, "no daemon-upgrade entry fires for the un-resolving re-bind")

	// The retained id short-circuits scope matching before the dead path is
	// re-derived, so the task stays in its own project's scoped list.
	vis, err = LoadTasksForRepo(mainRoot)
	require.NoError(t, err)
	require.Len(t, vis, 1, "the retained binding keeps the task visible in its own project after a dead-path re-patch")
}

// TestAudit_SamePathDeadPatchRetainsRepoIDForEquivalentSpelling: the daemon does
// not normalize project_path, so a remote caller can reassert the SAME dead path
// with an equivalent spelling (a trailing separator or a "."/".." leaf) whose raw
// string differs from the recorded one. Before the cleaned-path compare that raw
// inequality read as a rebind and the empty re-resolution erased the retained
// RepoID, stranding the worktree task from its own project's scoped list. The fix
// recognizes the equivalent spellings as same-path and keeps the binding. Pairs
// with TestAudit_SamePathDeadPatchRetainsWorktreeBinding, the exact-spelling twin.
func TestAudit_SamePathDeadPatchRetainsRepoIDForEquivalentSpelling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	mainRoot, worktree := bindMainWithLinkedWorktree(t)

	created, err := AddTaskChecked(Task{
		ID: "wte00001", Name: "worktree-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: worktree, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, created.RepoID, "precondition: bind-time resolution stamped a RepoID")
	retained := created.RepoID

	vis, err := LoadTasksForRepo(mainRoot)
	require.NoError(t, err)
	require.Len(t, vis, 1, "precondition: the worktree task is visible in its project's scoped list")

	// Kill the worktree's .git: the recorded path no longer resolves, and the
	// lateral main repo is unreachable by the ancestor walk (the stranding shape).
	require.NoError(t, os.Remove(filepath.Join(worktree, ".git")))
	require.Empty(t, repoIDForPath(worktree), "precondition: the worktree path no longer resolves to a repo")
	dead := config.ResolveProjectPath(worktree)
	require.Empty(t, dead.Root, "precondition: no surviving ancestor — the loader's known-gate cannot restore")
	require.NotEqual(t, retained, dead.ID, "precondition: re-derivation diverges from the retained id (the stranding condition)")

	// Built as raw strings rather than filepath.Join, which would Clean away the
	// very difference this test exercises. Each differs from worktree as a raw
	// string yet cleans back to it, so each is an equivalent dead-path reassertion.
	sep := string(filepath.Separator)
	spellings := []string{
		worktree + sep,       // trailing separator
		worktree + sep + ".", // trailing "." leaf
		worktree + sep + ".." + sep + filepath.Base(worktree), // ".." then back to the leaf
	}
	for _, spelling := range spellings {
		require.NotEqual(t, worktree, spelling, "precondition: the spelling differs as a raw string: %q", spelling)
		require.Equal(t, filepath.Clean(worktree), filepath.Clean(spelling), "precondition: but is equivalent once cleaned: %q", spelling)
		require.Empty(t, repoIDForPath(spelling), "precondition: the equivalent spelling is also a dead path: %q", spelling)

		_, err := UpdateTaskChecked("wte00001", TaskUpdate{ProjectPath: &spelling}, ProjectExpectation{}, ActorCLI, nil)
		require.NoError(t, err)

		stored, err := GetTask("wte00001")
		require.NoError(t, err)
		assert.Equal(t, retained, stored.RepoID, "an equivalent spelling of a dead path must not erase the retained RepoID: %q", spelling)
		for _, e := range stored.Audit {
			assert.NotEqual(t, ActorDaemonUpgrade, e.Actor, "no daemon-upgrade entry fires for an equivalent dead-path reassertion: %q", spelling)
		}

		vis, err := LoadTasksForRepo(mainRoot)
		require.NoError(t, err)
		require.Len(t, vis, 1, "the retained binding keeps the task visible in its own project across an equivalent dead-path reassertion: %q", spelling)
	}
}

// TestAudit_SamePathDeadPatchRetainsWorktreeBinding_DurableThroughLoaderPass:
// the daemon's startup re-binding pass must not disturb the retained binding
// either. It backfills only rows whose ProjectPath still resolves (Root != "");
// the dead worktree path does not, so the loader skips the row and the retained
// id stays authoritative on disk.
func TestAudit_SamePathDeadPatchRetainsWorktreeBinding_DurableThroughLoaderPass(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	mainRoot, worktree := bindMainWithLinkedWorktree(t)

	created, err := AddTaskChecked(Task{
		ID: "wtd00003", Name: "worktree-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: worktree, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	retained := created.RepoID

	require.NoError(t, os.Remove(filepath.Join(worktree, ".git")))
	same := worktree
	_, err = UpdateTaskChecked("wtd00003", TaskUpdate{ProjectPath: &same}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	// The daemon's loader pass: commits backfills for legacy rows whose path
	// resolves and returns the authoritative list plus what it rewrote.
	loaded, updated, err := LoadTasksForRepoIDWithBindingUpdates(retained)
	require.NoError(t, err)
	assert.Empty(t, updated, "the loader rewrites nothing: the retained id is already non-empty")
	require.Len(t, loaded, 1, "the retained id keeps the task in its project's authoritative list")
	assert.Equal(t, retained, loaded[0].RepoID)

	stored, err := GetTask("wtd00003")
	require.NoError(t, err)
	assert.Equal(t, retained, stored.RepoID, "the retained id survives the loader pass on disk")
	assert.Len(t, stored.Audit, 1, "the loader adds no daemon-upgrade entry for an already-bound row")

	vis, err := LoadTasksForRepo(mainRoot)
	require.NoError(t, err)
	require.Len(t, vis, 1, "the task remains visible in its project's scoped list after the loader pass")
}

// TestAudit_SamePathDeadPatchRetainsSubdirOfDeadParentBinding: the subdirectory
// shape. The TUI records the subdirectory the user typed; git resolves it back
// to the main repo while it lives. Once the repo's .git dies the ancestor walk
// from the subdir finds nothing, so re-derivation invents sha256(subdir), which
// differs from the retained sha256(repo) — erasing RepoID strands the task.
func TestAudit_SamePathDeadPatchRetainsSubdirOfDeadParentBinding(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	mainRoot, sub := bindMainWithSubdir(t)

	created, err := AddTaskChecked(Task{
		ID: "sub00001", Name: "subdir-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: sub, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, created.RepoID)
	retained := created.RepoID

	vis, err := LoadTasksForRepo(mainRoot)
	require.NoError(t, err)
	require.Len(t, vis, 1, "precondition: the subdir task is visible in its project while the repo lives")

	require.NoError(t, os.RemoveAll(filepath.Join(mainRoot, ".git")))
	require.Empty(t, repoIDForPath(sub), "precondition: the dead subdir no longer resolves to a repo")
	dead := config.ResolveProjectPath(sub)
	require.Empty(t, dead.Root, "precondition: no surviving ancestor — the loader's known-gate cannot restore")
	require.NotEqual(t, retained, dead.ID, "precondition: re-derivation diverges from the retained id (the stranding condition)")

	same := sub
	_, err = UpdateTaskChecked("sub00001", TaskUpdate{ProjectPath: &same}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("sub00001")
	require.NoError(t, err)
	assert.Equal(t, retained, stored.RepoID, "a dead-path same-path re-patch must not erase the retained RepoID")
	assert.Len(t, stored.Audit, 1, "no daemon-upgrade entry fires for the un-resolving re-bind")

	vis, err = LoadTasksForRepo(mainRoot)
	require.NoError(t, err)
	require.Len(t, vis, 1, "the retained binding keeps the task visible in its project after the owning repo dies")
}

// TestAudit_SamePathDeadPatchRetainsRepoIDWithUnchangedField: the realistic
// trigger — an operator re-applies task config that includes --project-path
// while editing another field, without knowing the path is already dead on the
// daemon host. The other field's change is recorded as the user's; RepoID is
// retained and no daemon-upgrade repo_id entry fires.
func TestAudit_SamePathDeadPatchRetainsRepoIDWithUnchangedField(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	mainRoot, worktree := bindMainWithLinkedWorktree(t)

	created, err := AddTaskChecked(Task{
		ID: "wtd00004", Name: "worktree-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: worktree, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	retained := created.RepoID

	require.NoError(t, os.Remove(filepath.Join(worktree, ".git")))

	same := worktree
	prompt := "sweep harder"
	_, err = UpdateTaskChecked("wtd00004", TaskUpdate{ProjectPath: &same, Prompt: &prompt}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("wtd00004")
	require.NoError(t, err)
	assert.Equal(t, retained, stored.RepoID, "the retained RepoID survives a same-path dead re-patch that also edits a field")
	require.Len(t, stored.Audit, 2, "the create, and the user's prompt change — no daemon-upgrade repo_id entry")
	assert.Equal(t, ActorCLI, stored.Audit[1].Actor)
	assert.Equal(t, []string{"prompt"}, stored.Audit[1].Fields, "the trail records the user's prompt change, not a derived repo_id")

	vis, err := LoadTasksForRepo(mainRoot)
	require.NoError(t, err)
	require.Len(t, vis, 1, "the task stays visible in its project after the combined re-patch")
}

// TestAudit_SymlinkDivergentRebindIsDetectedNotRetained pins the filesystem
// semantics sameProjectPathReassertion adds: two spellings that clean lexically
// equal but resolve through a symlink to DIFFERENT directories are a real
// rebind, not the same-path reassertion the dead-path protection retains. This
// is the inverse of TestAudit_SamePathDeadPatchRetainsRepoIDForEquivalentSpelling,
// whose equivalent spellings all resolve to the SAME directory and so retain.
//
// base/link -> other/child (a symlink), so base/link/../task resolves physically
// to other/task while it cleans lexically to base/task; base/task is a different,
// non-repo directory. Rebinding base/link/../task -> base/task with the new
// target non-Git must clear the retained RepoID rather than preserve it and
// leave the task scoped to its former project.
func TestAudit_SymlinkDivergentRebindIsDetectedNotRetained(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	// other is a git repo; the recorded path resolves into it through the link.
	other := filepath.Join(base, "other")
	require.NoError(t, os.MkdirAll(filepath.Join(other, "child"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(other, "task"), 0o755))
	require.NoError(t, exec.Command("git", "init", other).Run())
	// base/task is a plain non-Git dir — the rebind target.
	require.NoError(t, os.MkdirAll(filepath.Join(base, "task"), 0o755))
	// base/link -> other/child, so base/link/../task resolves physically to
	// other/task, not to base/task.
	require.NoError(t, os.Symlink(filepath.Join(other, "child"), filepath.Join(base, "link")))

	// Built as raw strings rather than via filepath.Join, which would Clean
	// away the "link/.." that this test exercises.
	sep := string(filepath.Separator)
	recorded := base + sep + "link" + sep + ".." + sep + "task"
	rebind := filepath.Join(base, "task")

	// Precondition: the two spellings are lexically equal but physically
	// distinct — the exact pair a cleaned-path compare misreads as same-path.
	require.Equal(t, filepath.Clean(recorded), filepath.Clean(rebind))
	resolvedRecorded, err := filepath.EvalSymlinks(recorded)
	require.NoError(t, err)
	resolvedRebind, err := filepath.EvalSymlinks(rebind)
	require.NoError(t, err)
	require.NotEqual(t, resolvedRecorded, resolvedRebind, "the two paths are physically distinct")

	created, err := AddTaskChecked(Task{
		ID: "sym00001", Name: "symlink-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: recorded, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, created.RepoID, "bind-time resolution stamped a RepoID from the repo behind the link")

	// The rebind target is non-Git, so its re-resolution is empty.
	require.Empty(t, repoIDForPath(rebind), "precondition: the rebind target is not a repo")

	_, err = UpdateTaskChecked("sym00001", TaskUpdate{ProjectPath: &rebind}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("sym00001")
	require.NoError(t, err)
	assert.Empty(t, stored.RepoID, "a symlink-divergent rebind to a non-Git path is a real rebind, not a same-path reassertion: the binding clears")
}

// TestAudit_SymlinkDivergentRebindAgainstDeadRecordedPath is the dead-recorded
// twin of TestAudit_SymlinkDivergentRebindIsDetectedNotRetained. There the two
// spellings both resolved, so EvalSymlinks caught the divergence on the physical
// branch; here the RECORDED path no longer exists (EvalSymlinks fails on it),
// driving sameProjectPathReassertion into the lexical Clean fallback — the path
// the ".." restriction now guards.
//
// base/link -> other/child, so base/link/../task resolves physically to
// other/task (a non-Git directory) while it cleans lexically to base/task. The
// recorded base/task is removed entirely, so EvalSymlinks cannot answer for it
// and the fallback must decide. A ".." segment can cross a symlink Clean cannot
// see, so the patch is read as a real rebind and re-resolved (to empty, the
// non-Git target) rather than retained as a same-path reassertion — the inverse
// of the dead-path retain cases, whose spellings resolve and so never reach
// this branch.
func TestAudit_SymlinkDivergentRebindAgainstDeadRecordedPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	// other/task is the non-Git directory the patch resolves to through the
	// link; other/child exists so the symlink target is reachable.
	other := filepath.Join(base, "other")
	require.NoError(t, os.MkdirAll(filepath.Join(other, "child"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(other, "task"), 0o755))
	// base/link -> other/child, so base/link/../task resolves physically to
	// other/task, not to base/task.
	require.NoError(t, os.Symlink(filepath.Join(other, "child"), filepath.Join(base, "link")))

	// base/task starts as its own git repo so the task binds to it and retains a
	// RepoID; it is then removed entirely so the recorded path no longer exists
	// and EvalSymlinks fails on it — the dead-recorded-path shape this test
	// exists for.
	recorded := filepath.Join(base, "task")
	require.NoError(t, os.MkdirAll(recorded, 0o755))
	require.NoError(t, exec.Command("git", "init", recorded).Run())

	created, err := AddTaskChecked(Task{
		ID: "sym00002", Name: "symlink-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: recorded, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, created.RepoID, "precondition: bind-time resolution stamped a RepoID from base/task's own repo")
	retained := created.RepoID

	require.NoError(t, os.RemoveAll(recorded))
	_, err = filepath.EvalSymlinks(recorded)
	require.Error(t, err, "precondition: the recorded path is gone, so the physical comparison is unavailable")
	require.Empty(t, repoIDForPath(recorded), "precondition: the removed path no longer resolves to a repo")

	// Built as a raw string rather than via filepath.Join, which would Clean
	// away the "link/.." that this test exercises.
	sep := string(filepath.Separator)
	patch := base + sep + "link" + sep + ".." + sep + "task"
	require.Equal(t, filepath.Clean(patch), filepath.Clean(recorded), "precondition: the two spellings clean lexically equal")
	resolvedPatch, err := filepath.EvalSymlinks(patch)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(other, "task"), resolvedPatch, "precondition: but the patch resolves physically to other/task")
	require.Empty(t, repoIDForPath(patch), "precondition: the patch resolves to a non-Git directory, so the re-resolution is empty")

	_, err = UpdateTaskChecked("sym00002", TaskUpdate{ProjectPath: &patch}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("sym00002")
	require.NoError(t, err)
	assert.Empty(t, stored.RepoID, "a symlink-divergent rebind against a dead recorded path is a real rebind, not a same-path reassertion: the binding re-resolves (to empty) rather than retain")
	assert.NotEqual(t, retained, stored.RepoID, "the stale binding for the removed base/task is not retained across a \"..\"-divergent rebind")
}

// TestAudit_SymlinkDivergentRebindWithDotDotInDeadRecordedPath is the inverse of
// TestAudit_SymlinkDivergentRebindAgainstDeadRecordedPath: there the ".." lived
// in the (live) patch and the recorded path was removed; here the RECORDED path
// carries the ".." and is the one that dies, while the patch is a plain non-Git
// directory with no "..". The recorded base/link/../task binds through the link
// to other/task (inside other's repo) and retains other's id; once other/task is
// removed EvalSymlinks cannot answer for the recorded spelling, so the decision
// falls to the lexical Clean compare. Both spellings clean to base/task, so
// before the guard covered the recorded spelling's ".." the cleaned compare
// read a real rebind as a same-path reassertion and the retained id survived —
// leaving the task scoped to a repository it no longer binds to. The guard now
// checks both spellings, so the recorded ".." drives a rebind and the (non-Git)
// patch re-resolves to empty, clearing the binding.
func TestAudit_SymlinkDivergentRebindWithDotDotInDeadRecordedPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	// other is a git repo; the recorded path resolves into it through the link.
	other := filepath.Join(base, "other")
	require.NoError(t, os.MkdirAll(filepath.Join(other, "child"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(other, "task"), 0o755))
	require.NoError(t, exec.Command("git", "init", other).Run())
	// base/task is a plain non-Git dir — the patch (rebind target).
	require.NoError(t, os.MkdirAll(filepath.Join(base, "task"), 0o755))
	// base/link -> other/child, so base/link/../task resolves physically to
	// other/task, not to base/task.
	require.NoError(t, os.Symlink(filepath.Join(other, "child"), filepath.Join(base, "link")))

	// Built as a raw string rather than via filepath.Join, which would Clean
	// away the "link/.." that this test exercises.
	sep := string(filepath.Separator)
	recorded := base + sep + "link" + sep + ".." + sep + "task"
	patch := filepath.Join(base, "task")

	created, err := AddTaskChecked(Task{
		ID: "sym00003", Name: "symlink-task", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: recorded, Program: "claude", Enabled: false, CreatedAt: time.Now(),
	}, ActorCLI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, created.RepoID, "bind-time resolution stamped a RepoID from the repo behind the link")
	retained := created.RepoID

	// Kill the recorded path's resolution: other/task is removed, so
	// EvalSymlinks(recorded) can no longer answer, and re-derivation from the raw
	// recorded string also finds no repo.
	require.NoError(t, os.RemoveAll(filepath.Join(other, "task")))
	_, err = filepath.EvalSymlinks(recorded)
	require.Error(t, err, "precondition: the recorded path is gone, so the physical comparison is unavailable")
	require.Empty(t, repoIDForPath(recorded), "precondition: the dead recorded path no longer resolves to a repo")

	// The patch is a live non-Git directory with no "..": before the guard
	// covered the recorded spelling this was the pair that cleaned equal and
	// retained a stale id.
	require.Equal(t, filepath.Clean(recorded), filepath.Clean(patch), "precondition: the two spellings clean lexically equal")
	resolvedPatch, err := filepath.EvalSymlinks(patch)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "task"), resolvedPatch, "precondition: the patch resolves physically to base/task, a live non-Git dir")
	require.Empty(t, repoIDForPath(patch), "precondition: the patch resolves to a non-Git directory, so the re-resolution is empty")

	_, err = UpdateTaskChecked("sym00003", TaskUpdate{ProjectPath: &patch}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)

	stored, err := GetTask("sym00003")
	require.NoError(t, err)
	assert.Empty(t, stored.RepoID, "a symlink-divergent rebind whose \"..\" is in the dead recorded path is a real rebind, not a same-path reassertion: the binding re-resolves (to empty) rather than retain")
	assert.NotEqual(t, retained, stored.RepoID, "the stale binding for the removed other/task is not retained across a \"..\"-divergent rebind whose \"..\" is in the recorded path")
}
