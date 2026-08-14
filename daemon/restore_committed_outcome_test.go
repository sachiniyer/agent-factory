package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestRestoreSession_RecoverFailsAfterRebuild_ReportsCommittedWithPath is
// #3236: a manual Lost/Dead restore whose Recover rebuilt the missing worktree
// (and possibly recreated its branch) before failing has already mutated — and
// persisted — durable workspace state. The failure arm persists that state on
// purpose, so returning a raw error and an empty worktree_path tells the caller
// failed-nothing-committed about a mutation that landed. The session layer
// marks post-rebuild failures with RecoverRebuiltWorkspaceError; the daemon
// must classify that as a committed mutation and preserve the real path.
func TestRestoreSession_RecoverFailsAfterRebuild_ReportsCommittedWithPath(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	backend.failWith = &session.RecoverRebuiltWorkspaceError{
		Err: errors.New("recover: failed to re-spawn session \"manual\": tmux spawn failed after rebuild"),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "manual", backend, true, session.Lost)
	// registerStarted seeds no worktree; the committed arm's contract is that the
	// REBUILT path is preserved, so the fixture must have one to preserve.
	gw, gwErr := sessiongit.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(filepath.Dir(repoPath), "wt-manual"), "manual", "af/manual", "", false, true)
	if gwErr != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", gwErr)
	}
	inst.SetGitWorktreeForTest(gw)

	worktreePath, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "manual", RepoID: repoID})
	if err == nil {
		t.Fatal("RestoreSession must surface the failed recovery")
	}
	if !isMutationCommitted(err) {
		t.Fatalf("RestoreSession error = %T %v, want a committed-mutation marker: the rebuilt worktree state is already persisted", err, err)
	}
	if want := inst.GetWorktreePath(); worktreePath != want || want == "" {
		t.Fatalf("RestoreSession worktree path = %q, want the rebuilt path %q preserved on the committed arm", worktreePath, want)
	}
	if !strings.Contains(err.Error(), "retry the restore") {
		t.Fatalf("the error must tell the caller the restore itself still needs a retry: %v", err)
	}
	if !strings.Contains(err.Error(), "tmux spawn failed after rebuild") {
		t.Fatalf("the underlying recover failure must be preserved: %v", err)
	}
}

// TestRestoreSession_RecoverFailsBeforeMutation_StaysPlainFailure pins the
// other half of #3236's contract: a recovery that failed BEFORE mutating any
// durable workspace state (no rebuild happened) remains an ordinary retryable
// failure with no committed marker and no path.
func TestRestoreSession_RecoverFailsBeforeMutation_StaysPlainFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	backend.failWith = errors.New("recover: session \"manual\" has no tmux binding")
	registerStarted(t, manager, repoID, repoPath, "manual", backend, true, session.Lost)

	worktreePath, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "manual", RepoID: repoID})
	if err == nil {
		t.Fatal("RestoreSession must surface the failed recovery")
	}
	if isMutationCommitted(err) {
		t.Fatalf("a pre-mutation recovery failure must stay an untouched, freely retryable failure, got committed: %v", err)
	}
	if worktreePath != "" {
		t.Fatalf("worktree path = %q, want empty on a plain failure", worktreePath)
	}
}
