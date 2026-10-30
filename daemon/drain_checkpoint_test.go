package daemon

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #5846: drain bounds the join inside the stopper's grace. A join that times
// out writes nothing: a durable write taken while admitted writers can still
// commit races them (see skippedOnJoinTimeout), so a wedged goroutine costs its
// in-flight work and no longer the grace.

func shortenDrainBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := drainJoinBudget
	drainJoinBudget = d
	t.Cleanup(func() { drainJoinBudget = prev })
}

func TestDrainAbandonsAWedgedGoroutineAndWritesNothing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	shortenDrainBudget(t, 150*time.Millisecond)

	// A joined goroutine that never returns on its own: the poll pass, orphan
	// sweep and update-driver lookup are all this shape from the drain's side.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-release
	}()

	// The repo start lock a CreateSession holds across provisioning: drain must
	// not wait on it, because nothing on its path takes it.
	startLock := manager.startLockForRepo("some-repo")
	startLock.Lock()
	t.Cleanup(startLock.Unlock)

	home, err := config.GetConfigDir()
	require.NoError(t, err)
	before := listFiles(t, home)

	httpClosed, controlClosed := false, false
	var joined bool
	drained := make(chan struct{})
	go func() {
		joined = drainDaemon(manager, nil, func() error { return nil }, &httpClosed, &controlClosed, make(chan struct{}), &workers)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain waited on a wedged joined goroutine past its budget")
	}

	require.False(t, joined, "an abandoned join must be reported so the final save is skipped")
	require.Equal(t, before, listFiles(t, home), "a timed-out drain must not write anything")
}

func TestDrainReportsACompletedJoin(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())

	stopCh := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-stopCh // a well-behaved worker exits when told to stop
	}()

	httpClosed, controlClosed := false, false
	require.True(t, drainDaemon(manager, nil, func() error { return nil }, &httpClosed, &controlClosed, stopCh, &workers))
}

func TestSkippedOnJoinTimeoutNamesTheOwedRetries(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	manager.mu.Lock()
	manager.dischargeOwed["r|a"] = dischargeRetryEntry{}
	manager.mu.Unlock()
	require.Equal(t, "0 owed settlement retries and 1 owed stand-down discharge stay as the disk has them", manager.skippedOnJoinTimeout())

	manager.mu.Lock() // held: the log line must not block
	defer manager.mu.Unlock()
	require.Contains(t, manager.skippedOnJoinTimeout(), "without blocking")
}

// listFiles returns path -> size+mtime for every file under dir.
func listFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		info, err := e.Info()
		require.NoError(t, err)
		out[e.Name()] = fmt.Sprintf("%s/%d", info.ModTime(), info.Size())
	}
	return out
}
