package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		State:         RelocationRecoveryCleanupReady,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}); err != nil {
		t.Fatalf("restore cleanup-ready identity: %v", err)
	}
	claim, err := gw.ClaimRelocationSource()
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

func TestCleanupClaimedRepoGone_RecursiveDeleteDeadlineRetainsClaim(t *testing.T) {
	gw, claim, worktree := repoGoneCleanupClaim(t)
	previousRemove := repoGoneRemoveDirectory
	previousTimeout := repoGoneCleanupTimeout
	blocked := make(chan struct{})
	workerFinished := make(chan struct{})
	repoGoneCleanupTimeout = 25 * time.Millisecond
	repoGoneRemoveDirectory = func(string, pathIdentity) error {
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
		State:         RelocationRecoveryCleanupStalled,
		IdentityKnown: true,
		Device:        17,
		Inode:         23,
		FileType:      uint32(syscall.S_IFDIR),
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
	repoGoneRemoveDirectory = func(string, pathIdentity) error { return errors.New("temporary I/O refusal") }
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
