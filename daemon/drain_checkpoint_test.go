package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #5846: drain guarantees a durable checkpoint inside the stopper's grace
// whatever a joined goroutine does. The checkpoint is taken after admission is
// closed and before anything is joined, and the join itself is bounded.

type checkpointProbe struct {
	mu    sync.Mutex
	calls int
	// stopClosedAtCall records, per call, whether stopCh was already closed: the
	// first checkpoint must precede the join.
	stopClosedAtCall []bool
}

func installCheckpointProbe(t *testing.T, stopCh chan struct{}) *checkpointProbe {
	t.Helper()
	p := &checkpointProbe{}
	prev := checkpointBeforeJoin
	checkpointBeforeJoin = func(*Manager) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls++
		select {
		case <-stopCh:
			p.stopClosedAtCall = append(p.stopClosedAtCall, true)
		default:
			p.stopClosedAtCall = append(p.stopClosedAtCall, false)
		}
		return nil
	}
	t.Cleanup(func() { checkpointBeforeJoin = prev })
	return p
}

func shortenDrainBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := drainJoinBudget
	drainJoinBudget = d
	t.Cleanup(func() { drainJoinBudget = prev })
}

func TestDrainCheckpointsBeforeJoinAndAbandonsAWedgedGoroutine(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	shortenDrainBudget(t, 150*time.Millisecond)

	stopCh := make(chan struct{})
	probe := installCheckpointProbe(t, stopCh)

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

	httpClosed, controlClosed := false, false
	var joined bool
	drained := make(chan struct{})
	go func() {
		joined = drainDaemon(manager, nil, func() error { return nil }, &httpClosed, &controlClosed, stopCh, &workers)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain waited on a wedged joined goroutine past its budget")
	}

	require.False(t, joined, "an abandoned join must be reported so the final save is skipped")
	probe.mu.Lock()
	defer probe.mu.Unlock()
	require.Equal(t, 1, probe.calls, "exactly one checkpoint, taken before the join")
	require.Equal(t, []bool{false}, probe.stopClosedAtCall, "the checkpoint must precede closing stopCh and joining")
}

func TestDrainReportsACompletedJoin(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())

	stopCh := make(chan struct{})
	probe := installCheckpointProbe(t, stopCh)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-stopCh // a well-behaved worker exits when told to stop
	}()

	httpClosed, controlClosed := false, false
	require.True(t, drainDaemon(manager, nil, func() error { return nil }, &httpClosed, &controlClosed, stopCh, &workers))
	probe.mu.Lock()
	defer probe.mu.Unlock()
	require.Equal(t, 1, probe.calls)
}
