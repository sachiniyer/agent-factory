package task

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createDefinitionFields are the Task fields a create takes from its caller: the
// task's DEFINITION. Every other field is in storeOwnedTaskFields.
var createDefinitionFields = []string{
	"id", "name", "prompt", "cron_expr", "watch_cmd", "target_session",
	"max_concurrent_runs", "on_complete", "host", "project_path", "program", "enabled",
}

// storeOwnedTaskFields are the Task fields the store owns rather than the caller
// (see resetStoreOwnedFields). A create must not keep a client-supplied value for
// any of them.
var storeOwnedTaskFields = []string{
	// History and provenance, persisted.
	"repo_id", "sandbox_owner", "created_at", "last_run_at", "last_run_status",
	"last_run_target", "last_run_id", "last_run_started_at", "dropped_events", "audit",
	// Derived at read time, never persisted (stripDerived).
	"overdue", "missed_occurrences", "missed_occurrences_capped", "unschedulable",
	"unschedulable_reason", "unassessable", "next_run_at", "next_run_far", "arming",
	"ordinal", "store_generation",
}

func taskJSONName(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("json"), ",")[0]
}

func taskFieldByJSONName(t *testing.T, tk Task, name string) reflect.Value {
	t.Helper()
	v := reflect.ValueOf(tk)
	for i := 0; i < v.NumField(); i++ {
		if taskJSONName(v.Type().Field(i)) == name {
			return v.Field(i)
		}
	}
	t.Fatalf("Task has no field with json name %q", name)
	return reflect.Value{}
}

// TestTaskFieldsAreClassifiedForCreate makes adding a Task field a decision. A
// new field fails here until it is listed as either definition or store-owned,
// and a store-owned one is then forged by TestAddTask_ResetsEveryStoreOwnedField.
// LastRunTarget (#7059) and SandboxOwner (#7048) were both added after the reset
// existed, and both were missed by it.
func TestTaskFieldsAreClassifiedForCreate(t *testing.T) {
	listed := map[string]int{}
	for _, name := range append(append([]string{}, createDefinitionFields...), storeOwnedTaskFields...) {
		listed[name]++
	}
	typ := reflect.TypeOf(Task{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := taskJSONName(f)
		assert.Equal(t, 1, listed[name],
			"Task.%s (%q) must be listed exactly once: in createDefinitionFields if a create takes it from the caller, "+
				"or in storeOwnedTaskFields if the store owns it (then clear it in resetStoreOwnedFields)", f.Name, name)
		delete(listed, name)
	}
	assert.Empty(t, listed, "listed names that are not Task fields")
}

// TestAddTask_ResetsEveryStoreOwnedField is the class rather than its members:
// three review rounds on #3623 found the same defect through three different
// fields, and #7048 and #7059 found it twice more. So the create path resets
// all of them together, and this test forges every one at once.
func TestAddTask_ResetsEveryStoreOwnedField(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	future := time.Now().Add(48 * time.Hour)
	next := time.Now().Add(time.Hour)
	forged := Task{
		ID: "kitchen1", Name: "Everything at once", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,

		RepoID: "forged-repo-id", SandboxOwner: "forged-session-id",
		CreatedAt: future, LastRunAt: &future, LastRunStatus: "started",
		LastRunTarget: "someone-elses-session", LastRunID: "forged-run-id", LastRunStartedAt: &future,
		DroppedEvents: 99,
		Audit:         []AuditEntry{{At: future, Actor: ActorCLI, Action: AuditEnabled}},

		Overdue: true, MissedOccurrences: 99, MissedOccurrencesCapped: true,
		Unschedulable: true, UnschedulableReason: "forged", Unassessable: true,
		NextRunAt: &next, NextRunFar: true, Arming: ArmingArmed,
		Ordinal: 99, StoreGeneration: "forged-generation",
	}
	created, err := AddTaskChecked(forged, ActorAPI, nil, "")
	require.NoError(t, err)
	stored, err := GetTask("kitchen1")
	require.NoError(t, err)

	for _, name := range storeOwnedTaskFields {
		sent := taskFieldByJSONName(t, forged, name)
		require.False(t, sent.IsZero(), "%s: the kitchen sink must forge every store-owned field", name)
		assert.NotEqual(t, sent.Interface(), taskFieldByJSONName(t, created, name).Interface(),
			"%s: the client's value survived into the record AddTaskChecked returned", name)
		assert.NotEqual(t, sent.Interface(), taskFieldByJSONName(t, *stored, name).Interface(),
			"%s: the client's value survived into the stored record", name)
	}

	// The fields the store stamps rather than clears hold the store's value.
	assert.False(t, created.CreatedAt.After(time.Now().Add(time.Minute)), "created_at clamped")
	require.Len(t, created.Audit, 1, "only the store's own create entry")
	assert.Equal(t, AuditCreated, created.Audit[0].Action)
	assert.Empty(t, stored.SandboxOwner, "an operator create (empty sandboxOwner) has no sandbox provenance")
	assert.Empty(t, stored.RepoID, "re-derived from a project path that is not a repo")
	assert.Empty(t, stored.LastRunTarget, "a task that has never run has no delivery target")
}
