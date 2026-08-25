package upgradetxn

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func lockTestExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(path, []byte("af binary"), 0o755))
	return path
}

// The lock is keyed to the executable, beside the executable — the only location
// derivable from the binary alone, which is what makes it work across AF homes.
func TestExecutableLockPath(t *testing.T) {
	require.Equal(t,
		filepath.Join("/usr/local/bin", ".af.af-upgrade.lock"),
		executableLockPath("/usr/local/bin/af"))

	// It must not collide with the staged artifacts in the same directory: the
	// installer's cross-home check scans for ".<base>.af-upgrade-<id>.previous",
	// and a lock that matched that shape would look like a live transaction.
	previous, candidate := binaryArtifactPaths("/usr/local/bin/af", "upgrade-abc")
	lock := executableLockPath("/usr/local/bin/af")
	require.NotEqual(t, previous, lock)
	require.NotEqual(t, candidate, lock)
	require.False(t, strings.HasPrefix(filepath.Base(lock), ".af.af-upgrade-"),
		"the lock must not match the artifact prefix the interlock scans for")
}

// The whole point: two DIFFERENT AF homes writing the same binary exclude each
// other. A per-home lock cannot do this, which is the gap this closes.
func TestWithInstallLock_ExcludesAcrossDifferentHomes(t *testing.T) {
	executable := lockTestExecutable(t)
	homeA, homeB := t.TempDir(), t.TempDir()

	inA := make(chan struct{})
	releaseA := make(chan struct{})
	doneA := make(chan error, 1)
	go func() {
		doneA <- WithInstallLock(homeA, executable, func() error {
			close(inA)
			<-releaseA
			return nil
		})
	}()
	<-inA

	entered := make(chan struct{})
	doneB := make(chan error, 1)
	go func() {
		doneB <- WithInstallLock(homeB, executable, func() error {
			close(entered)
			return nil
		})
	}()

	select {
	case <-entered:
		t.Fatal("a second AF home entered the critical section while another held the executable lock")
	case <-time.After(250 * time.Millisecond):
	}

	close(releaseA)
	require.NoError(t, <-doneA)
	select {
	case err := <-doneB:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the second home never acquired the lock after the first released it")
	}
	<-entered
}

// Different executables must not block each other — the lock is per binary, not
// a global mutex over all upgrades on the box.
func TestWithInstallLock_DoesNotSerialiseUnrelatedExecutables(t *testing.T) {
	first := lockTestExecutable(t)
	second := lockTestExecutable(t)
	home := t.TempDir()

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = WithInstallLock(home, first, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	done := make(chan error, 1)
	go func() {
		// A different home too, so only the executable identity can be what
		// separates them.
		done <- WithInstallLock(t.TempDir(), second, func() error { return nil })
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("an unrelated executable was blocked by another executable's lock")
	}
	close(release)
}

// Prepare takes the same executable lock, so a transaction cannot publish while
// an in-place installer holds it — the cross-home half of the interlock.
func TestPrepare_WaitsForTheExecutableLock(t *testing.T) {
	executable := lockTestExecutable(t)
	installerHome := t.TempDir()
	daemonHome := t.TempDir()

	held := make(chan struct{})
	release := make(chan struct{})
	installerDone := make(chan error, 1)
	go func() {
		installerDone <- WithInstallLock(installerHome, executable, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	prepared := make(chan error, 1)
	go func() {
		_, err := Prepare(Plan{
			ID:             "upgrade-" + strings.Repeat("b", 32),
			HomeDir:        daemonHome,
			ExecutablePath: executable,
			FromVersion:    "1.0.100",
			ToVersion:      "1.0.300",
			Candidate:      []byte("candidate-af-binary"),
			Daemon: DaemonSnapshot{
				WasRunning: true,
				BootID:     "boot",
				Owner:      DaemonOwner{Kind: SupervisionAdHoc},
			},
			RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
		})
		prepared <- err
	}()

	select {
	case err := <-prepared:
		t.Fatalf("Prepare published while an in-place install held the executable lock (err=%v)", err)
	case <-time.After(250 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-installerDone)
	select {
	case err := <-prepared:
		require.NoError(t, err, "Prepare must succeed once the executable lock is free")
	case <-time.After(20 * time.Second):
		t.Fatal("Prepare never completed after the executable lock was released")
	}
}

// One acquisition order everywhere (home lock, then executable lock) is what
// keeps two homes racing one binary from deadlocking. Drive both directions
// concurrently and require both to finish.
func TestWithInstallLock_OppositeHomesDoNotDeadlock(t *testing.T) {
	executable := lockTestExecutable(t)
	homeA, homeB := t.TempDir(), t.TempDir()

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		home := homeA
		if i%2 == 1 {
			home = homeB
		}
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			errs <- WithInstallLock(h, executable, func() error { return nil })
		}(home)
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("interleaved acquisitions from two homes deadlocked")
	}
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// The lock lives outside the 0700 AF home, in whatever directory the binary is
// installed to, and that can be group-writable. A symlink planted at the lock
// path must not be followed into a file this never meant to touch.
func TestWithExecutableLock_RefusesToFollowASymlink(t *testing.T) {
	executable := lockTestExecutable(t)
	elsewhere := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.Symlink(elsewhere, executableLockPath(executable)))

	err := withExecutableLock(executable, false, func() error {
		t.Fatal("the critical section must not run when the lock path is a symlink")
		return nil
	})
	require.Error(t, err)
	require.ErrorIs(t, err, syscall.ELOOP)

	_, statErr := os.Stat(elsewhere)
	require.True(t, os.IsNotExist(statErr), "the symlink target must never be created")
}

// The lock file is deliberately left behind — removing one another process may
// hold is a race — and it must be private.
func TestWithExecutableLock_LeavesAPrivateLockFile(t *testing.T) {
	// State the directory posture rather than inheriting it (#3465). The lock's
	// audience is DERIVED from the directory's, so a test that does not set the
	// directory is not testing a branch — it is testing whoever's umask ran the
	// suite. sharedInstallDir is the fixture that sets a posture and then proves
	// it; its own comment names this same umask trap.
	//
	// The mechanism, precisely, because "MkdirTemp is 0700" makes it look
	// impossible: t.TempDir() is TWO steps. It calls os.MkdirTemp (0700, umask
	// irrelevant) and then creates the numbered subdirectory it hands back with
	// os.Mkdir(dir, 0777) — and THAT one the umask reduces. Measured: 002 yields
	// 0775, which is group rwx without other-write, exactly what
	// directoryWriterGroup classifies as shared. 022 yields 0755 and 077 yields
	// 0700, both private, which is why this assertion passed everywhere else.
	//
	// The widened counterpart is TestExecutableLock_SharedWithTheDirectorysWriters
	// in executable_lock_sharing_test.go, which already pins the 0660 branch from
	// the same fixture — this is the private half it was missing.
	executable := sharedInstallDir(t, 0o700)
	_, shared := directoryWriterGroup(filepath.Dir(executable))
	require.False(t, shared, "precondition: a privately-owned install directory")

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	info, err := os.Stat(executableLockPath(executable))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(journalFileMode), info.Mode().Perm())
}
