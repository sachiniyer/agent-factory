package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCanonicalOnComplete pins the normalization every surface relies on: empty
// and "keep" are the SAME policy, so nothing downstream has to special-case a
// legacy row, and case/whitespace from a CLI flag never reaches storage.
func TestCanonicalOnComplete(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", OnCompleteKeep},
		{"   ", OnCompleteKeep},
		{"keep", OnCompleteKeep},
		{"archive", OnCompleteArchive},
		{"  ARCHIVE ", OnCompleteArchive},
		{"Kill", OnCompleteKill},
		{"nope", "nope"}, // preserved verbatim so validation can name it back
	} {
		assert.Equal(t, tc.want, CanonicalOnComplete(tc.in), "CanonicalOnComplete(%q)", tc.in)
	}
}

// TestValidateTriggerOnComplete is the admission half of #2595.
func TestValidateTriggerOnComplete(t *testing.T) {
	cases := []struct {
		name    string
		task    Task
		wantErr string
	}{
		{
			name: "unset is the default and always valid",
			task: Task{ID: "t1", CronExpr: "0 3 * * *", Prompt: "p", Enabled: true},
		},
		{
			name: "explicit keep is valid",
			task: Task{ID: "t2", CronExpr: "0 3 * * *", Prompt: "p", Enabled: true, OnComplete: "keep"},
		},
		{
			name: "archive on a cron task is the headline case",
			task: Task{ID: "t3", CronExpr: "0 3 * * *", Prompt: "p", Enabled: true, OnComplete: "archive"},
		},
		{
			name: "kill on a cron task is valid",
			task: Task{ID: "t4", CronExpr: "0 3 * * *", Prompt: "p", Enabled: true, OnComplete: "kill"},
		},
		{
			name: "a watch task that spawns per event may declare one",
			task: Task{ID: "t5", WatchCmd: "tail -f log", Enabled: true, OnComplete: "kill"},
		},
		{
			// The session a target task names is the thing it exists to REUSE;
			// reaping it after each run destroys exactly that.
			name:    "archive with a target session is refused",
			task:    Task{ID: "t6", WatchCmd: "tail -f log", TargetSession: "shared", Enabled: true, OnComplete: "archive"},
			wantErr: "target_session",
		},
		{
			name:    "kill with a target session is refused",
			task:    Task{ID: "t7", CronExpr: "0 3 * * *", Prompt: "p", TargetSession: "shared", Enabled: true, OnComplete: "kill"},
			wantErr: "target_session",
		},
		{
			// keep IS the target task's behavior, so it must not be refused —
			// otherwise reverting a task to the default would fail.
			name: "keep with a target session is fine",
			task: Task{ID: "t8", CronExpr: "0 3 * * *", Prompt: "p", TargetSession: "shared", Enabled: true, OnComplete: "keep"},
		},
		{
			name:    "an unknown verb is refused and named back",
			task:    Task{ID: "t9", CronExpr: "0 3 * * *", Prompt: "p", Enabled: true, OnComplete: "delete"},
			wantErr: `unknown on_complete "delete"`,
		},
		{
			name: "a disabled draft may still carry a verb",
			task: Task{ID: "t10", CronExpr: "0 3 * * *", OnComplete: "archive"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.task.ValidateTrigger()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestOnCompleteUnknownVerbNamesTheAcceptedOnes: a rejection that does not say
// what IS accepted makes the user guess, and there are exactly three answers.
func TestOnCompleteUnknownVerbNamesTheAcceptedOnes(t *testing.T) {
	err := Task{ID: "t", CronExpr: "0 3 * * *", Prompt: "p", Enabled: true, OnComplete: "purge"}.ValidateTrigger()
	require.Error(t, err)
	for _, verb := range OnCompleteValues() {
		assert.Contains(t, err.Error(), verb, "the refusal must offer %q", verb)
	}
}

// TestSessionLifecycleReadsThroughTheDefault: callers ask the task, not the raw
// field, so a legacy row and an explicit keep are indistinguishable to them.
func TestSessionLifecycleReadsThroughTheDefault(t *testing.T) {
	assert.Equal(t, OnCompleteKeep, Task{}.SessionLifecycle())
	assert.Equal(t, OnCompleteKeep, Task{OnComplete: "  "}.SessionLifecycle())
	assert.Equal(t, OnCompleteArchive, Task{OnComplete: "Archive"}.SessionLifecycle())
}

// TestAddTaskStoresKeepAsAbsent is the compatibility guarantee that makes this
// field safe to ship: a task that does not opt in must serialize EXACTLY as it
// did before the field existed, so an older af reading the same tasks.json sees
// no new key and a diff of the store shows nothing.
func TestAddTaskStoresKeepAsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	for i, in := range []string{"", "keep", "  KEEP  "} {
		t.Run("stored as absent: "+strconv.Quote(in), func(t *testing.T) {
			created, err := AddTaskChecked(Task{
				ID:          fmt.Sprintf("id%d", i),
				CronExpr:    "0 3 * * *",
				Prompt:      "p",
				ProjectPath: dir,
				Program:     "claude",
				Enabled:     false,
				OnComplete:  in,
			}, nil)
			require.NoError(t, err)
			assert.Empty(t, created.OnComplete, "keep must be stored as the zero value")

			raw, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "on_complete",
				"a task that did not opt in must not gain a new key on disk")
		})
	}
}

// TestAddTaskStoresAnExplicitVerbCanonically: an opted-in task DOES carry the
// key, lowercased and trimmed, so the daemon never has to normalize at read time.
func TestAddTaskStoresAnExplicitVerbCanonically(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	created, err := AddTaskChecked(Task{
		ID:          "abc",
		CronExpr:    "0 3 * * *",
		Prompt:      "p",
		ProjectPath: dir,
		Program:     "claude",
		Enabled:     false,
		OnComplete:  "  Archive ",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, OnCompleteArchive, created.OnComplete)

	stored, err := LoadTasks()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, OnCompleteArchive, stored[0].OnComplete,
		"the canonical verb must be what a reader gets back")
}

// TestUpdateTaskPatchesOnComplete covers the round trip that matters for a
// pointer patch field: setting a verb, and reverting to the default. "keep" is a
// real value to patch back to, which is why TaskUpdate.OnComplete is a *string —
// a plain string could not tell "revert" from "leave alone".
func TestUpdateTaskPatchesOnComplete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	err := AddTask(Task{
		ID: "abc", CronExpr: "0 3 * * *", Prompt: "p",
		ProjectPath: dir, Program: "claude", Enabled: false,
	})
	require.NoError(t, err)

	set := OnCompleteKill
	updated, err := UpdateTask("abc", TaskUpdate{OnComplete: &set}, ProjectExpectation{})
	require.NoError(t, err)
	assert.Equal(t, OnCompleteKill, updated.OnComplete)

	// An unrelated patch must not disturb it.
	name := "renamed"
	updated, err = UpdateTask("abc", TaskUpdate{Name: &name}, ProjectExpectation{})
	require.NoError(t, err)
	assert.Equal(t, OnCompleteKill, updated.OnComplete, "an unrelated patch must not clear the verb")

	revert := OnCompleteKeep
	updated, err = UpdateTask("abc", TaskUpdate{OnComplete: &revert}, ProjectExpectation{})
	require.NoError(t, err)
	assert.Empty(t, updated.OnComplete, "reverting to keep must return the row to its default shape")
}

// TestUpdateTaskRejectsAnUnknownVerb: the patch path runs the same validation as
// the create path, so a bad value cannot be introduced by an update either.
func TestUpdateTaskRejectsAnUnknownVerb(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	err := AddTask(Task{
		ID: "abc", CronExpr: "0 3 * * *", Prompt: "p",
		ProjectPath: dir, Program: "claude", Enabled: true,
	})
	require.NoError(t, err)

	bad := "shred"
	_, err = UpdateTask("abc", TaskUpdate{OnComplete: &bad}, ProjectExpectation{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown on_complete")
}

// TestTaskUpdateIsEmptyCountsOnComplete: a patch carrying ONLY this field must
// not be mistaken for a no-op, or `af tasks update --on-complete kill` would
// validate and write nothing.
func TestTaskUpdateIsEmptyCountsOnComplete(t *testing.T) {
	assert.True(t, TaskUpdate{}.IsEmpty())
	verb := OnCompleteArchive
	assert.False(t, TaskUpdate{OnComplete: &verb}.IsEmpty())
}

// TestTaskUpdateOnCompleteSurvivesGob is the #1700 trap: the control socket
// gob-encodes the patch, and gob elides a pointer to a zero value — so a
// *string pointing at "" would decode back as nil and silently drop the patch.
// TaskUpdate round-trips through JSON to keep the nil-vs-set distinction, and
// "keep" canonicalizes from "" so this is exactly the reverting patch.
func TestTaskUpdateOnCompleteSurvivesGob(t *testing.T) {
	empty := ""
	encoded, err := TaskUpdate{OnComplete: &empty}.GobEncode()
	require.NoError(t, err)

	var decoded TaskUpdate
	require.NoError(t, decoded.GobDecode(encoded))
	require.NotNil(t, decoded.OnComplete, "a patch reverting to the default must survive the control socket")
	assert.Equal(t, "", *decoded.OnComplete)
}

// TestUpdateTaskAddingATargetSessionDropsAnInapplicableVerb is the retargeting
// case. A per-run task that declares on_complete would otherwise merge with a
// newly-added target_session into a record ValidateTrigger rejects, so ordinary
// retargeting would fail from every surface that does not expose the field — and
// force a CLI user to know to pass --on-complete keep alongside.
//
// It mirrors the max_concurrent_runs rule exactly: an unpatched, now-inapplicable
// field is dropped by the shared merge.
func TestUpdateTaskAddingATargetSessionDropsAnInapplicableVerb(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	require.NoError(t, AddTask(Task{
		ID: "abc", CronExpr: "0 3 * * *", Prompt: "p",
		ProjectPath: dir, Program: "claude", Enabled: true, OnComplete: OnCompleteArchive,
	}))

	target := "shared"
	updated, err := UpdateTask("abc", TaskUpdate{TargetSession: &target}, ProjectExpectation{})
	require.NoError(t, err, "retargeting a task must not be blocked by a verb the new shape cannot carry")
	assert.Equal(t, "shared", updated.TargetSession)
	assert.Empty(t, updated.OnComplete, "the inapplicable lifecycle is dropped, not left to fail validation")
}

// TestUpdateTaskExplicitVerbWithATargetSessionStillErrors: dropping is only for
// a field the caller did NOT mention. Asking for both in one patch is a
// contradiction and must still surface as one rather than being silently ignored.
func TestUpdateTaskExplicitVerbWithATargetSessionStillErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	require.NoError(t, AddTask(Task{
		ID: "abc", CronExpr: "0 3 * * *", Prompt: "p",
		ProjectPath: dir, Program: "claude", Enabled: true,
	}))

	target := "shared"
	verb := OnCompleteKill
	_, err := UpdateTask("abc", TaskUpdate{TargetSession: &target, OnComplete: &verb}, ProjectExpectation{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target_session")
}
