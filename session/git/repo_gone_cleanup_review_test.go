package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
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

	removeTreeAfterEntryClaim = previousAfterClaim
	if err := os.Remove(filepath.Join(recovery.AlternatePath, "late-work.txt")); err != nil {
		t.Fatalf("empty secured root for retry: %v", err)
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

func TestPrepareRelocationClaimForCleanup_BoundsGenerationInstall(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "archive")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("create archive: %v", err)
	}
	identity, err := inspectRelocationPathIdentity(worktree)
	if err != nil {
		t.Fatalf("inspect archive: %v", err)
	}
	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "missing-repo"), worktree, "repo-gone", "af/repo-gone", "", false, true,
	)
	if err != nil {
		t.Fatalf("restore worktree: %v", err)
	}

	previousInstall := cleanupGenerationInstall
	previousTimeout := relocationIdentityTimeout
	release := make(chan struct{})
	started := make(chan struct{})
	workerFinished := make(chan struct{}, 2)
	var starts atomic.Int32
	cleanupGenerationInstall = func(string, pathIdentity) (string, error) {
		starts.Add(1)
		defer func() { workerFinished <- struct{}{} }()
		if starts.Load() == 1 {
			close(started)
		}
		<-release
		return "late-generation", nil
	}
	relocationIdentityTimeout = 25 * time.Millisecond
	t.Cleanup(func() {
		cleanupGenerationInstall = previousInstall
		relocationIdentityTimeout = previousTimeout
	})

	result := make(chan error, 1)
	go func() {
		result <- gw.PrepareRelocationClaimForCleanup(RelocationClaim{Path: worktree, identity: identity})
	}()
	<-started
	select {
	case err := <-result:
		if !errors.Is(err, ErrRelocateStateUnknown) {
			t.Fatalf("generation timeout must fail closed; err=%v", err)
		}
	case <-time.After(150 * time.Millisecond):
		close(release)
		<-result
		t.Fatal("generation installation exceeded its outer deadline")
	}
	if _, retryErr := boundedCleanupGenerationInstall(worktree, identity); !errors.Is(retryErr, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("retry did not observe the timed-out generation process fence: %v", retryErr)
	}
	if got := starts.Load(); got != 1 {
		close(release)
		for range got {
			<-workerFinished
		}
		t.Fatalf("generation deadline spawned %d installers, want one process fence", got)
	}
	close(release)
	<-workerFinished
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryClaimStale {
		t.Fatalf("generation timeout lost its unresolved record; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestInstallCleanupGeneration_DoesNotOverwriteNewerGeneration(t *testing.T) {
	worktree := t.TempDir()
	identity, err := inspectRelocationPathIdentity(worktree)
	if err != nil {
		t.Fatalf("inspect archive: %v", err)
	}
	directory, err := os.Open(worktree)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer directory.Close()
	const newerGeneration = "0123456789abcdef0123456789abcdef"
	if err := unix.Fsetxattr(int(directory.Fd()), cleanupGenerationXattr, []byte(newerGeneration), 0); err != nil {
		t.Fatalf("install newer cleanup generation: %v", err)
	}

	observed, err := installCleanupGeneration(worktree, identity)
	if err != nil {
		t.Fatalf("late generation installer must adopt the winner: %v", err)
	}
	if observed != newerGeneration {
		t.Fatalf("late generation installer overwrote a newer generation: got %q want %q", observed, newerGeneration)
	}
	stored, err := cleanupGenerationFromFile(directory)
	if err != nil {
		t.Fatalf("read cleanup generation: %v", err)
	}
	if stored != newerGeneration {
		t.Fatalf("stale installer changed durable generation: got %q want %q", stored, newerGeneration)
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
