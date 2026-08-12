package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCleanupClaimedRepoGone_DoubleFailureRetainsSecuredRoot(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	previousAfterClaim := removeTreeAfterEntryClaim
	removeTreeAfterEntryClaim = func(_ *os.File, parentPath, claimedName, originalName string) error {
		if parentPath != filepath.Dir(worktree) {
			return nil
		}
		secured := filepath.Join(parentPath, claimedName)
		if err := os.WriteFile(filepath.Join(secured, "late-work.txt"), []byte("writer survived"), 0o644); err != nil {
			return err
		}
		original := filepath.Join(parentPath, originalName)
		if err := os.Mkdir(original, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(original, "replacement.txt"), []byte("not ours"), 0o644)
	}
	t.Cleanup(func() { removeTreeAfterEntryClaim = previousAfterClaim })

	state, err := gw.CleanupClaimedRepoGone(claim)
	if state != CleanupStateUnknown || err == nil {
		t.Fatalf("double unlink/restore failure must remain retryable; state=%v err=%v", state, err)
	}
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupFinalizing || recovery.AlternatePath == "" {
		t.Fatalf("secured root lost its discoverable finalization handle; retained=%v recovery=%+v", retained, recovery)
	}
	if _, err := os.Stat(filepath.Join(worktree, "replacement.txt")); err != nil {
		t.Fatalf("cleanup disturbed the same-path replacement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recovery.AlternatePath, "late-work.txt")); err != nil {
		t.Fatalf("persisted alternate does not identify the secured archive: %v", err)
	}
	if err := gw.ValidateRelocationCleanupAdmission(); !errors.Is(err, ErrRelocateStateUnknown) {
		t.Fatalf("repopulated secured finalization root must be refused before the kill commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "replacement.txt")); err != nil {
		t.Fatalf("pre-commit refusal disturbed the same-path replacement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recovery.AlternatePath, "late-work.txt")); err != nil {
		t.Fatalf("pre-commit refusal disturbed the secured archive: %v", err)
	}

	removeTreeAfterEntryClaim = previousAfterClaim
	if err := os.Remove(filepath.Join(recovery.AlternatePath, "late-work.txt")); err != nil {
		t.Fatalf("empty secured root for retry: %v", err)
	}
	if err := gw.ValidateRelocationCleanupAdmission(); err != nil {
		t.Fatalf("empty secured finalization root should pass admission: %v", err)
	}
	retry, err := gw.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("resolve secured finalization root: %v", err)
	}
	state, err = gw.CleanupClaimedRepoGone(retry)
	if state != CleanupSettled || err != nil {
		t.Fatalf("secured-root retry did not settle: state=%v err=%v", state, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "replacement.txt")); err != nil {
		t.Fatalf("secured-root retry deleted the replacement: %v", err)
	}
}

func TestCleanupClaimedRepoGone_FinalizingGenerationChangeRetainsRecord(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	if err := os.Remove(filepath.Join(worktree, "work.txt")); err != nil {
		t.Fatalf("empty finalizing root: %v", err)
	}
	finalizing, err := NewGitWorktreeFromStorage(
		gw.GetRepoPath(), worktree, "repo-gone", "af/repo-gone", "", false, true,
	)
	if err != nil {
		t.Fatalf("restore finalizing worktree: %v", err)
	}
	if err := finalizing.RestoreRelocationRecovery(RelocationRecovery{
		State: RelocationRecoveryCleanupFinalizing, IdentityKnown: true,
		Device: claim.identity.device, Inode: claim.identity.inode, FileType: claim.identity.fileType,
		CleanupGeneration: claim.cleanupGeneration,
	}); err != nil {
		t.Fatalf("restore finalizing recovery: %v", err)
	}
	finalizingClaim, err := finalizing.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("claim finalizing root: %v", err)
	}
	if err := unix.Setxattr(worktree, cleanupGenerationXattr, []byte("changed-after-admission"), 0); err != nil {
		t.Fatalf("change generation after admission: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "late-work.txt"), []byte("writer survived"), 0o644); err != nil {
		t.Fatalf("repopulate changed-generation root: %v", err)
	}

	state, err := finalizing.CleanupClaimedRepoGone(finalizingClaim)
	if state != CleanupStateUnknown || err == nil {
		t.Fatalf("changed finalizing generation must remain unresolved; state=%v err=%v", state, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "late-work.txt")); err != nil {
		t.Fatalf("cleanup touched changed-generation work: %v", err)
	}
	recovery, retained := finalizing.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupFinalizing {
		t.Fatalf("changed generation lost its finalizing record; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestCleanupClaimedRepoGone_LateLiveSuccessRetainsFinalizationFence(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	previousRemove := repoGoneRemoveDirectory
	previousTimeout := repoGoneCleanupTimeout
	started := make(chan struct{})
	release := make(chan struct{})
	workerFinished := make(chan struct{})
	repoGoneCleanupTimeout = 25 * time.Millisecond
	repoGoneRemoveDirectory = func(path string, _ pathIdentity, _ string, checkpoint func(string) error) error {
		defer close(workerFinished)
		secured := path + "-secured"
		if err := os.Rename(path, secured); err != nil {
			return err
		}
		if err := checkpoint(secured); err != nil {
			return err
		}
		close(started)
		<-release
		return os.RemoveAll(secured)
	}
	t.Cleanup(func() {
		repoGoneRemoveDirectory = previousRemove
		repoGoneCleanupTimeout = previousTimeout
	})

	type result struct {
		state CleanupState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := gw.CleanupClaimedRepoGone(claim)
		done <- result{state: state, err: err}
	}()
	<-started
	observed := <-done
	if observed.state != CleanupStateUnknown || !errors.Is(observed.err, context.DeadlineExceeded) {
		close(release)
		<-workerFinished
		t.Fatalf("live cleanup did not return at its deadline: state=%v err=%v", observed.state, observed.err)
	}
	close(release)
	<-workerFinished

	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupFinalizing {
		t.Fatalf("late live success dropped its durable finalization fence; retained=%v recovery=%+v", retained, recovery)
	}
	retry, err := gw.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("claim late live finalization: %v", err)
	}
	state, err := gw.CleanupClaimedRepoGone(retry)
	if err != nil || state != CleanupSettled {
		t.Fatalf("finalize late live cleanup: state=%v err=%v", state, err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("late live cleanup unexpectedly rematerialized the removed archive: %v", err)
	}
}
