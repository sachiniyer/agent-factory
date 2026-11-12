//go:build linux

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	gitworktree "github.com/sachiniyer/agent-factory/session/git"
)

// publishUnfinishedJournal writes a fresh, owned, resumable hook-progress
// journal for (worktreePath, sessionID) at the conventional path, returning the
// journal path and receipt directory. It mirrors the on-disk shape a
// same-session rebuild produces: identical SessionID/Worktree/Prefix, distinct
// Directory/Generation.
func publishUnfinishedJournal(t *testing.T, home, worktreePath, sessionID, generation string) (journalPath, dir string) {
	t.Helper()
	dir = filepath.Join(home, "logs", "hooks", "entries-"+generation)
	if err := os.MkdirAll(filepath.Join(dir, "0"), 0700); err != nil {
		t.Fatalf("mkdir entries: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"commands":            []string{"true", "echo rebuilt > /tmp/should-not-run"},
		"worktree":            worktreePath,
		"session_id":          sessionID,
		"scope_prefix":        "af-hook-" + sessionID,
		"generation":          generation,
		"directory":           dir,
		"publication_version": 1,
		"resume_ready":        1,
	})
	if err != nil {
		t.Fatalf("marshal journal: %v", err)
	}
	sum := sha256.Sum256([]byte(worktreePath))
	journalPath = filepath.Join(home, "logs", "hooks", "progress-wt"+hex.EncodeToString(sum[:8])+".json")
	if err := os.WriteFile(journalPath, data, 0600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	return journalPath, dir
}

// TestRestoreInstallsAbandonmentThatSurvivesSameSessionRebuild is the daemon-
// level realization of the bug's E2E procedure. It drives the real Manager
// restore path (NewManager → AdoptRunningHooks → reconcileHookProgressBatch →
// installHookProgressAbandonment) with shimmed systemctl/systemd-run (the same
// harness as TestHookProgressTombstoneRestoreNeverResumes), publishes an
// UNFINISHED journal for an archived session so restore installs the abandonment
// retry with a transient marker-storage failure, publishes a replacement journal
// at the same path (new Generation, new entries- directory) while the retry is
// pending, lets the retry drain, and confirms:
//
//   - the rebuilt journal is NOT terminalized (no finished marker);
//   - a later restore's readPendingHookProgress still sees it as resumable
//     (pending != nil, err is not os.ErrNotExist) — the silent command-loss the
//     journal subsystem exists to prevent.
//
// This is the deterministic in-repo equivalent of the manual procedure in the
// test plan (archive → restart → restore → unarchive/rebuild within the retry
// window → restart → confirm resume). A real systemd user manager is not
// required because the shimmed systemctl answers the adoption probe the way an
// empty manager would.
func TestRestoreInstallsAbandonmentThatSurvivesSameSessionRebuild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	claimDaemonProcessForRestore(t)

	// Shim systemctl (empty manager) and systemd-run (exec the command). The
	// adoption probe consults systemctl; an empty answer means no survivor.
	shim := t.TempDir()
	for name, body := range map[string]string{
		"systemctl":   "#!/bin/sh\nexit 0\n",
		"systemd-run": "#!/bin/sh\nif [ \"$1\" = --help ]; then exit 0; fi\nwhile [ \"$1\" != -- ]; do shift; done; shift; exec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(shim, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	tree := t.TempDir()

	// Publish the ORIGINAL unfinished journal for an archived session. Restore
	// will see it as resumable-pending, then install the abandonment retry
	// because the row is terminal (LiveArchived).
	origJournal, origDir := publishUnfinishedJournal(t, home, tree, "rebuild-e2e", "gen1")
	if _, err := os.Stat(origJournal); err != nil {
		t.Fatalf("original journal not written: %v", err)
	}

	// Inject a transient marker-storage failure scoped to the ORIGINAL journal's
	// receipt directory so the synchronous abandonment inside
	// installHookProgressAbandonment fails and the bounded retry is scheduled.
	// The failure persists for the original directory only; the replacement
	// journal (different directory) is unaffected once storage is restored.
	originalMark := gitworktree.HookProgressMarkFinishedForTest()
	var failed atomic.Bool
	var firstAttempt sync.Once
	firstAttemptReached := make(chan struct{})
	restoreMark := gitworktree.SetHookProgressMarkFinishedForTest(func(path string, data []byte, mode os.FileMode) error {
		if path == filepath.Join(origDir, "finished") {
			firstAttempt.Do(func() { close(firstAttemptReached) })
			if !failed.Swap(true) {
				return errors.New("marker storage unavailable")
			}
		}
		return originalMark(path, data, mode)
	})
	t.Cleanup(restoreMark)

	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID: "rebuild-e2e", Title: "rebuild-e2e", Path: repoPath,
		Liveness: session.LiveArchived, StartupStateUnknown: true, BackendType: "local",
		Worktree: session.GitWorktreeData{RepoPath: repoPath, WorktreePath: tree, HookScopeUnitPrefix: "af-hook-rebuild-e2e"},
	}); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	gw := restoredInstanceByTitle(t, manager, "rebuild-e2e").GitWorktreeForTest()
	if gw == nil {
		t.Fatal("the restored archived session has no worktree")
	}
	retryDone := gw.HookProgressRetirementDoneForTest()
	if retryDone == nil {
		t.Fatal("restore did not install the abandonment for the archived session")
	}

	// The abandonment retry is now scheduled. Wait for the synchronous first
	// attempt to fail (it terminalizes the original journal and fails on the
	// injected transient error).
	select {
	case <-firstAttemptReached:
	case <-time.After(5 * time.Second):
		t.Fatal("synchronous abandonment of the original journal did not attempt to mark finished")
	}

	// While the abandonment retry is pending, the user unarches the session and
	// the rebuild path republishes a NEW journal at the same path: new
	// Generation, new entries- directory, identical SessionID/Worktree/Prefix.
	rebuiltJournal, rebuiltDir := publishUnfinishedJournal(t, home, tree, "rebuild-e2e", "gen2")
	if rebuiltJournal != origJournal {
		t.Fatalf("rebuild did not target the same journal path: %q != %q", rebuiltJournal, origJournal)
	}
	if rebuiltDir == origDir {
		t.Fatalf("rebuild did not produce a distinct receipt directory")
	}

	// Restore marker storage so a regression would actually terminalize the
	// replacement journal on the next retry attempt.
	restoreMark()

	// Wait for the abandonment retry goroutine to finish. Its completion ends
	// the wait; the deadline only bounds a retry that never finishes. The retry
	// runs up to 8 attempts at 100ms→2s backoff (~9s plus lock waits).
	select {
	case <-retryDone:
	case <-time.After(20 * time.Second):
		t.Fatal("abandonment retry did not finish within 20s")
	}

	// The rebuilt journal must NOT be terminalized.
	if _, err := os.Stat(filepath.Join(rebuiltDir, "finished")); err == nil {
		t.Fatalf("rebuilt journal was terminalized by the abandonment retry")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on rebuilt finished marker: %v", err)
	}

	// A later restore's readPendingHookProgress must see the rebuilt journal as
	// resumable — the silent command-loss class the journal subsystem exists to
	// prevent. Use the exported test seam to read through the same ownership
	// and finished-marker logic restore uses.
	pending, pendingErr := gitworktree.ReadPendingHookProgressForTest(tree, "rebuild-e2e")
	if pending == nil {
		t.Fatalf("rebuilt journal was treated as non-resumable after abandonment retry: err=%v", pendingErr)
	}
	if errors.Is(pendingErr, os.ErrNotExist) {
		t.Fatalf("rebuilt journal terminalized or treated as conclusive non-resumable: %v", pendingErr)
	}
	if pending.Directory != rebuiltDir || pending.Generation != "gen2" {
		t.Fatalf("readPendingHookProgress returned the wrong journal: dir=%s gen=%s; want %s gen2", pending.Directory, pending.Generation, rebuiltDir)
	}
}
