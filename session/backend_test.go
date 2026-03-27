package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	log.Initialize(false)
	defer log.Close()
	os.Exit(m.Run())
}

// --- Backend interface compliance ---

func TestLocalBackendType(t *testing.T) {
	b := &LocalBackend{}
	assert.Equal(t, "local", b.Type())
}

func TestHookBackendType(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	assert.Equal(t, "remote", b.Type())
}

// --- IsRemote helper ---

func TestIsRemote(t *testing.T) {
	t.Run("local backend", func(t *testing.T) {
		i := &Instance{backend: &LocalBackend{}}
		assert.False(t, i.IsRemote())
	})
	t.Run("hook backend", func(t *testing.T) {
		i := &Instance{backend: &HookBackend{Hooks: config.RemoteHooks{}}}
		assert.True(t, i.IsRemote())
	})
	t.Run("nil backend", func(t *testing.T) {
		i := &Instance{}
		assert.False(t, i.IsRemote())
	})
}

// --- HookBackend with real scripts ---

// writeScript writes an executable shell script to the given path.
func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0755)
	require.NoError(t, err)
	return path
}

// makeHooks creates a set of fake hook scripts in a temp dir and returns
// a HookBackend configured to use them.
func makeHooks(t *testing.T) *HookBackend {
	t.Helper()
	dir := t.TempDir()

	launchCmd := writeScript(t, dir, "launch.sh",
		`echo '{"name": "'"$2"'", "status": "running"}'`)
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "test-session", "status": "running"}]'`)
	attachCmd := writeScript(t, dir, "attach.sh",
		`echo "attached to $1"; sleep 0.1`)
	deleteCmd := writeScript(t, dir, "delete.sh",
		`echo '{"name": "'"$2"'", "deleted": true}'`)

	return &HookBackend{
		Hooks: config.RemoteHooks{
			LaunchCmd: launchCmd,
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}
}

func TestHookBackendStartFirstTime(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)
	assert.True(t, i.Started())
	assert.Equal(t, "test-session", i.Branch)
	assert.NotNil(t, i.remoteMeta)
	assert.Equal(t, "running", i.remoteMeta["status"])

	// Cleanup
	b.closePTY(i.Title)
}

func TestHookBackendStartRestore(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, false)
	require.NoError(t, err)
	assert.True(t, i.Started())

	b.closePTY(i.Title)
}

func TestHookBackendStartEmptyTitle(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "",
		backend: b,
	}
	err := b.Start(i, true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestHookBackendKill(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	// Start first so there's something to kill
	err := b.Start(i, true)
	require.NoError(t, err)
	assert.True(t, i.Started())

	err = b.Kill(i)
	require.NoError(t, err)
	assert.False(t, i.Started())
	assert.Nil(t, i.remoteMeta)
}

func TestHookBackendPreview(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)

	// Give the background PTY reader a moment to capture output
	time.Sleep(500 * time.Millisecond)

	content, err := b.Preview(i)
	require.NoError(t, err)
	// The attach.sh script echoes "attached to test-session"
	assert.Contains(t, content, "attached to test-session")

	b.closePTY(i.Title)
}

func TestHookBackendPreviewFullHistory(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	content, err := b.PreviewFullHistory(i)
	require.NoError(t, err)
	assert.Contains(t, content, "attached to test-session")

	b.closePTY(i.Title)
}

func TestHookBackendPreviewNoPTY(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{Title: "no-pty", backend: b}

	content, err := b.Preview(i)
	require.NoError(t, err)
	assert.Equal(t, "", content)
}

func TestHookBackendIsAlive(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		backend: b,
	}

	alive := b.IsAlive(i)
	assert.True(t, alive)
}

func TestHookBackendIsAliveNotFound(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "nonexistent-session",
		backend: b,
	}

	alive := b.IsAlive(i)
	assert.False(t, alive)
}

func TestHookBackendIsAliveFailedCmd(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `exit 1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test", backend: b}
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendHasUpdated(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	updated, hasPrompt := b.HasUpdated(i)
	assert.False(t, updated)
	assert.False(t, hasPrompt)
}

func TestHookBackendSendPromptReturnsError(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	err := b.SendPrompt(i, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestHookBackendSendPromptCommandReturnsError(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	err := b.SendPromptCommand(i, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestHookBackendSetPreviewSizeIsNoop(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	err := b.SetPreviewSize(i, 80, 24)
	assert.NoError(t, err)
}

func TestHookBackendCheckAndHandleTrustPrompt(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	assert.False(t, b.CheckAndHandleTrustPrompt(i))
}

func TestHookBackendTapEnterIsNoop(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	// Should not panic
	b.TapEnter(i)
}

func TestHookBackendAttachNotStarted(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b, started: false}
	_, err := b.Attach(i)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not been started")
}

// --- Serialization round-trip ---

func TestToInstanceDataIncludesBackendType(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		i := &Instance{
			Title:   "local-inst",
			backend: &LocalBackend{},
		}
		data := i.ToInstanceData()
		assert.Equal(t, "local", data.BackendType)
		assert.Nil(t, data.RemoteMeta)
	})

	t.Run("remote", func(t *testing.T) {
		meta := map[string]interface{}{"name": "test", "status": "running"}
		i := &Instance{
			Title:      "remote-inst",
			backend:    &HookBackend{Hooks: config.RemoteHooks{}},
			remoteMeta: meta,
		}
		data := i.ToInstanceData()
		assert.Equal(t, "remote", data.BackendType)
		assert.Equal(t, "test", data.RemoteMeta["name"])
		assert.Equal(t, "running", data.RemoteMeta["status"])
	})
}

func TestInstanceDataJSONRoundTrip(t *testing.T) {
	t.Run("local backend serializes correctly", func(t *testing.T) {
		data := InstanceData{
			Title:       "test-local",
			Path:        "/tmp/test",
			Branch:      "main",
			Status:      Running,
			BackendType: "local",
			Program:     "claude",
		}

		jsonBytes, err := json.Marshal(data)
		require.NoError(t, err)

		var restored InstanceData
		err = json.Unmarshal(jsonBytes, &restored)
		require.NoError(t, err)

		assert.Equal(t, "local", restored.BackendType)
		assert.Equal(t, "test-local", restored.Title)
		assert.Nil(t, restored.RemoteMeta)
	})

	t.Run("remote backend serializes correctly", func(t *testing.T) {
		meta := map[string]interface{}{
			"name":   "fix-bug",
			"status": "running",
			"host":   "remote-1.example.com",
		}
		data := InstanceData{
			Title:       "test-remote",
			Path:        "/tmp/test",
			Branch:      "fix-bug",
			Status:      Running,
			BackendType: "remote",
			RemoteMeta:  meta,
		}

		jsonBytes, err := json.Marshal(data)
		require.NoError(t, err)

		var restored InstanceData
		err = json.Unmarshal(jsonBytes, &restored)
		require.NoError(t, err)

		assert.Equal(t, "remote", restored.BackendType)
		assert.Equal(t, "fix-bug", restored.RemoteMeta["name"])
		assert.Equal(t, "running", restored.RemoteMeta["status"])
		assert.Equal(t, "remote-1.example.com", restored.RemoteMeta["host"])
	})

	t.Run("empty backend_type defaults to empty string", func(t *testing.T) {
		// Simulate old data without backend_type
		jsonStr := `{"title":"old-inst","path":"/tmp","branch":"main","status":0}`
		var restored InstanceData
		err := json.Unmarshal([]byte(jsonStr), &restored)
		require.NoError(t, err)
		assert.Equal(t, "", restored.BackendType)
		assert.Nil(t, restored.RemoteMeta)
	})
}

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

	err = b.Kill(i)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "delete_cmd failed")
}

// --- PTY management ---

func TestHookBackendPTYEnsureIdempotent(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "pty-test",
		Path:    t.TempDir(),
		backend: b,
	}

	// ensurePTY should be safe to call multiple times
	b.ensurePTY(i)
	b.ensurePTY(i) // Should not create a second PTY

	b.mu.Lock()
	count := len(b.ptys)
	b.mu.Unlock()
	assert.Equal(t, 1, count)

	b.closePTY(i.Title)

	b.mu.Lock()
	count = len(b.ptys)
	b.mu.Unlock()
	assert.Equal(t, 0, count)
}

func TestHookBackendClosePTYNonexistent(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	// Should not panic
	b.closePTY("nonexistent")
}

// --- Instance delegation ---

func TestInstanceDelegatesStartToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)
	assert.True(t, i.Started())

	b.closePTY(i.Title)
}

func TestInstanceDelegatesKillToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-kill",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)

	err = i.Kill()
	require.NoError(t, err)
	assert.False(t, i.Started())
}

func TestInstanceDelegatesPreviewToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-preview",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	content, err := i.Preview()
	require.NoError(t, err)
	assert.Contains(t, content, "attached to delegate-preview")

	b.closePTY(i.Title)
}

func TestInstanceRepoNameErrorsForRemote(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{
		Title:   "remote-inst",
		backend: b,
		started: true,
	}
	_, err := i.RepoName()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote")
}

func TestInstanceGetWorktreePathEmptyForRemote(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{
		Title:   "remote-inst",
		backend: b,
		started: true,
	}
	assert.Equal(t, "", i.GetWorktreePath())
}

// --- list_cmd variations ---

func TestHookBackendIsAliveWithBadJSON(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `echo "not json"`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test", backend: b}
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendIsAliveWithStoppedSession(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "test-session", "status": "stopped"}]'`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test-session", backend: b}
	// status=stopped means not alive
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendIsAliveWithMultipleSessions(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "session-a", "status": "stopped"}, {"name": "session-b", "status": "running"}]'`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}

	iA := &Instance{Title: "session-a", backend: b}
	iB := &Instance{Title: "session-b", backend: b}

	assert.False(t, b.IsAlive(iA))
	assert.True(t, b.IsAlive(iB))
}
