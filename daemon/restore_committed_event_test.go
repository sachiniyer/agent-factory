package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// registerLostRecoverBackend builds the committed-but-unfinished restore fixture
// at the handler layer: a Lost session whose Recover fails after a (test-seeded)
// worktree rebuild — the exact path of #3236/#3353, and the same fixture
// TestRestoreSession_RecoverFailsAfterRebuild_ReportsCommittedWithPath drives at
// the manager layer. The backend's Recover controls whether the failure is
// permanent (the spurious-event test) or fail-then-succeed (the duplicate test).
func registerLostRecoverBackend(t *testing.T, title string, backend session.Backend) (*Manager, string) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, title, backend, true, session.Lost)
	// registerStarted seeds no worktree; the committed arm's contract is that the
	// REBUILT path is preserved, so the fixture must have one to preserve.
	gw, gwErr := sessiongit.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(filepath.Dir(repoPath), "wt-"+title), title, "af/"+title, "", false, true)
	if gwErr != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", gwErr)
	}
	inst.SetGitWorktreeForTest(gw)
	return manager, repoID
}

// restoredRebuildErr is the failure shape the session layer marks a post-rebuild
// recover with, reused across both handler tests so they exercise the exact
// committed-but-unfinished arm of #3353.
func restoredRebuildErr(title string) *session.RecoverRebuiltWorkspaceError {
	return &session.RecoverRebuiltWorkspaceError{
		Err: errors.New("recover: failed to re-spawn session " + title + ": tmux spawn failed after rebuild"),
	}
}

// TestControlRestoreSession_CommittedRecoverFailure_PublishesSpuriousRestoredEvent
// pins the handler half of the #3353 bug: a committed-but-unfinished restore that
// the manager's own marker says is NOT restored must land in the RPC envelope
// (ErrorCodeMutationCommitted) WITHOUT publishing session.restored on the events
// plane — that event means the synchronous restore call completed, which it did
// not. Mirrors the kill analog's events-plane assertion shape in
// TestControlKillSession_CommittedTeardownFailure_FillsEnvelopeWithoutKilledEvent.
func TestControlRestoreSession_CommittedRecoverFailure_PublishesSpuriousRestoredEvent(t *testing.T) {
	const title = "committed-restore"
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	backend.failWith = restoredRebuildErr(title)
	manager, repoID := registerLostRecoverBackend(t, title, backend)
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()
	var resp RestoreSessionResponse
	if err := cs.RestoreSession(RestoreSessionRequest{Title: title, RepoID: repoID}, &resp); err != nil {
		t.Fatalf("a committed restore must land in the envelope, not be returned as an rpc error: %v", err)
	}
	if !resp.OK {
		t.Fatal("resp.OK = false, want true: the mutation committed")
	}
	if resp.MutationOutcome.Code != apiproto.ErrorCodeMutationCommitted {
		t.Fatalf("resp code = %q, want %q", resp.MutationOutcome.Code, apiproto.ErrorCodeMutationCommitted)
	}
	if !strings.Contains(resp.MutationOutcome.Warning, "retry the restore") {
		t.Fatalf("the warning must tell the caller the session is NOT restored and to retry, got %q", resp.MutationOutcome.Warning)
	}

	// publish is synchronous, so anything the handler emitted is already queued.
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionRestored {
				t.Fatal("the handler published session.restored for a committed-but-unfinished restore: the manager's own marker says the session is NOT restored and the caller must retry, and a successful retry would publish a second restored event for one logical restore — the same duplicate the kill handler's gate exists to prevent")
			}
		default:
			return
		}
	}
}

// failOnceRecoverBackend models the committed-but-unfinished retry contract: its
// first Recover returns the configured (rebuilt-workspace) failure that produces
// the committed marker, and every subsequent Recover succeeds — the recovery the
// marker directs the caller to retry into.
type failOnceRecoverBackend struct {
	*recoverFakeBackend
	failed atomic.Bool
}

func (b *failOnceRecoverBackend) Recover(inst *session.Instance) error {
	if !b.failed.Swap(true) {
		// First attempt: fail exactly as the parent would with failWith set,
		// accounting for the call under the parent's own counter.
		b.recoverFakeBackend.mu.Lock()
		defer b.recoverFakeBackend.mu.Unlock()
		b.recoverFakeBackend.recovers++
		return b.recoverFakeBackend.failWith
	}
	// Retry: clear the configured failure so the parent's success arm runs.
	b.recoverFakeBackend.mu.Lock()
	b.recoverFakeBackend.failWith = nil
	b.recoverFakeBackend.mu.Unlock()
	return b.recoverFakeBackend.Recover(inst)
}

// TestControlRestoreSession_CommittedThenSucceed_PublishesDuplicateRestoredEvent
// pins the duplicate half of the #3353 bug: when a caller follows the committed
// marker's advice and retries through the handler, the retry succeeds and — with
// the unconditional publish — emits a SECOND session.restored for one logical
// restore. Under the fix the committed-but-unfinished first attempt must not
// publish, and only the successful retry should — exactly one restored event.
func TestControlRestoreSession_CommittedThenSucceed_PublishesDuplicateRestoredEvent(t *testing.T) {
	const title = "committed-then-succeed"
	backend := &failOnceRecoverBackend{
		recoverFakeBackend: &recoverFakeBackend{
			FakeBackend: session.NewFakeBackend(),
			failWith:    restoredRebuildErr(title),
		},
	}
	manager, repoID := registerLostRecoverBackend(t, title, backend)
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()

	// First handler call: committed-but-unfinished (the marker says retry).
	var first RestoreSessionResponse
	if err := cs.RestoreSession(RestoreSessionRequest{Title: title, RepoID: repoID}, &first); err != nil {
		t.Fatalf("a committed restore must land in the envelope, not be returned as an rpc error: %v", err)
	}
	if first.MutationOutcome.Code != apiproto.ErrorCodeMutationCommitted {
		t.Fatalf("first resp code = %q, want %q", first.MutationOutcome.Code, apiproto.ErrorCodeMutationCommitted)
	}

	// Retry through the handler, exactly as the marker directs: this one succeeds.
	var retry RestoreSessionResponse
	if err := cs.RestoreSession(RestoreSessionRequest{Title: title, RepoID: repoID}, &retry); err != nil {
		t.Fatalf("the retry a committed marker directs must complete the restore, not return an rpc error: %v", err)
	}
	if !retry.OK {
		t.Fatal("retry resp.OK = false, want true: the restore completed")
	}
	if retry.MutationOutcome.Code != "" {
		t.Fatalf("retry resp code = %q, want empty: the retry succeeded cleanly", retry.MutationOutcome.Code)
	}

	// Count restored events for the one logical restore: the spurious first-attempt
	// publish must be gone, leaving exactly the successful retry's single event.
	restored := 0
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionRestored {
				restored++
			}
		default:
			if restored != 1 {
				t.Fatalf("one logical user restore produced %d session.restored events, want 1: the committed-but-unfinished first attempt must not publish, only the successful retry should", restored)
			}
			return
		}
	}
}

// TestControlRestoreArchived_CommittedRelocate_PublishesSpuriousRestoredEvent
// pins the handler half of the #3353 bug for the RestoreArchived handler, which
// received the identical `if err == nil` gate. The committed-but-unfinished arm
// for archived restores is a worktree relocate that LANDED before the durable
// record write (or the agent re-spawn) failed: restoreArchivedInstance returns a
// mutationCommittedError via failedRestoredArchiveResult so the relocate is not
// misread as failed-nothing-committed (#3235). The handler must record that marker
// in the envelope WITHOUT publishing session.restored — the restore did not
// complete. Mirrors the manager-level fixture
// TestRestoreArchived_SuccessfulRelocateReportsAFailedPersist.
func TestControlRestoreArchived_CommittedRelocate_PublishesSpuriousRestoredEvent(t *testing.T) {
	const title = "committed-archived"
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, title)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	if _, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID}); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
	restored, pathErr := sessiongit.RestoreWorktreePath(repoPath, title, inst.GetBranch())
	if pathErr != nil {
		t.Fatalf("RestoreWorktreePath: %v", pathErr)
	}

	// Fail exactly the restore commit: the write that first carries the RESTORED
	// location, taken BEFORE the re-spawn. The relocate lands first, so this is a
	// committed-but-unfinished restore — the path that produces the
	// mutationCommittedError the handler is publishing over.
	diskFull := errors.New("no space left on device")
	var mu sync.Mutex
	fired := false
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title != title || data.Worktree.WorktreePath != restored {
			return nil
		}
		mu.Lock()
		fired = true
		mu.Unlock()
		return diskFull
	}

	cs := &controlServer{manager: manager}
	_, ch := manager.events.subscribe()

	var resp RestoreArchivedResponse
	if err := cs.RestoreArchived(RestoreArchivedRequest{Title: title, RepoID: repoID}, &resp); err != nil {
		t.Fatalf("a committed archive restore must land in the envelope, not be returned as an rpc error: %v", err)
	}
	if !resp.OK {
		t.Fatal("resp.OK = false, want true: the mutation committed")
	}
	if resp.MutationOutcome.Code != apiproto.ErrorCodeMutationCommitted {
		t.Fatalf("resp code = %q, want %q", resp.MutationOutcome.Code, apiproto.ErrorCodeMutationCommitted)
	}
	// Re-assure the test exercised the committed arm rather than passing vacuously.
	mu.Lock()
	if !fired {
		mu.Unlock()
		t.Fatal("the restore commit was never attempted with the restored path, so this test exercised nothing")
	}
	mu.Unlock()

	// publish is synchronous, so anything the handler emitted is already queued.
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionRestored {
				t.Fatal("the RestoreArchived handler published session.restored for a committed-but-unfinished restore: the worktree relocate landed but the durable write failed, so the restore did not complete — the same gate the RestoreSession handler and the kill handler apply")
			}
		default:
			return
		}
	}
}
