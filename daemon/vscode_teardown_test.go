package daemon

import (
	"strings"
	"testing"
	"time"

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
	m.mu.Lock()
	instanceID := ""
	if inst := m.instances[key]; inst != nil {
		instanceID = inst.ID
	}
	m.mu.Unlock()
	m.vscode.mu.Lock()
	defer m.vscode.mu.Unlock()
	m.vscode.servers[key] = &vscodeServer{
		worktree: "/nowhere", instanceID: instanceID, exited: make(chan struct{}),
	}
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

// TestFinishUserKill_StopsVSCodeEditor covers the post-tombstone retry path:
// a kill interrupted after committing its durable intent must converge to the
// same no-editor end state as the synchronous KillSession tail.
func TestFinishUserKill_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "worker", session.NewFakeBackend(), true, session.Ready)
	key := daemonInstanceKey(repoID, "worker")
	registerVSCodeMarker(manager, key)
	require.NoError(t, manager.persistKillTombstone(repoID, inst, nil))

	manager.finishUserKill(repoID, inst)

	require.False(t, vscodeServerRegistered(manager, key),
		"finishing a tombstoned kill left its VS Code editor running")
}

// TestFinishUserKill_StopsEditorOwnedByPreviousSupervisor models the daemon-crash
// case that creates a tombstone retry. The restarted daemon has an empty
// supervisor map, so the editor can be found only through durable process
// ownership recorded by the daemon that spawned it.
func TestFinishUserKill_StopsEditorOwnedByPreviousSupervisor(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, _, _, _ := newVSCodeFixture(t, binary)
	const title = "vscodeproxy"
	inst, repoID := vscodeFixtureInstance(t, manager, title)
	key := daemonInstanceKey(repoID, title)

	if _, err := manager.ensureVSCodeServer(inst, repoID, title); err != nil {
		t.Fatalf("ensureVSCodeServer: %v", err)
	}
	manager.vscode.mu.Lock()
	previousEditor := manager.vscode.servers[key]
	manager.vscode.mu.Unlock()
	if previousEditor == nil || !previousEditor.alive() {
		t.Fatal("precondition: previous supervisor did not own a live editor")
	}
	// Keep the session teardown itself inert so the only possible reaper is the
	// restarted supervisor's durable-ownership path. A local backend may discover
	// and reap processes rooted in its worktree, which would let this pass without
	// proving stopFor found the previous daemon's editor.
	inst.SetBackend(session.NewFakeBackend())

	// Simulate the daemon restart: durable session state survives, but the new
	// supervisor has no pointer to the old process.
	manager.vscode = newVSCodeSupervisor()
	require.NoError(t, manager.persistKillTombstone(repoID, inst, nil))
	require.True(t, previousEditor.alive(), "persisting the tombstone unexpectedly reaped the editor")
	manager.finishUserKill(repoID, inst)

	select {
	case <-previousEditor.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("finishing the tombstoned kill could not reap the previous daemon's editor")
	}
}

// TestReapDeadRoot_StopsVSCodeEditor is the adjacent automatic teardown path.
// A dead root is removed and recreated under the same session key, but its old
// vscode tabs are not carried forward, so retaining their editor leaks an
// unreachable daemon child into the replacement root's lifetime.
func TestReapDeadRoot_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, session.RootSessionTitle, session.NewFakeBackend(), true, session.Dead)
	key := daemonInstanceKey(repoID, session.RootSessionTitle)
	registerVSCodeMarker(manager, key)

	reaped, err := manager.reapDeadRoot(repoID, inst)
	require.NoError(t, err)
	require.True(t, reaped)
	require.False(t, vscodeServerRegistered(manager, key),
		"reaping a dead root left its unreachable VS Code editor running")
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
