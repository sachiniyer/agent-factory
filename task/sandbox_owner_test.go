package task

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sandbox-owner provenance on the create path (#5571 W3.5).
//
// AddTaskRequest carries a whole task.Task, so a client can populate
// sandbox_owner. resetStoreOwnedFields clears it and AddTaskChecked re-stamps
// the daemon's own value from its sandboxOwner parameter, so a client cannot
// forge which credential a task's deliveries are held to. These tests live here
// rather than in audit_test.go to keep that file under the test-file-length
// limit (#1145).

// TestAddTask_ClearsClientSuppliedSandboxOwner is the headline regression for
// the sandbox_owner forgery: a forged owner makes the daemon treat an
// operator-created task as sandbox-created — runs are refused forever for
// per-run-session tasks, and deliveries are re-authorized against the
// attacker-chosen owner. resetStoreOwnedFields clears the field; the
// sandboxOwner parameter re-stamps the daemon's own value from the credential.
func TestAddTask_ClearsClientSuppliedSandboxOwner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	created, err := AddTaskChecked(Task{
		ID: "forgedowner", Name: "Forged owner", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,
		SandboxOwner: "attacker-session-id",
	}, ActorAPI, nil, "")
	require.NoError(t, err)

	assert.Empty(t, created.SandboxOwner, "the returned record must not carry a client-supplied sandbox_owner")

	stored, err := GetTask("forgedowner")
	require.NoError(t, err)
	assert.Empty(t, stored.SandboxOwner, "the on-disk record must not carry a client-supplied sandbox_owner")
}

// TestAddTaskChecked_StampsSandboxOwnerFromParameter guards the sandbox branch:
// resetStoreOwnedFields clears any client-supplied value, and AddTaskChecked
// re-stamps the daemon's own value from the sandboxOwner parameter. A sandbox
// credential's create must carry the owner as durable provenance so every
// delivery re-checks against it (checkSandboxTaskDelivery).
func TestAddTaskChecked_StampsSandboxOwnerFromParameter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	// A client-supplied value is cleared and the daemon's parameter is stamped.
	created, err := AddTaskChecked(Task{
		ID: "sandbox01", Name: "Sandbox task", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,
		SandboxOwner: "client-forged-owner",
	}, ActorAPI, nil, "daemon-credential-owner")
	require.NoError(t, err)
	assert.Equal(t, "daemon-credential-owner", created.SandboxOwner,
		"the daemon's sandboxOwner parameter overwrites any client-supplied value")

	stored, err := GetTask("sandbox01")
	require.NoError(t, err)
	assert.Equal(t, "daemon-credential-owner", stored.SandboxOwner,
		"the on-disk record carries the daemon-stamped owner, not the client's")

	// An empty sandboxOwner parameter is an operator-created task: no provenance.
	opCreated, err := AddTaskChecked(Task{
		ID: "op01", Name: "Operator task", Prompt: "p", CronExpr: "20 * * * *",
		ProjectPath: dir, Program: "claude", Enabled: true,
		SandboxOwner: "client-forged-owner",
	}, ActorAPI, nil, "")
	require.NoError(t, err)
	assert.Empty(t, opCreated.SandboxOwner,
		"an empty sandboxOwner parameter leaves no provenance — an operator-created task")
}
