package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- HookBackend launch (no prompt) ---

// --- HookBackend launch failure ---

func TestHookBackendStartLaunchCmdFails(t *testing.T) {
	dir := t.TempDir()
	launchCmd := writeScript(t, dir, "launch.sh", `exit 1`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{LaunchCmd: launchCmd},
	}

	i := &Instance{
		Title:   "fail-test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "launch_cmd failed")
	assert.False(t, i.Started())
}

func TestHookBackendStartLaunchCmdBadJSON(t *testing.T) {
	dir := t.TempDir()
	launchCmd := writeScript(t, dir, "launch.sh", `echo "not json"`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{LaunchCmd: launchCmd},
	}

	i := &Instance{
		Title:   "badjson-test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no JSON")
}

// --- HookBackend kill failure ---

func TestHookBackendKillDeleteCmdFails(t *testing.T) {
	dir := t.TempDir()
	deleteCmd := writeScript(t, dir, "delete.sh", `echo "error" >&2; exit 1`)
	launchCmd := writeScript(t, dir, "launch.sh",
		`echo '{"name": "test", "status": "running"}'`)
	attachCmd := writeScript(t, dir, "attach.sh", `sleep 0.1`)
	listCmd := writeScript(t, dir, "list.sh", `echo '[]'`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{
			LaunchCmd: launchCmd,
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}

	i := &Instance{
		Title:   "test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)
	assert.True(t, i.Started())

	err = b.Kill(i)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "delete_cmd failed")

	// Even when delete_cmd fails, the instance must be marked stopped so the
	// UI doesn't show it as running while its PTY is already closed (#266).
	// remoteMeta, however, must be PRESERVED on failure: it is the only record
	// that a remote session was allocated, and a retried Kill needs it to
	// re-run delete_cmd. Clearing it early leaked the remote session (#922).
	assert.False(t, i.Started(), "instance should be stopped after failed Kill")
	i.mu.RLock()
	meta := i.remoteMeta
	i.mu.RUnlock()
	assert.NotNil(t, meta, "remoteMeta should be preserved after a failed Kill so a retry can re-run delete_cmd (#922)")
	// The PTY should have been cleaned up too.
	assert.Nil(t, b.getPTY(i.Title), "PTY should be closed after failed Kill")
}
