package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func repoGoneCleanupClaim(t *testing.T) (*GitWorktree, RelocationClaim, string) {
	t.Helper()
	root := t.TempDir()
	worktree := filepath.Join(root, "archived")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("create archived worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "work.txt"), []byte("user work"), 0o644); err != nil {
		t.Fatalf("write archived worktree: %v", err)
	}
	info, err := os.Stat(worktree)
	if err != nil {
		t.Fatalf("stat archived worktree: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("archived worktree stat has no syscall identity")
	}
	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "missing-repo"), worktree, "repo-gone", "af/repo-gone", "", false, true,
	)
	if err != nil {
		t.Fatalf("restore worktree handle: %v", err)
	}
	if err := gw.RestoreRelocationRecovery(RelocationRecovery{
		State:         RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}); err != nil {
		t.Fatalf("restore unresolved identity: %v", err)
	}
	claim, err := gw.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("claim unresolved source: %v", err)
	}
	if err := gw.PrepareRelocationClaimForCleanup(claim); err != nil {
		t.Fatalf("prepare cleanup-ready identity: %v", err)
	}
	claim, err = gw.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("claim cleanup-ready source: %v", err)
	}
	return gw, claim, worktree
}

func TestCleanupClaimedRepoGone_RefusesSamePathReplacementAtRecursiveDelete(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	relocated := worktree + "-relocated"
	repoGoneBeforeRecursiveDelete = func(path string) {
		if err := os.Rename(path, relocated); err != nil {
			t.Errorf("move claimed archive at delete boundary: %v", err)
			return
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Errorf("install same-path replacement: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(path, "replacement.txt"), []byte("not ours"), 0o644); err != nil {
			t.Errorf("write same-path replacement: %v", err)
		}
	}
	t.Cleanup(func() { repoGoneBeforeRecursiveDelete = func(string) {} })

	state, err := gw.CleanupClaimedRepoGone(claim)
	if _, statErr := os.Stat(filepath.Join(worktree, "replacement.txt")); statErr != nil {
		t.Fatalf("cleanup deleted a directory that replaced the claimed archive after validation: %v", statErr)
	}
	if state != CleanupStateUnknown || err == nil {
		t.Fatalf("replacement must fail closed with retained recovery; state=%v err=%v", state, err)
	}
	if _, retained := gw.GetRelocationRecovery(); !retained {
		t.Fatal("replacement refusal lost the recovery claim")
	}
}

func TestCleanupClaimedRepoGone_RetainedTreeDeletionSharesDeadline(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	previousCleanup := repoGoneCleanupRetainedTrees
	previousTimeout := repoGoneCleanupTimeout
	started := make(chan struct{})
	release := make(chan struct{})
	repoGoneCleanupTimeout = 25 * time.Millisecond
	repoGoneCleanupRetainedTrees = func(*GitWorktree) error {
		close(started)
		<-release
		return errors.New("retained tree remained unavailable")
	}
	t.Cleanup(func() {
		repoGoneCleanupRetainedTrees = previousCleanup
		repoGoneCleanupTimeout = previousTimeout
	})

	type result struct {
		state CleanupState
		err   error
		late  <-chan error
	}
	done := make(chan result, 1)
	go func() {
		state, err, late := gw.CleanupClaimedRepoGoneWithLateResult(claim)
		done <- result{state: state, err: err, late: late}
	}()
	<-started

	var observed result
	select {
	case observed = <-done:
	case <-time.After(250 * time.Millisecond):
		close(release)
		<-done
		t.Fatal("retained-tree deletion ran outside the repo-gone cleanup deadline")
	}
	if observed.state != CleanupStateUnknown || !errors.Is(observed.err, context.DeadlineExceeded) || observed.late == nil {
		close(release)
		t.Fatalf("retained-tree deadline did not return a retryable late result: state=%v err=%v late=%v", observed.state, observed.err, observed.late)
	}
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupStalled {
		close(release)
		t.Fatalf("retained-tree deadline lost the cleanup process fence: retained=%v recovery=%+v", retained, recovery)
	}
	close(release)
	if err := <-observed.late; err == nil {
		t.Fatal("released retained-tree failure was lost")
	}
}

func TestCleanupClaimedRepoGone_CommittedClaimSurvivesLateOriginReturn(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)

	state, err := gw.CleanupClaimedRepoGone(claim)
	if err != nil || state != CleanupSettled {
		t.Fatalf("an origin return after kill admission must not strand the committed cleanup; state=%v err=%v", state, err)
	}
	if _, statErr := os.Stat(worktree); !os.IsNotExist(statErr) {
		t.Fatalf("committed identity-qualified cleanup left the archive behind: %v", statErr)
	}
}

func TestCleanupClaimedRepoGone_CheckpointsEmptyRootBeforeUnlink(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	checkpointed := false
	restoreCheckpoint := gw.SetRepoGoneFinalizationCheckpoint(func() error {
		checkpointed = true
		recovery, ok := gw.GetRelocationRecovery()
		if !ok || recovery.State != RelocationRecoveryCleanupFinalizing {
			t.Fatalf("checkpoint observed recovery=%+v present=%v, want cleanup_finalizing", recovery, ok)
		}
		entries, err := os.ReadDir(worktree)
		if err != nil {
			t.Fatalf("finalization checkpoint ran after root unlink: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("finalization checkpoint ran before descriptor cleanup emptied the root: %v", entries)
		}
		return errors.New("checkpoint unavailable")
	})
	t.Cleanup(restoreCheckpoint)

	state, err := gw.CleanupClaimedRepoGone(claim)
	if !checkpointed || state != CleanupStateUnknown || err == nil {
		t.Fatalf("failed finalization checkpoint must retain an ordinary retry; checkpointed=%v state=%v err=%v", checkpointed, state, err)
	}
	if _, statErr := os.Stat(worktree); statErr != nil {
		t.Fatalf("failed checkpoint unlinked the only durable retry marker: %v", statErr)
	}
	recovery, ok := gw.GetRelocationRecovery()
	if !ok || recovery.State != RelocationRecoveryCleanupReady {
		t.Fatalf("failed checkpoint did not restore cleanup_ready; present=%v recovery=%+v", ok, recovery)
	}
}

func TestCleanupClaimedRepoGone_FinalizingRetryLeavesReplacement(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	if err := os.Remove(filepath.Join(worktree, "work.txt")); err != nil {
		t.Fatalf("empty root before finalization checkpoint: %v", err)
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
	relocated := worktree + "-relocated"
	if err := os.Rename(worktree, relocated); err != nil {
		t.Fatalf("move empty finalization marker: %v", err)
	}
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("create same-path replacement: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "replacement.txt"), []byte("not ours"), 0o644); err != nil {
		t.Fatalf("write same-path replacement: %v", err)
	}

	if err := finalizing.ValidateRelocationCleanupAdmission(); err != nil {
		t.Fatalf("durable finalization should admit without authorizing the replacement: %v", err)
	}
	finalizingClaim, err := finalizing.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("claim finalization retry: %v", err)
	}
	state, err := finalizing.CleanupClaimedRepoGone(finalizingClaim)
	if err != nil || state != CleanupSettled {
		t.Fatalf("finalization retry did not settle: state=%v err=%v", state, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "replacement.txt")); err != nil {
		t.Fatalf("finalization retry deleted the same-path replacement: %v", err)
	}
}

func TestCleanupClaimedRepoGone_FinalizingRetryRetainsRepopulatedRoot(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	if err := os.Remove(filepath.Join(worktree, "work.txt")); err != nil {
		t.Fatalf("empty root before finalization retry: %v", err)
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
	if err := os.WriteFile(filepath.Join(worktree, "late-work.txt"), []byte("writer survived"), 0o644); err != nil {
		t.Fatalf("repopulate finalizing root: %v", err)
	}
	if err := finalizing.ValidateRelocationCleanupAdmission(); !errors.Is(err, ErrRelocateStateUnknown) {
		t.Fatalf("repopulated finalizing root must be refused before the kill commit: %v", err)
	}
	if recovery, retained := finalizing.GetRelocationRecovery(); !retained || recovery.State != RelocationRecoveryCleanupFinalizing {
		t.Fatalf("pre-commit refusal lost finalizing ownership; retained=%v recovery=%+v", retained, recovery)
	}

	finalizingClaim, err := finalizing.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("claim finalization retry: %v", err)
	}
	state, err := finalizing.CleanupClaimedRepoGone(finalizingClaim)
	if state != CleanupStateUnknown || err == nil {
		t.Fatalf("repopulated finalizing root must remain retryable; state=%v err=%v", state, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "late-work.txt")); err != nil {
		t.Fatalf("finalization retry touched repopulated work: %v", err)
	}
	recovery, retained := finalizing.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupFinalizing {
		t.Fatalf("repopulated root lost its finalization record; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestCleanupClaimedRepoGone_RootUnlinkFailureRestoresFinalizingName(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	previousClaim := removeTreeBeforeEntryClaim
	repopulated := false
	removeTreeBeforeEntryClaim = func(directory *os.File, path string) error {
		if !repopulated && path == filepath.Dir(worktree) {
			repopulated = true
			if err := os.WriteFile(filepath.Join(worktree, "late-work.txt"), []byte("writer survived"), 0o644); err != nil {
				return err
			}
		}
		return previousClaim(directory, path)
	}
	t.Cleanup(func() { removeTreeBeforeEntryClaim = previousClaim })

	state, err := gw.CleanupClaimedRepoGone(claim)
	if !repopulated || state != CleanupStateUnknown || err == nil {
		t.Fatalf("late root entry must make unlink retryable; repopulated=%v state=%v err=%v", repopulated, state, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "late-work.txt")); err != nil {
		t.Fatalf("failed root unlink did not restore the durable finalizing pathname: %v", err)
	}
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupFinalizing {
		t.Fatalf("failed root unlink lost its finalization record; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestCleanupClaimedRepoGone_RemovesRetainedArchiveTreesBeforePrimary(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	retained := filepath.Join(filepath.Dir(worktree), ".af-source-0123456789abcdef0123456789abcdef")
	if err := os.Mkdir(retained, 0o700); err != nil {
		t.Fatalf("create retained archive tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(retained, "private-work.txt"), []byte("retained user work"), 0o600); err != nil {
		t.Fatalf("write retained archive tree: %v", err)
	}
	identity, err := inspectRelocationPathIdentity(retained)
	if err != nil {
		t.Fatalf("inspect retained archive tree: %v", err)
	}
	gw.RestoreArchiveReport(ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(retained, identity, []ArchiveSkippedEntry{
			newArchiveSkippedEntry("private-work.txt", ArchiveSkipPermissionDenied),
		}),
	}})
	hookCanceled := make(chan struct{})
	hooksDone := make(chan struct{})
	gw.hooksCancel = func() { close(hookCanceled) }
	gw.hooksDone = hooksDone
	previousClaim := removeTreeBeforeEntryClaim
	deletionStarted := make(chan struct{}, 1)
	removeTreeBeforeEntryClaim = func(directory *os.File, path string) error {
		if strings.HasPrefix(path, retained) {
			select {
			case deletionStarted <- struct{}{}:
			default:
			}
		}
		return previousClaim(directory, path)
	}
	t.Cleanup(func() { removeTreeBeforeEntryClaim = previousClaim })

	type result struct {
		state CleanupState
		err   error
	}
	completed := make(chan result, 1)
	go func() {
		state, err := gw.CleanupClaimedRepoGone(claim)
		completed <- result{state: state, err: err}
	}()
	<-hookCanceled
	deletedBeforeHookExit := false
	select {
	case <-deletionStarted:
		deletedBeforeHookExit = true
	case <-time.After(100 * time.Millisecond):
	}
	close(hooksDone)
	observed := <-completed
	state, err := observed.state, observed.err
	if err != nil || state != CleanupSettled {
		t.Fatalf("cleanup with retained archive tree did not settle: state=%v err=%v", state, err)
	}
	if deletedBeforeHookExit {
		t.Error("retained-tree deletion started before hook exit was confirmed")
	} else {
		select {
		case <-deletionStarted:
		default:
			t.Error("repo-gone cleanup did not continue after hook exit was confirmed")
		}
	}
	if _, err := os.Stat(retained); !os.IsNotExist(err) {
		t.Fatalf("cleanup orphaned a retained archive tree after removing its durable row: %v", err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("cleanup left the primary archive behind: %v", err)
	}
	if report := gw.GetArchiveReport(); !report.Empty() {
		t.Fatalf("cleanup retained a report after consuming all owned trees: %+v", report)
	}
}

func TestCleanupClaimedRepoGone_RecursiveDeleteDeadlineRetainsClaim(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	previousRemove := repoGoneRemoveDirectory
	previousTimeout := repoGoneCleanupTimeout
	blocked := make(chan struct{})
	workerFinished := make(chan struct{})
	repoGoneCleanupTimeout = 25 * time.Millisecond
	repoGoneRemoveDirectory = func(string, pathIdentity, string, func(string) error) error {
		defer close(workerFinished)
		<-blocked
		return nil
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

	var got result
	select {
	case got = <-done:
	case <-time.After(250 * time.Millisecond):
		close(blocked)
		<-done
		t.Fatal("repo-gone recursive deletion ignored its hard caller deadline")
	}
	if got.state != CleanupStateUnknown || !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("deadline must report unknown cleanup state; state=%v err=%v", got.state, got.err)
	}
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupStalled || !recovery.IdentityKnown {
		t.Fatalf("deadline must retain the identity-qualified cleanup claim; retained=%v recovery=%+v", retained, recovery)
	}
	reloaded, err := NewGitWorktreeFromStorage(
		gw.GetRepoPath(), worktree, "repo-gone", "af/repo-gone", "", false, true,
	)
	if err != nil {
		t.Fatalf("recreate worktree handle after restart: %v", err)
	}
	if err := reloaded.RestoreRelocationRecovery(recovery); err != nil {
		t.Fatalf("reload identity-qualified cleanup stall: %v", err)
	}
	_, restored, ok := reloaded.RelocationSnapshot()
	if !ok || restored.State != RelocationRecoveryCleanupReady {
		t.Fatalf("a fresh daemon must re-arm the exact cleanup obligation; ok=%v recovery=%+v", ok, restored)
	}
	close(blocked)
	<-workerFinished
	if recovery, retained := gw.GetRelocationRecovery(); retained {
		t.Fatalf("late successful descriptor cleanup left a permanent stalled record: %+v", recovery)
	}
}

func TestCleanupClaimedRepoGone_RestartDoesNotTreatAbsentPathAsCompletedDelete(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "renamed-before-restart")
	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "missing-repo"), worktree, "repo-gone", "af/repo-gone", "", false, true,
	)
	if err != nil {
		t.Fatalf("recreate worktree handle after restart: %v", err)
	}
	if err := gw.RestoreRelocationRecovery(RelocationRecovery{
		State:             RelocationRecoveryCleanupStalled,
		IdentityKnown:     true,
		Device:            17,
		Inode:             23,
		FileType:          uint32(syscall.S_IFDIR),
		CleanupGeneration: "0123456789abcdef0123456789abcdef",
	}); err != nil {
		t.Fatalf("reload identity-qualified cleanup stall: %v", err)
	}
	_, recovery, retained := gw.RelocationSnapshot()
	if !retained || recovery.State != RelocationRecoveryCleanupReady {
		t.Fatalf("absent pathname was misread as completed deletion; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestValidateRelocationCleanupAdmission_RepoRecheckDeadlineRetainsAuthorization(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	previousProbe := repoGoneOriginProbe
	previousTimeout := relocationIdentityTimeout
	probeFinished := make(chan struct{})
	relocationIdentityTimeout = 25 * time.Millisecond
	repoGoneOriginProbe = func(ctx context.Context, _ *GitWorktree) error {
		defer close(probeFinished)
		<-ctx.Done()
		return ErrRepoGone
	}
	t.Cleanup(func() {
		repoGoneOriginProbe = previousProbe
		relocationIdentityTimeout = previousTimeout
	})

	done := make(chan error, 1)
	go func() {
		done <- gw.ValidateRelocationCleanupAdmission()
	}()

	select {
	case err := <-done:
		<-probeFinished
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("repo recheck deadline must refuse admission: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("repo-gone origin recheck ignored its hard caller deadline")
	}
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupReady || !recovery.IdentityKnown {
		t.Fatalf("origin recheck deadline lost cleanup authorization; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestValidateRelocationCleanupAdmission_DeadlineCancelsOriginProbeProcess(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	repoPath := gw.GetRepoPath()
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("recreate origin pathname: %v", err)
	}

	binDir := t.TempDir()
	pidPath := filepath.Join(binDir, "probe.pid")
	gitPath := filepath.Join(binDir, "git")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$$\" > %q\nexec /bin/sleep 60\n", pidPath)
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write blocking git probe: %v", err)
	}
	t.Setenv("PATH", binDir)
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 25 * time.Millisecond
	t.Cleanup(func() { relocationIdentityTimeout = previousTimeout })

	err := gw.ValidateRelocationCleanupAdmission()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocking origin probe must hit its admission deadline: %v", err)
	}
	if !waitForFile(t, pidPath, 250*time.Millisecond) {
		t.Fatal("blocking git probe never recorded its process id")
	}
	var pid int
	rawPID, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read blocking git probe pid: %v", err)
	}
	if _, err := fmt.Sscanf(string(rawPID), "%d", &pid); err != nil {
		t.Fatalf("parse blocking git probe pid %q: %v", rawPID, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("origin probe process %d survived after the admission deadline returned", pid)
}

func TestValidateRelocationCleanupAdmission_DeadlineReusesOriginProbeFence(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	previousProbe := repoGoneOriginProbe
	previousTimeout := relocationIdentityTimeout
	started := make(chan struct{}, 2)
	finished := make(chan struct{}, 2)
	release := make(chan struct{})
	repoGoneOriginProbe = func(context.Context, *GitWorktree) error {
		defer func() { finished <- struct{}{} }()
		started <- struct{}{}
		<-release
		return errors.New("probe released")
	}
	relocationIdentityTimeout = 25 * time.Millisecond
	t.Cleanup(func() {
		repoGoneOriginProbe = previousProbe
		relocationIdentityTimeout = previousTimeout
	})

	if err := boundedRepoGoneOriginProbe(gw); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("first attempt did not fail closed at the probe deadline: %v", err)
	}
	reloaded := &GitWorktree{repoPath: gw.repoPath}
	if err := boundedRepoGoneOriginProbe(reloaded); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("retry did not observe the process fence: %v", err)
	}
	got := len(started)
	close(release)
	for range got {
		<-finished
	}
	if got != 1 {
		t.Fatalf("deadline spawned %d origin probes for one stuck operation, want one process fence", got)
	}
}

func TestValidateRelocationCleanupAdmission_NonGitOriginRemainsRepoGone(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	repoPath := gw.GetRepoPath()
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("recreate origin pathname without git metadata: %v", err)
	}

	if err := gw.ValidateRelocationCleanupAdmission(); err != nil {
		t.Fatalf("a non-Git origin is still conclusively repo-gone and must not strand cleanup: %v", err)
	}
}

func TestValidateRelocationCleanupAdmission_FilteredAmbientGitDirDoesNotObscureRepoGone(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	repoPath := gw.GetRepoPath()
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("recreate origin pathname without git metadata: %v", err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "unrelated-git-dir"))

	if err := gw.ValidateRelocationCleanupAdmission(); err != nil {
		t.Fatalf("GIT_DIR filtered from the Git subprocess cannot make a conclusive missing origin unknown: %v", err)
	}
}

func TestValidateRelocationCleanupAdmission_AncestorRepoDoesNotReplaceOrigin(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	root := filepath.Dir(gw.GetRepoPath())
	if err := exec.Command("git", "init", root).Run(); err != nil {
		t.Fatalf("create ancestor repository: %v", err)
	}
	if err := os.MkdirAll(gw.GetRepoPath(), 0o755); err != nil {
		t.Fatalf("create recorded non-Git origin beneath ancestor: %v", err)
	}

	if err := gw.ValidateRelocationCleanupAdmission(); err != nil {
		t.Fatalf("ancestor repository must not be mistaken for the recorded origin: %v", err)
	}
}

func TestValidateRelocationCleanupAdmission_NonDirectoryOriginRemainsRepoGone(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	repoPath := gw.GetRepoPath()
	if err := os.WriteFile(repoPath, []byte("unrelated replacement"), 0o644); err != nil {
		t.Fatalf("replace missing origin with a regular file: %v", err)
	}

	if err := gw.ValidateRelocationCleanupAdmission(); err != nil {
		t.Fatalf("a non-directory origin is conclusively repo-gone and must not strand cleanup: %v", err)
	}
}

func TestValidateRelocationCleanupAdmission_UnreadableGitMetadataFailsClosed(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	repoPath := gw.GetRepoPath()
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		t.Fatalf("create repository metadata: %v", err)
	}
	if err := os.Chmod(filepath.Join(repoPath, ".git"), 0); err != nil {
		t.Fatalf("make repository metadata unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(repoPath, ".git"), 0o755) })

	if err := gw.ValidateRelocationCleanupAdmission(); !errors.Is(err, ErrRelocateStateUnknown) {
		t.Fatalf("unreadable metadata must not be classified as a missing origin, got: %v", err)
	}
	if recovery, retained := gw.GetRelocationRecovery(); !retained || recovery.State != RelocationRecoveryCleanupReady {
		t.Fatalf("an unreadable origin must retain cleanup authorization without consuming it; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestValidateRelocationCleanupAdmission_IdentityTupleAloneFailsClosed(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "replacement")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("create same-tuple replacement fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "user-work.txt"), []byte("not authorized"), 0o644); err != nil {
		t.Fatalf("write replacement fixture: %v", err)
	}
	info, err := os.Stat(worktree)
	if err != nil {
		t.Fatalf("stat replacement fixture: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("replacement stat has no syscall identity")
	}
	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "missing-repo"), worktree, "reused", "af/reused", "", false, true,
	)
	if err != nil {
		t.Fatalf("restore worktree handle: %v", err)
	}
	if err := gw.RestoreRelocationRecovery(RelocationRecovery{
		State: RelocationRecoveryCleanupReady, IdentityKnown: true,
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), FileType: uint32(stat.Mode & syscall.S_IFMT),
	}); err == nil {
		t.Fatal("a reusable inode tuple without a durable generation must fail closed while loading")
	}
	if _, err := os.Stat(filepath.Join(worktree, "user-work.txt")); err != nil {
		t.Fatalf("tuple-only cleanup authority touched replacement work: %v", err)
	}
}

func TestValidateRelocationCleanupAdmission_GitExecutionFailureFailsClosed(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	gw.PreserveRelocationClaim(claim)
	repoPath := gw.GetRepoPath()
	if err := exec.Command("git", "init", repoPath).Run(); err != nil {
		t.Fatalf("create valid returned origin: %v", err)
	}
	t.Setenv("PATH", t.TempDir())

	err := gw.ValidateRelocationCleanupAdmission()
	if err == nil || !errors.Is(err, ErrRelocateStateUnknown) {
		t.Fatalf("an unexecutable Git probe must fail closed, got: %v", err)
	}
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupReady {
		t.Fatalf("an operational probe failure lost cleanup authorization; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestCleanupClaimedRepoGone_AnsweredErrorPreservesCleanupAuthorization(t *testing.T) {
	gw, claim, _ := repoGoneCleanupClaim(t)
	previousRemove := repoGoneRemoveDirectory
	repoGoneRemoveDirectory = func(string, pathIdentity, string, func(string) error) error {
		return errors.New("temporary I/O refusal")
	}
	t.Cleanup(func() { repoGoneRemoveDirectory = previousRemove })

	state, err := gw.CleanupClaimedRepoGone(claim)
	if state != CleanupStateUnknown || err == nil {
		t.Fatalf("answered delete error must retain the cleanup; state=%v err=%v", state, err)
	}
	recovery, retained := gw.GetRelocationRecovery()
	if !retained || recovery.State != RelocationRecoveryCleanupReady {
		t.Fatalf("transient delete error permanently downgraded cleanup authority; retained=%v recovery=%+v", retained, recovery)
	}
}

func TestCleanupClaimedRepoGone_DoesNotReapThroughReplaceablePath(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	relocated := worktree + "-relocated"
	previousBefore := repoGoneBeforeWriterReap
	previousReap := repoGoneReapMatching
	previousOpen := repoGoneOpenWorkingDir
	repoGoneBeforeWriterReap = func(path string) {
		if err := os.Rename(path, relocated); err != nil {
			t.Fatalf("move claimed archive before writer reap: %v", err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("install same-path replacement before writer reap: %v", err)
		}
	}
	reapedReplacement := false
	repoGoneOpenWorkingDir = func(int) (*os.File, string, bool) {
		directory, err := os.Open(worktree)
		return directory, worktree, err == nil
	}
	repoGoneReapMatching = func(_ string, matches func(int) bool) {
		reapedReplacement = matches(1)
	}
	t.Cleanup(func() {
		repoGoneBeforeWriterReap = previousBefore
		repoGoneReapMatching = previousReap
		repoGoneOpenWorkingDir = previousOpen
	})

	_, _ = gw.CleanupClaimedRepoGone(claim)
	if reapedReplacement {
		t.Fatal("repo-gone cleanup reaped processes through a pathname after its claimed directory was replaced")
	}
}
