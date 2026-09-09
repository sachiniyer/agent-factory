//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHookProgressRecoveryWritesAreBounded(t *testing.T) {
	tests := []struct {
		name  string
		stall func(t *testing.T, entered, release, drained chan struct{})
	}{
		{name: "temporary receipt directory", stall: stallHookProgressFailureMkdir},
		{name: "launch-failed marker staging", stall: stallHookProgressFailureFirstMarker},
		{name: "exit marker staging", stall: stallHookProgressFailureSecondMarker},
		{name: "receipt claim rename", stall: stallHookProgressFailureRename},
		{name: "failed staging cleanup", stall: stallHookProgressFailureCleanup},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &hookProgress{Directory: t.TempDir()}
			previousTimeout := relocationIdentityTimeout
			relocationIdentityTimeout = 50 * time.Millisecond
			entered := make(chan struct{})
			release := make(chan struct{})
			drained := make(chan struct{})
			test.stall(t, entered, release, drained)
			t.Cleanup(func() { relocationIdentityTimeout = previousTimeout })

			result := make(chan bool, 1)
			go func() { result <- p.recordLaunchFailure(context.Background(), 0, errors.New("launcher failed")) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("recovery write did not reach stalled filesystem operation")
			}
			returned := false
			select {
			case handled := <-result:
				returned = true
				if handled {
					t.Fatal("inconclusive recovery write reported a terminal claim")
				}
			case <-time.After(3 * relocationIdentityTimeout):
				t.Error("recovery write did not return at its filesystem bound")
			}
			close(release)
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("stalled recovery write did not drain")
			}
			waitForHookEntryRecoveryWriteFlight(t, p.receipt(0))
			if !returned {
				select {
				case handled := <-result:
					if handled {
						t.Fatal("inconclusive recovery write reported a terminal claim")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("recovery write did not return after the filesystem operation drained")
				}
			}
		})
	}
}

func stallHookProgressFailureMkdir(t *testing.T, entered, release, drained chan struct{}) {
	t.Helper()
	original := hookProgressMkdirTemp
	var once sync.Once
	hookProgressMkdirTemp = func(dir, pattern string) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		defer close(drained)
		return original(dir, pattern)
	}
	t.Cleanup(func() { hookProgressMkdirTemp = original })
}

func stallHookProgressFailureFirstMarker(t *testing.T, entered, release, drained chan struct{}) {
	t.Helper()
	stallHookProgressFailureMarker(t, entered, release, drained, 1)
}

func stallHookProgressFailureSecondMarker(t *testing.T, entered, release, drained chan struct{}) {
	t.Helper()
	stallHookProgressFailureMarker(t, entered, release, drained, 2)
}

func stallHookProgressFailureMarker(t *testing.T, entered, release, drained chan struct{}, target int32) {
	t.Helper()
	original := hookProgressWriteFile
	var calls atomic.Int32
	hookProgressWriteFile = func(path string, data []byte, mode os.FileMode) error {
		call := calls.Add(1)
		if call == target {
			close(entered)
			<-release
		}
		err := original(path, data, mode)
		if call == 2 {
			close(drained)
		}
		return err
	}
	t.Cleanup(func() { hookProgressWriteFile = original })
}

func stallHookProgressFailureRename(t *testing.T, entered, release, drained chan struct{}) {
	t.Helper()
	original := hookProgressRenameNoReplace
	hookProgressRenameNoReplace = func(oldPath, newPath string) error {
		close(entered)
		<-release
		defer close(drained)
		return original(oldPath, newPath)
	}
	t.Cleanup(func() { hookProgressRenameNoReplace = original })
}

func stallHookProgressFailureCleanup(t *testing.T, entered, release, drained chan struct{}) {
	t.Helper()
	originalWrite := hookProgressWriteFile
	originalRemove := hookProgressRemoveAll
	hookProgressWriteFile = func(string, []byte, os.FileMode) error { return errors.New("marker write failed") }
	hookProgressRemoveAll = func(path string) error {
		close(entered)
		<-release
		defer close(drained)
		return originalRemove(path)
	}
	t.Cleanup(func() {
		hookProgressWriteFile = originalWrite
		hookProgressRemoveAll = originalRemove
	})
}

func TestHookProgressAbandonedClaimMarkersAreBounded(t *testing.T) {
	for _, test := range []struct {
		name   string
		target int32
	}{
		{name: "launch-failed marker", target: 1},
		{name: "exit marker", target: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &hookProgress{Directory: t.TempDir(), Prefix: "af-hook-owner"}
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			originalWrite := hookProgressWriteFile
			originalProbe := runningHookPrefixesForResume
			previousTimeout := relocationIdentityTimeout
			relocationIdentityTimeout = 50 * time.Millisecond
			entered := make(chan struct{})
			release := make(chan struct{})
			drained := make(chan struct{})
			var calls atomic.Int32
			hookProgressWriteFile = func(path string, data []byte, mode os.FileMode) error {
				call := calls.Add(1)
				if call == test.target {
					close(entered)
					<-release
				}
				err := originalWrite(path, data, mode)
				if call == 2 {
					close(drained)
				}
				return err
			}
			runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, nil }
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				select {
				case <-drained:
				case <-time.After(5 * time.Second):
					t.Error("abandoned-claim marker write did not drain")
				}
				hookProgressWriteFile = originalWrite
				runningHookPrefixesForResume = originalProbe
				relocationIdentityTimeout = previousTimeout
			})

			result := make(chan bool, 1)
			go func() { result <- p.terminalizeInactiveClaim(context.Background(), 0, errors.New("claimant gone")) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("terminalization did not reach stalled marker write")
			}
			returned := false
			select {
			case terminal := <-result:
				returned = true
				if terminal {
					t.Fatal("inconclusive marker write reported a terminal claim")
				}
			case <-time.After(3 * relocationIdentityTimeout):
				t.Error("abandoned-claim marker write did not return at its filesystem bound")
			}
			close(release)
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("abandoned-claim marker write did not drain")
			}
			waitForHookEntryRecoveryWriteFlight(t, p.receipt(0))
			if !returned {
				select {
				case terminal := <-result:
					if terminal {
						t.Fatal("inconclusive marker write reported a terminal claim")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("terminalization did not return after the filesystem operation drained")
				}
			}
		})
	}
}

func waitForHookEntryRecoveryWriteFlight(t *testing.T, receipt string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hookEntryRecoveryWriteFlights.Lock()
		active := hookEntryRecoveryWriteFlights.byReceipt[receipt]
		hookEntryRecoveryWriteFlights.Unlock()
		if active == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("hook entry recovery write flight did not drain")
		}
		time.Sleep(time.Millisecond)
	}
}
