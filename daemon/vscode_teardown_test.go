package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"

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

// TestEnsureVSCodeServer_RefusesInertSessions is the codex P1 gate: the webtab
// proxy resolves (and may spawn) an editor WITHOUT the kill/archive op-lock — it
// must, since a spawn blocks for seconds — so nothing but this check stops a
// stale iframe refresh, or simply selecting an archived row that still has a
// vscode tab, from starting an editor for a session whose worktree is being moved
// or removed.
func TestEnsureVSCodeServer_RefusesInertSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")

	// A live session resolves (it gets as far as needing a binary, which is the
	// point: the gate is not what stops it).
	if _, err := manager.ensureVSCodeServer(inst, repoID, "worker"); err != nil &&
		strings.Contains(err.Error(), "archived") {
		t.Fatalf("a LIVE session was refused as inert: %v", err)
	}

	// Archived: refused, and named actionably.
	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatalf("BeginArchive: %v", err)
	}
	if err := inst.Transition(session.CommitArchive()); err != nil {
		t.Fatalf("CommitArchive: %v", err)
	}
	_, err := manager.ensureVSCodeServer(inst, repoID, "worker")
	if err == nil {
		t.Fatal("ensureVSCodeServer started an editor for an ARCHIVED session; archive must stop it until restore")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("err = %v, want one naming the archived state", err)
	}
}

// TestEnsureVSCodeServer_RefusesMidArchive covers the in-flight window rather than
// the settled state: BeginArchive raises the fence before the worktree moves, and
// a request arriving in that window must not start an editor rooted at a directory
// that is about to be relocated.
func TestEnsureVSCodeServer_RefusesMidArchive(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")

	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatalf("BeginArchive: %v", err)
	}
	_, err := manager.ensureVSCodeServer(inst, repoID, "worker")
	if err == nil {
		t.Fatal("ensureVSCodeServer started an editor for a session mid-archive")
	}
	if !strings.Contains(err.Error(), "being archived or removed") {
		t.Fatalf("err = %v, want one naming the in-flight teardown", err)
	}
}
