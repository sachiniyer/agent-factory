package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
	workerFinished := make(chan struct{}, 1)
	var starts atomic.Int32
	cleanupGenerationInstall = func(string, pathIdentity) (string, error) {
		starts.Add(1)
		defer func() { workerFinished <- struct{}{} }()
		close(started)
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
		<-workerFinished
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

func TestCleanupGenerationFromFile_RejectsShrinkingValue(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cleanup directory: %v", err)
	}
	defer directory.Close()
	previousRead := cleanupGenerationRead
	reads := 0
	cleanupGenerationRead = func(int, string, []byte) (int, error) {
		reads++
		if reads == 1 {
			return 32, nil
		}
		return 0, nil
	}
	t.Cleanup(func() { cleanupGenerationRead = previousRead })

	if _, err := cleanupGenerationFromFile(directory); err == nil {
		t.Fatal("a cleanup generation which shrank between reads was accepted")
	}
}

func TestBoundedRepoGoneOriginProbe_UnpublishesCompletedFlightBeforeWake(t *testing.T) {
	gw := &GitWorktree{repoPath: filepath.Join(t.TempDir(), "origin")}
	previousProbe := repoGoneOriginProbe
	previousTimeout := relocationIdentityTimeout
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	repoGoneOriginProbe = func(context.Context, *GitWorktree) error {
		started <- struct{}{}
		<-release
		return nil
	}
	relocationIdentityTimeout = time.Second
	t.Cleanup(func() {
		repoGoneOriginProbe = previousProbe
		relocationIdentityTimeout = previousTimeout
	})

	result := make(chan error, 1)
	go func() { result <- boundedRepoGoneOriginProbe(gw) }()
	<-started
	repoGoneOriginProbeFlights.Lock()
	close(release)
	select {
	case err := <-result:
		repoGoneOriginProbeFlights.Unlock()
		t.Fatalf("probe woke its caller before removing the completed process fence: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	repoGoneOriginProbeFlights.Unlock()

	if err := <-result; err != nil {
		t.Fatalf("completed origin probe failed after its fence was unpublished: %v", err)
	}
	if err := boundedRepoGoneOriginProbe(gw); err != nil {
		t.Fatalf("immediate authoritative recheck reused a completed flight: %v", err)
	}
}

func TestBoundedRepoGoneOriginProbe_SharesHealthyActiveFlight(t *testing.T) {
	gw := &GitWorktree{repoPath: filepath.Join(t.TempDir(), "origin")}
	previousProbe := repoGoneOriginProbe
	previousTimeout := relocationIdentityTimeout
	started := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	repoGoneOriginProbe = func(context.Context, *GitWorktree) error {
		starts.Add(1)
		close(started)
		<-release
		return nil
	}
	relocationIdentityTimeout = time.Second
	t.Cleanup(func() {
		repoGoneOriginProbe = previousProbe
		relocationIdentityTimeout = previousTimeout
	})

	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- boundedRepoGoneOriginProbe(gw) }()
	<-started
	go func() { second <- boundedRepoGoneOriginProbe(gw) }()

	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		repoGoneOriginProbeFlights.Lock()
		flight := repoGoneOriginProbeFlights.byPath[gw.repoPath]
		joined := flight != nil && flight.waiters == 2
		repoGoneOriginProbeFlights.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			<-first
			<-second
			t.Fatal("second restore did not join the healthy active origin probe")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first origin probe failed: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("shared origin probe failed: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("two restores started %d origin probes, want one shared flight", got)
	}
}

func TestCheckRepoPresentForRelocation_FiltersAmbientGitDir(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "non-git-origin")
	if err := os.Mkdir(repoPath, 0o755); err != nil {
		t.Fatalf("create non-Git origin: %v", err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "unrelated-git-dir"))

	err := CheckRepoPresentForRelocation(repoPath)
	if !errors.Is(err, ErrRepoGone) {
		t.Fatalf("filtered GIT_DIR obscured a conclusive non-Git origin: %v", err)
	}
}

func TestCheckRepoPresentForRelocation_RejectsAncestorRepository(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "init", root).Run(); err != nil {
		t.Fatalf("create ancestor repository: %v", err)
	}
	repoPath := filepath.Join(root, "recorded-origin")
	if err := os.Mkdir(repoPath, 0o755); err != nil {
		t.Fatalf("create recorded non-Git origin: %v", err)
	}

	err := CheckRepoPresentForRelocation(repoPath)
	if !errors.Is(err, ErrRepoGone) {
		t.Fatalf("ancestor repository replaced the recorded origin classification: %v", err)
	}
}

func TestDefinitiveMissingRepository_IgnoresReasonWordsInsidePath(t *testing.T) {
	probeErr := &exec.ExitError{Stderr: []byte(
		"fatal: cannot change to '/tmp/Not a directory/repo': Permission denied\n",
	)}
	if definitiveMissingRepository(probeErr) {
		t.Fatal("a reason phrase inside the quoted pathname authorized deletion despite the terminal permission error")
	}
}
