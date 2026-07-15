package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Session teardown must leave no editor behind. These drive the real
// ArchiveSession/KillSession paths and assert the supervisor entry is gone —
// registering a marker rather than spawning a real code-server, so the test
// proves the LIFECYCLE WIRING (which is the part that rots) without paying for a
// process start. vscode_server_test.go covers actually killing a real child.

// registerVSCodeMarker stands a marker editor in the supervisor for key.
func registerVSCodeMarker(m *Manager, key string) {
	m.vscode.mu.Lock()
	defer m.vscode.mu.Unlock()
	m.vscode.servers[key] = &vscodeServer{worktree: "/nowhere", exited: make(chan struct{})}
}

func vscodeServerRegistered(m *Manager, key string) bool {
	m.vscode.mu.Lock()
	defer m.vscode.mu.Unlock()
	_, ok := m.vscode.servers[key]
	return ok
}

// TestArchiveSession_StopsVSCodeEditor: archiving MOVES the worktree, and the
// editor's cwd is that worktree — leaving it running would strand it serving a
// path that no longer exists.
func TestArchiveSession_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	key := daemonInstanceKey(repoID, "worker")
	registerVSCodeMarker(manager, key)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	require.False(t, vscodeServerRegistered(manager, key),
		"archiving a session left its VS Code editor running against the moved worktree")
}

// TestKillSession_StopsVSCodeEditor: killing removes the worktree, so its editor
// must go with it rather than linger as an orphan holding a loopback port.
func TestKillSession_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	key := daemonInstanceKey(repoID, "worker")
	registerVSCodeMarker(manager, key)

	_, err := manager.KillSession(KillSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	require.False(t, vscodeServerRegistered(manager, key),
		"killing a session left its VS Code editor running")
}
