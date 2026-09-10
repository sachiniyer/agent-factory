package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSaveInstancesForShutdown_FencesArchiveAfterFinalSnapshot covers finding
// 3970544710. A control RPC can reach BeginArchive after the checkpoint's
// locked snapshot but before its durable write. The terminal checkpoint must
// either include that archive's settled outcome or refuse to start it.
func TestSaveInstancesForShutdown_FencesArchiveAfterFinalSnapshot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()
	target := &Instance{
		ID: "post-snapshot-id", Title: "post-snapshot", Path: repoPath,
		Program: "claude", started: true, liveness: LiveRunning,
		backend: &dockerBackend{},
	}
	alive := makeAliveInstance("alive", repoPath)
	archiveStarted := false
	var beginErr error
	state.beforeSave = func() {
		beginErr = target.Transition(BeginArchive())
		if beginErr != nil {
			return
		}
		archiveStarted = true
		target.recordArchivePush("af/post-snapshot")
		require.NoError(t, target.Transition(CommitArchive()))
	}

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstancesForShutdown([]*Instance{target, alive}))

	for _, row := range readDisk(t, state, repoPath) {
		if row.ID != target.ID {
			continue
		}
		if archiveStarted {
			if row.Liveness != LiveArchived || row.Branch != "af/post-snapshot" {
				t.Fatalf("archive committed after the final snapshot but the shutdown checkpoint wrote stale state: "+
					"Liveness=%v Branch=%q, want LiveArchived and %q", row.Liveness, row.Branch, "af/post-snapshot")
			}
			return
		}
		require.ErrorContains(t, beginErr, "shutdown checkpoint")
		if row.Liveness != LiveRunning || row.Branch != "" {
			t.Fatalf("checkpoint-fenced archive changed durable state: Liveness=%v Branch=%q", row.Liveness, row.Branch)
		}
		return
	}
	t.Fatal("shutdown checkpoint erased the live session")
}

func TestSaveInstancesForShutdown_WaitsForArchiveAlreadyInFlight(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()
	target := &Instance{
		ID: "in-flight-id", Title: "in-flight", Path: repoPath,
		Program: "claude", started: true, liveness: LiveRunning,
		backend: &dockerBackend{},
	}
	alive := makeAliveInstance("alive", repoPath)
	require.NoError(t, target.Transition(BeginArchive()))

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	saved := make(chan error, 1)
	go func() {
		saved <- storage.SaveInstancesForShutdown([]*Instance{target, alive})
	}()

	require.Eventually(t, func() bool {
		target.mu.Lock()
		defer target.mu.Unlock()
		return target.archiveCheckpointSealed
	}, time.Second, time.Millisecond)
	select {
	case err := <-saved:
		t.Fatalf("shutdown checkpoint returned before the admitted archive settled: %v", err)
	default:
	}

	target.recordArchivePush("af/in-flight")
	require.NoError(t, target.Transition(CommitArchive()))
	require.NoError(t, <-saved)

	for _, row := range readDisk(t, state, repoPath) {
		if row.ID != target.ID {
			continue
		}
		if row.Liveness != LiveArchived || row.Branch != "af/in-flight" {
			t.Fatalf("shutdown checkpoint missed the admitted archive's settled state: "+
				"Liveness=%v Branch=%q, want LiveArchived and %q", row.Liveness, row.Branch, "af/in-flight")
		}
		return
	}
	t.Fatal("shutdown checkpoint erased the settled archive")
}
