package session

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceStorage is a simple in-memory implementation of config.InstanceStorage.
type mockInstanceStorage struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
}

func newMockStorage() *mockInstanceStorage {
	return &mockInstanceStorage{data: make(map[string]json.RawMessage)}
}

func (m *mockInstanceStorage) SaveInstances(repoID string, instancesJSON json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[repoID] = instancesJSON
	return nil
}

func (m *mockInstanceStorage) GetInstances(repoID string) json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[repoID]
}

func (m *mockInstanceStorage) GetAllInstances() map[string]json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]json.RawMessage, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out
}

func (m *mockInstanceStorage) DeleteAllInstances() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = make(map[string]json.RawMessage)
	return nil
}

// helper to seed disk state for a specific repo.
func seedDisk(t *testing.T, ms *mockInstanceStorage, repoPath string, instances []InstanceData) {
	t.Helper()
	rid := config.RepoIDFromRoot(repoPath)
	b, err := json.Marshal(instances)
	require.NoError(t, err)
	ms.data[rid] = b
}

// helper to read back disk state for a repo.
func readDisk(t *testing.T, ms *mockInstanceStorage, repoPath string) []InstanceData {
	t.Helper()
	rid := config.RepoIDFromRoot(repoPath)
	raw := ms.data[rid]
	if raw == nil {
		return nil
	}
	var out []InstanceData
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// makeInstance creates a minimal Instance for testing.
// started controls whether the instance appears started.
func makeInstance(title, repoPath string, started bool) *Instance {
	i := &Instance{
		Title:     title,
		Path:      repoPath,
		Status:    Running,
		CreatedAt: time.Now(),
		backend:   &LocalBackend{},
		started:   started,
	}
	return i
}

func TestDaemonSavePreservesExternalInstances(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	ms := newMockStorage()

	// Seed disk with two instances: A (daemon-loaded) and B (added externally).
	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "instance-A", Path: repoPath},
		{Title: "instance-B", Path: repoPath},
	})

	// The daemon only knows about instance-A (loaded at startup).
	instanceA := makeInstance("instance-A", repoPath, true)

	storage, err := NewStorage(ms, "") // daemon mode (empty repoID)
	require.NoError(t, err)

	// Save: daemon has only instance-A in memory.
	err = storage.SaveInstances([]*Instance{instanceA})
	require.NoError(t, err)

	// Verify: both A and B should be on disk.
	result := readDisk(t, ms, repoPath)
	titles := make(map[string]bool)
	for _, d := range result {
		titles[d.Title] = true
	}
	assert.True(t, titles["instance-A"], "in-memory instance should be saved")
	assert.True(t, titles["instance-B"], "externally-added instance should be preserved")
}

func TestDaemonSaveRemovesKilledInstances(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	ms := newMockStorage()

	// Seed disk with instances A and B (both known to daemon).
	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "instance-A", Path: repoPath},
		{Title: "instance-B", Path: repoPath},
	})

	// The daemon knows about both, but B was killed (started=false).
	instanceA := makeInstance("instance-A", repoPath, true)
	instanceB := makeInstance("instance-B", repoPath, false) // killed

	storage, err := NewStorage(ms, "") // daemon mode
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{instanceA, instanceB})
	require.NoError(t, err)

	// Verify: only A should remain; B was killed and should be removed.
	result := readDisk(t, ms, repoPath)
	titles := make(map[string]bool)
	for _, d := range result {
		titles[d.Title] = true
	}
	assert.True(t, titles["instance-A"], "started instance should be saved")
	assert.False(t, titles["instance-B"], "killed instance should not be preserved")
}

func TestDaemonSaveMergesCorrectly(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	ms := newMockStorage()

	// Disk has: A (daemon-known), B (external), C (daemon-known, will be killed).
	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "instance-A", Path: repoPath, Branch: "old-branch-a"},
		{Title: "instance-B", Path: repoPath, Branch: "branch-b"},
		{Title: "instance-C", Path: repoPath, Branch: "branch-c"},
	})

	// Daemon memory: A (started, updated), C (killed).
	instanceA := makeInstance("instance-A", repoPath, true)
	instanceA.Branch = "new-branch-a"                        // updated in memory
	instanceC := makeInstance("instance-C", repoPath, false) // killed

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{instanceA, instanceC})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	titleMap := make(map[string]InstanceData)
	for _, d := range result {
		titleMap[d.Title] = d
	}

	// A should be present with updated data from memory.
	assert.Contains(t, titleMap, "instance-A")
	// B should be preserved (external).
	assert.Contains(t, titleMap, "instance-B")
	assert.Equal(t, "branch-b", titleMap["instance-B"].Branch)
	// C should be gone (killed).
	assert.NotContains(t, titleMap, "instance-C")
}

func TestDaemonSaveDoesNotTouchUnknownRepos(t *testing.T) {
	const repoPath1 = "/tmp/repo1"
	const repoPath2 = "/tmp/repo2"
	ms := newMockStorage()

	// Seed disk with instances in two different repos.
	seedDisk(t, ms, repoPath1, []InstanceData{
		{Title: "instance-A", Path: repoPath1},
	})
	seedDisk(t, ms, repoPath2, []InstanceData{
		{Title: "instance-X", Path: repoPath2},
	})

	// Daemon only knows about repo1.
	instanceA := makeInstance("instance-A", repoPath1, true)

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{instanceA})
	require.NoError(t, err)

	// Repo2 should be untouched.
	result2 := readDisk(t, ms, repoPath2)
	require.Len(t, result2, 1)
	assert.Equal(t, "instance-X", result2[0].Title)

	// Repo1 should have instance-A.
	result1 := readDisk(t, ms, repoPath1)
	require.Len(t, result1, 1)
	assert.Equal(t, "instance-A", result1[0].Title)
}

func TestDaemonSaveEmptyDisk(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	ms := newMockStorage()

	// No existing disk state.
	instanceA := makeInstance("instance-A", repoPath, true)

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{instanceA})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	require.Len(t, result, 1)
	assert.Equal(t, "instance-A", result[0].Title)
}

// TestDaemonSaveCrossRepoTitleCollision verifies that when two repos have
// instances with the same title, saving does not drop an externally-added
// instance from one repo just because the daemon knows about a same-titled
// instance in another repo. Regression test for #198.
func TestDaemonSaveCrossRepoTitleCollision(t *testing.T) {
	const repoPathA = "/tmp/repo-a"
	const repoPathB = "/tmp/repo-b"
	ms := newMockStorage()

	// Repo A has instance "shared" known to the daemon.
	// Repo B has instance "shared" added externally (NOT known to the daemon).
	seedDisk(t, ms, repoPathA, []InstanceData{
		{Title: "shared", Path: repoPathA, Branch: "branch-a"},
	})
	seedDisk(t, ms, repoPathB, []InstanceData{
		{Title: "shared", Path: repoPathB, Branch: "branch-b"},
	})

	// Daemon knows about repo A's "shared" and also has some other instance
	// in repo B so that repo B is a known repo (forcing SaveInstances to
	// visit repo B).
	instanceAShared := makeInstance("shared", repoPathA, true)
	instanceAShared.Branch = "branch-a"
	instanceBOther := makeInstance("other-b", repoPathB, true)

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{instanceAShared, instanceBOther})
	require.NoError(t, err)

	// Repo A: "shared" should be present (in-memory copy).
	resultA := readDisk(t, ms, repoPathA)
	titlesA := make(map[string]bool)
	for _, d := range resultA {
		titlesA[d.Title] = true
	}
	assert.True(t, titlesA["shared"], "repo A's shared instance should be preserved")

	// Repo B: BOTH "shared" (externally added) AND "other-b" (in-memory)
	// should be present. Before the fix, "shared" would be dropped from
	// repo B because the global allInMemoryTitles set contained "shared"
	// (from repo A).
	resultB := readDisk(t, ms, repoPathB)
	titlesB := make(map[string]bool)
	for _, d := range resultB {
		titlesB[d.Title] = true
	}
	assert.True(t, titlesB["other-b"], "repo B's in-memory instance should be saved")
	assert.True(t, titlesB["shared"], "repo B's externally-added instance with title colliding with a different repo's daemon instance must be preserved")
}

func TestDaemonSaveNoInstances(t *testing.T) {
	ms := newMockStorage()

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	// Saving empty list should succeed without error.
	err = storage.SaveInstances([]*Instance{})
	require.NoError(t, err)
}
