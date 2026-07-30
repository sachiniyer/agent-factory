//go:build linux

package config

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileLockWaitersDoNotEnterThroughReplacedLockFile(t *testing.T) {
	tests := []struct {
		name string
		wait func(string, func() error) error
	}{
		{
			name: "blocking",
			wait: WithFileLock,
		},
		{
			name: "timeout",
			wait: func(path string, fn func() error) error {
				return WithFileLockTimeout(path, 2*time.Second, fn)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			lockPath := path + ".lock"
			oldLock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
			require.NoError(t, err)
			require.NoError(t, syscall.Flock(int(oldLock.Fd()), syscall.LOCK_EX))
			var releaseOld sync.Once
			releaseOldLock := func() {
				releaseOld.Do(func() {
					_ = syscall.Flock(int(oldLock.Fd()), syscall.LOCK_UN)
					_ = oldLock.Close()
				})
			}
			t.Cleanup(releaseOldLock)

			oldInfo, err := oldLock.Stat()
			require.NoError(t, err)
			waiterEntered := make(chan struct{})
			waiterDone := make(chan error, 1)
			go func() {
				waiterDone <- tt.wait(path, func() error {
					close(waiterEntered)
					return nil
				})
			}()

			require.Eventually(t, func() bool {
				return openFileDescriptions(t, oldInfo) >= 2
			}, time.Second, 10*time.Millisecond,
				"the waiter must have opened the old lock inode before it is replaced")

			require.NoError(t, os.Remove(lockPath))
			currentEntered := make(chan struct{})
			releaseCurrent := make(chan struct{})
			var releaseCurrentOnce sync.Once
			releaseCurrentLock := func() { releaseCurrentOnce.Do(func() { close(releaseCurrent) }) }
			t.Cleanup(releaseCurrentLock)
			currentDone := make(chan error, 1)
			go func() {
				currentDone <- WithFileLock(path, func() error {
					close(currentEntered)
					<-releaseCurrent
					return nil
				})
			}()

			select {
			case <-currentEntered:
			case <-time.After(time.Second):
				t.Fatal("replacement lock holder did not enter its critical section")
			}
			releaseOldLock()

			select {
			case <-waiterEntered:
				t.Error("waiter entered through the stale inode while the replacement lock was held")
			case <-time.After(100 * time.Millisecond):
			}

			releaseCurrentLock()
			select {
			case <-waiterEntered:
			case <-time.After(time.Second):
				t.Fatal("waiter did not acquire the current lock after it was released")
			}
			require.NoError(t, <-currentDone)
			require.NoError(t, <-waiterDone)
		})
	}
}

func openFileDescriptions(t *testing.T, want os.FileInfo) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	count := 0
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && os.SameFile(info, want) {
			count++
		}
	}
	return count
}
