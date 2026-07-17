package session

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// DeleteInstanceByStableID is the LAST step of a session kill, and the #1917
// field evidence points straight at it: the daemon's restart log ("tombstoned
// record survived its teardown") proves the kill-intent tombstone reached disk
// and the record delete did not. The delete took a blocking flock, so any other
// af process holding it parked the kill forever — while the kill held the guard
// that makes every retry fail and the session undeletable.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: with config.WithFileLock (no deadline) this
// test hangs until the go test timeout instead of failing its deadline.
func TestDeleteInstanceByStableID_ContendedLock_TimesOutInsteadOfHanging(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const repoID = "deadbeef"
	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, []byte(`[{"title":"doomed"}]`)); err != nil {
		t.Fatalf("seed instances: %v", err)
	}

	prev := InstanceDeleteLockTimeout
	InstanceDeleteLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { InstanceDeleteLockTimeout = prev })

	// Hold the same flock from an independent file description, exactly as a
	// second af process would (flock contends per open-file-description).
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}

	storage, err := NewStorage(config.LoadState(), repoID)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, derr := storage.DeleteInstanceByStableID("doomed", "")
		done <- derr
	}()

	select {
	case derr := <-done:
		if !errors.Is(derr, config.ErrLockTimeout) {
			t.Fatalf("delete must fail with a retryable ErrLockTimeout, got: %v", derr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HUNG: DeleteInstanceByStableID never returned against a held instances lock — " +
			"this is the #1917 wedge: the kill holds killsInFlight across this call, so the " +
			"session becomes undeletable until the daemon restarts")
	}

	// Releasing the contention must make the very same delete succeed: the bound
	// has to leave a RETRYABLE state, not a poisoned one.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	deleted, err := storage.DeleteInstanceByStableID("doomed", "")
	if err != nil {
		t.Fatalf("delete after the lock cleared must succeed, got: %v", err)
	}
	if !deleted {
		t.Fatal("delete after the lock cleared reported nothing deleted")
	}
}
