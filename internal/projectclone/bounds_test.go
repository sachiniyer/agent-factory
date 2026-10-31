package projectclone

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A git whose child keeps the output pipes open after git is killed must not
// hold the caller past the deadline: the group is killed and the drain is
// bounded (#6094). Without either, this blocks for the child's full sleep.
func TestGitRunnerBoundsAStalledChildHoldingThePipes(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nsleep 120 &\nsleep 120\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := gitRunner(ctx, t.TempDir(), os.Environ(), "clone")
	require.Error(t, err)
	require.Less(t, time.Since(start), waitDelay+5*time.Second, "the drain outlived the deadline")
}

func TestBudgetCoversTheCloneDeadline(t *testing.T) {
	require.Greater(t, Budget, cloneTimeout+originCheckTimeout)
}

// The table lock is bookkeeping only: a clone that is stuck must not delay the
// registration of another repository (#6094).
func TestFlightsDoNotSerializeDifferentDestinations(t *testing.T) {
	var f Flights
	release := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = f.Do(context.Background(), "/r/a", "https://h/o/a", func() (string, error) {
			close(started)
			<-release
			return "/r/a", nil
		})
	}()
	<-started

	done := make(chan string, 1)
	go func() {
		p, _ := f.Do(context.Background(), "/r/b", "https://h/o/b", func() (string, error) { return "/r/b", nil })
		done <- p
	}()
	select {
	case p := <-done:
		require.Equal(t, "/r/b", p)
	case <-time.After(3 * time.Second):
		t.Fatal("a stuck clone blocked an unrelated registration")
	}
	close(release)
	wg.Wait()
}

func TestFlightsShareTheOutcomeOfAnIdenticalCloneAndRefuseAnotherURL(t *testing.T) {
	var f Flights
	release := make(chan struct{})
	started := make(chan struct{})
	runs := 0
	var mu sync.Mutex
	go func() {
		_, _ = f.Do(context.Background(), "/r/x", "https://h/o/x", func() (string, error) {
			mu.Lock()
			runs++
			mu.Unlock()
			close(started)
			<-release
			return "/r/x", nil
		})
	}()
	<-started

	_, err := f.Do(context.Background(), "/r/x", "https://other/o/x", func() (string, error) {
		t.Fatal("ran a clone into a directory another clone is filling")
		return "", nil
	})
	require.ErrorContains(t, err, "being cloned")

	got := make(chan string, 1)
	go func() {
		p, _ := f.Do(context.Background(), "/r/x", "https://h/o/x", func() (string, error) {
			t.Error("a duplicate registration started a second clone")
			return "", nil
		})
		got <- p
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	require.Equal(t, "/r/x", <-got)
	mu.Lock()
	require.Equal(t, 1, runs)
	mu.Unlock()
}

func TestFlightsWaitIsBoundedByTheContext(t *testing.T) {
	var f Flights
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	go func() {
		_, _ = f.Do(context.Background(), "/r/y", "https://h/o/y", func() (string, error) {
			close(started)
			<-release
			return "", nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := f.Do(ctx, "/r/y", "https://h/o/y", func() (string, error) { return "", nil })
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
