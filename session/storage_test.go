package session

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceStorage is a simple in-memory implementation of config.InstanceStorage.
type mockInstanceStorage struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
	// readErr, when non-nil, makes GetInstances fail to simulate a transient
	// read failure (permission denied, I/O error) on instances.json.
	readErr error
	// readAllErr, when non-nil, makes GetAllInstances fail to simulate an
	// unreadable instances directory (permission denied, I/O error).
	readAllErr error
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

func (m *mockInstanceStorage) GetInstances(repoID string) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, m.readErr
	}
	return m.data[repoID], nil
}

func (m *mockInstanceStorage) GetAllInstances() (map[string]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readAllErr != nil {
		return nil, m.readAllErr
	}
	out := make(map[string]json.RawMessage, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out, nil
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
// Its backend is a LocalBackend with no tmux session, so TmuxAlive()
// reports false — the shape of a session that died or was killed.
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

// makeAliveInstance creates a minimal started Instance whose backend reports
// the session alive, for tests modeling live sessions. Live sessions must be
// persisted even when their disk record is missing (#736 territory); only
// dead-AND-deleted instances are dropped by the save merge (#819).
func makeAliveInstance(title, repoPath string) *Instance {
	i := makeInstance(title, repoPath, true)
	i.backend = &FakeBackend{}
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

	// No existing disk state. The instance must be alive to be persisted —
	// a dead instance with no disk record is treated as externally killed (#819).
	instanceA := makeAliveInstance("instance-A", repoPath)

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
	instanceBOther := makeAliveInstance("other-b", repoPathB)

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

// TestRepoSaveAbortsOnReadError is the core regression test for #766. When the
// existing instances.json cannot be read (a transient permission/I/O error, as
// opposed to a missing file), saveRepoInstances must NOT treat disk as empty
// and merge-then-overwrite — that silently and permanently drops sessions that
// are present on disk. The save must surface the error and leave disk untouched.
func TestRepoSaveAbortsOnReadError(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	// Seed disk with a real, present-on-disk session.
	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "on-disk", Path: repoPath, Status: Running},
	})
	// Snapshot the exact bytes so we can prove they are untouched afterwards.
	before := append(json.RawMessage(nil), ms.data[repoID]...)

	// Reads now fail (e.g. EACCES / EIO on instances.json).
	ms.readErr = errors.New("permission denied")

	// The TUI has a different session in memory; a naive merge against an
	// empty disk state would write only this one and erase "on-disk".
	inMem := makeInstance("in-memory", repoPath, true)
	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{inMem})
	require.Error(t, err, "SaveInstances must surface the read error instead of overwriting disk")

	// Disk bytes must be exactly what we seeded — no empty/partial overwrite.
	assert.Equal(t, string(before), string(ms.data[repoID]),
		"on-disk session data must be preserved when the existing file cannot be read (#766)")
}

func TestDaemonSaveNoInstances(t *testing.T) {
	ms := newMockStorage()

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	// Saving empty list should succeed without error.
	err = storage.SaveInstances([]*Instance{})
	require.NoError(t, err)
}

func TestRepoSavePreservesDiskOnlyInstances(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "loaded-in-tui", Path: repoPath, Status: Running, Branch: "old"},
		{Title: "created-by-cli", Path: repoPath, Status: Running},
	})

	tuiInstance := makeInstance("loaded-in-tui", repoPath, true)
	tuiInstance.Branch = "new"
	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{tuiInstance})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	byTitle := make(map[string]InstanceData)
	for _, d := range result {
		byTitle[d.Title] = d
	}
	assert.Equal(t, "new", byTitle["loaded-in-tui"].Branch, "in-memory TUI instance should update its disk record")
	assert.Contains(t, byTitle, "created-by-cli", "disk-only CLI/task instance must not be overwritten by TUI saves")
}

func TestRepoSaveDoesNotResurrectDeadDiskMissingInstance(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	// This looks started in memory but has no tmux session, which is the
	// shape left behind if another process already killed and deleted it.
	stale := makeInstance("stale", repoPath, true)
	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{stale})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	assert.Empty(t, result, "stale TUI memory must not recreate a deleted instance record")
}

// TestDaemonSaveDoesNotResurrectDeadDiskMissingInstance is the daemon-mode
// counterpart of the TUI test above (#819). If tmux is killed externally and
// the disk record is deleted by another process before the daemon's next
// refresh tick, a shutdown-triggered save runs with stale memory — it must
// not write the dead session back to disk, where it would block title reuse
// and fail to restore.
func TestDaemonSaveDoesNotResurrectDeadDiskMissingInstance(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	ms := newMockStorage()

	// The repo has another live session on disk, so the daemon's save
	// definitely visits and rewrites this repo's file.
	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "other-session", Path: repoPath, Status: Running},
	})

	// Looks started in daemon memory, but its tmux session is dead and its
	// disk record was deleted by another process.
	stale := makeInstance("stale", repoPath, true)
	other := makeInstance("other-session", repoPath, true)

	storage, err := NewStorage(ms, "") // daemon mode
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{other, stale})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	titles := make(map[string]bool)
	for _, d := range result {
		titles[d.Title] = true
	}
	assert.False(t, titles["stale"], "stale daemon memory must not recreate a deleted instance record (#819)")
	assert.True(t, titles["other-session"], "the surviving session must still be saved")
}

// TestDaemonSavePreservesAliveDiskMissingInstance is the #819 counter-case:
// when the disk record is missing but the tmux session is still ALIVE (e.g.
// instances.json was wiped externally, #736 territory), the daemon must
// rewrite the record, not drop the live session.
func TestDaemonSavePreservesAliveDiskMissingInstance(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	ms := newMockStorage()

	alive := makeAliveInstance("alive", repoPath)

	storage, err := NewStorage(ms, "") // daemon mode
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{alive})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	require.Len(t, result, 1, "live session with a missing disk record must be re-persisted")
	assert.Equal(t, "alive", result[0].Title)
}

// TestRepoSavePreservesAliveDiskMissingInstance mirrors the counter-case for
// the TUI path, guarding the shared merge helper from both call sites.
func TestRepoSavePreservesAliveDiskMissingInstance(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	alive := makeAliveInstance("alive", repoPath)

	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{alive})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	require.Len(t, result, 1, "live session with a missing disk record must be re-persisted")
	assert.Equal(t, "alive", result[0].Title)
}

// TestRepoSaveDropsLoadingFromMemory is a regression test for
// sachiniyer/agent-factory#551. When the TUI quits while a session is
// still in Loading status (worktree not yet populated), the in-memory
// instance must not be persisted to disk. FromInstanceData cannot
// restore Loading entries, and the daemon's title-collision check would
// otherwise see the orphan and reject any future session with the same
// title.
func TestRepoSaveDropsLoadingFromMemory(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	loading := makeInstance("in-flight", repoPath, false)
	loading.Status = Loading
	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{loading})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	assert.Empty(t, result, "Loading instance must not be persisted to disk (#551)")
}

// TestRepoSaveDropsDeletingFromMemory is the #844 resurrection guard. While
// an async kill is in flight the TUI's in-memory instance is Deleting and its
// backing session can still look alive. Once the daemon finishes the teardown
// and deletes the disk record, a TUI save must NOT re-persist the instance —
// the "alive but disk-missing" rule (#819) that protects live sessions from
// external file wipes would otherwise resurrect the killed session's record.
func TestRepoSaveDropsDeletingFromMemory(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	// Alive backend + started + empty disk: exactly the shape #819 preserves —
	// unless the instance is Deleting.
	deleting := makeAliveInstance("mid-teardown", repoPath)
	deleting.Status = Deleting

	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	require.NoError(t, storage.SaveInstances([]*Instance{deleting}))

	result := readDisk(t, ms, repoPath)
	assert.Empty(t, result, "Deleting instance must never be persisted (#844)")
}

// TestRepoSavePreservesDiskRecordWhileDeleting covers the in-flight window of
// an async kill: the daemon has not yet deleted the disk record, and a TUI
// save runs. The record must survive untouched (so a TUI crash mid-deletion
// loses nothing) and must keep its pre-kill status — Deleting itself is never
// written to disk.
func TestRepoSavePreservesDiskRecordWhileDeleting(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "mid-teardown", Path: repoPath, Status: Running},
	})

	deleting := makeAliveInstance("mid-teardown", repoPath)
	deleting.Status = Deleting

	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	require.NoError(t, storage.SaveInstances([]*Instance{deleting}))

	result := readDisk(t, ms, repoPath)
	require.Len(t, result, 1, "the pre-kill disk record must survive the save")
	assert.Equal(t, "mid-teardown", result[0].Title)
	assert.Equal(t, Running, result[0].Status,
		"the Deleting marker is in-memory only and must not reach disk")
}

// TestRepoSaveReapsLegacyLoadingGhost verifies that an older binary's
// orphaned Loading record on disk is reaped on the next TUI save, even
// when the in-memory state does not include a same-titled instance. The
// merge path used to preserve such entries as "external", which kept
// the ghost alive across saves.
func TestRepoSaveReapsLegacyLoadingGhost(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()

	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "ghost", Path: repoPath, Status: Loading},
		{Title: "alive", Path: repoPath, Status: Running},
	})

	alive := makeInstance("alive", repoPath, true)
	storage, err := NewStorage(ms, repoID)
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{alive})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	titles := make(map[string]bool, len(result))
	for _, d := range result {
		titles[d.Title] = true
	}
	assert.True(t, titles["alive"], "running instance should remain on disk")
	assert.False(t, titles["ghost"], "legacy Loading ghost must be reaped on save (#551)")
}

// TestDaemonSaveReapsLegacyLoadingGhost mirrors the TUI check for the
// daemon merge path: a Loading record on disk that the daemon did not
// create should be dropped rather than preserved as an external entry.
func TestDaemonSaveReapsLegacyLoadingGhost(t *testing.T) {
	const repoPath = "/tmp/test-repo"
	ms := newMockStorage()

	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "ghost", Path: repoPath, Status: Loading},
		{Title: "alive", Path: repoPath, Status: Running},
	})

	alive := makeInstance("alive", repoPath, true)
	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	err = storage.SaveInstances([]*Instance{alive})
	require.NoError(t, err)

	result := readDisk(t, ms, repoPath)
	titles := make(map[string]bool, len(result))
	for _, d := range result {
		titles[d.Title] = true
	}
	assert.True(t, titles["alive"], "daemon-known instance should remain on disk")
	assert.False(t, titles["ghost"], "legacy Loading ghost must be reaped by daemon save (#551)")
}

// TestDaemonSaveUsesResolvedRepoPathForSymlinkedRepo verifies that the daemon
// computes the on-disk repo ID from the worktree's resolved repo path rather
// than the (possibly symlinked) Instance.Path. Before the fix, an instance
// created from a symlinked directory would be persisted under a *different*
// repo ID than the TUI used, splitting the same repo's state across two
// files and creating ghost sessions on subsequent reloads (#667).
func TestDaemonSaveUsesResolvedRepoPathForSymlinkedRepo(t *testing.T) {
	const resolvedRepoPath = "/tmp/test-repo-resolved"
	const symlinkPath = "/tmp/test-repo-symlink"
	ms := newMockStorage()

	// Disk state pre-exists under the RESOLVED repo ID (this is what the
	// TUI wrote — TUI does not recompute the ID on save).
	seedDisk(t, ms, resolvedRepoPath, []InstanceData{
		{Title: "from-tui", Path: symlinkPath, Worktree: GitWorktreeData{RepoPath: resolvedRepoPath}, Status: Running},
	})

	// The daemon loaded that instance: Path is the symlinked path, but its
	// gitWorktree carries the resolved repo path (set during construction).
	gw, err := git.NewGitWorktreeFromStorage(
		resolvedRepoPath, "/tmp/test-repo-symlink-wt", "from-tui",
		"branch-x", "deadbeef", false, true,
	)
	require.NoError(t, err)
	inst := makeInstance("from-tui", symlinkPath, true)
	inst.gitWorktree = gw

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	require.NoError(t, storage.SaveInstances([]*Instance{inst}))

	// The instance must be written back to the resolved-path repo ID,
	// not the symlinked-path repo ID. Before the fix, the daemon would
	// create a SECOND file under RepoIDFromRoot(symlinkPath).
	resolvedID := config.RepoIDFromRoot(resolvedRepoPath)
	symlinkID := config.RepoIDFromRoot(symlinkPath)
	require.NotEqual(t, resolvedID, symlinkID, "test fixture: paths must hash to distinct IDs")

	all, err := ms.GetAllInstances()
	require.NoError(t, err)
	_, hasResolved := all[resolvedID]
	_, hasSymlink := all[symlinkID]
	assert.True(t, hasResolved, "instance must be saved under the resolved repo ID")
	assert.False(t, hasSymlink, "instance must NOT be duplicated under the symlinked repo ID")

	result := readDisk(t, ms, resolvedRepoPath)
	require.Len(t, result, 1, "exactly one record should be persisted for the repo")
	assert.Equal(t, "from-tui", result[0].Title)
}

// TestDaemonSaveFallsBackToPathForRemoteBackend verifies that the daemon
// still groups by Instance.Path when no worktree is attached (load-bearing
// for remote backends where Worktree.RepoPath is empty).
func TestDaemonSaveFallsBackToPathForRemoteBackend(t *testing.T) {
	const repoPath = "/tmp/test-repo-remote"
	ms := newMockStorage()

	// Remote-backend instance: no gitWorktree, Worktree.RepoPath empty.
	inst := makeAliveInstance("remote-1", repoPath)
	require.Empty(t, inst.GetRepoPath(), "test fixture: remote instance must have empty resolved repo path")

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{inst}))

	result := readDisk(t, ms, repoPath)
	require.Len(t, result, 1)
	assert.Equal(t, "remote-1", result[0].Title)
}

// TestCollectRepoRoots verifies that Storage.CollectRepoRoots returns the
// unique set of repo roots from stored instances across ALL repos. This
// underpins the fix for #265 (af reset must clean worktrees in every repo
// whose instance storage will be deleted, not just the current repo).
func TestCollectRepoRoots(t *testing.T) {
	const repoA = "/tmp/repo-a"
	const repoB = "/tmp/repo-b"
	const repoC = "/tmp/repo-c"
	ms := newMockStorage()

	// Repo A: two instances, both with Worktree.RepoPath set.
	seedDisk(t, ms, repoA, []InstanceData{
		{Title: "a1", Path: repoA, Worktree: GitWorktreeData{RepoPath: repoA}},
		{Title: "a2", Path: repoA, Worktree: GitWorktreeData{RepoPath: repoA}},
	})
	// Repo B: one instance with Worktree.RepoPath set.
	seedDisk(t, ms, repoB, []InstanceData{
		{Title: "b1", Path: repoB, Worktree: GitWorktreeData{RepoPath: repoB}},
	})
	// Repo C: instance with empty Worktree.RepoPath (e.g. remote backend);
	// should fall back to Path.
	seedDisk(t, ms, repoC, []InstanceData{
		{Title: "c1", Path: repoC},
	})

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	roots, err := storage.CollectRepoRoots()
	require.NoError(t, err)

	assert.Len(t, roots, 3, "should collect one entry per unique repo root")
	assert.Contains(t, roots, repoA)
	assert.Contains(t, roots, repoB)
	assert.Contains(t, roots, repoC)
}

// TestCollectRepoRootsEmpty verifies the helper returns an empty set when
// there are no stored instances.
func TestCollectRepoRootsEmpty(t *testing.T) {
	ms := newMockStorage()

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	roots, err := storage.CollectRepoRoots()
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// TestCollectRepoRootsSkipsEmpty verifies that instances with neither a
// Worktree.RepoPath nor a Path are skipped rather than producing an empty
// string entry.
func TestCollectRepoRootsSkipsEmpty(t *testing.T) {
	const repoA = "/tmp/repo-a"
	ms := newMockStorage()

	// One usable instance and one with no usable repo info.
	seedDisk(t, ms, repoA, []InstanceData{
		{Title: "a1", Path: repoA, Worktree: GitWorktreeData{RepoPath: repoA}},
		{Title: "ghost"},
	})

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	roots, err := storage.CollectRepoRoots()
	require.NoError(t, err)

	assert.Len(t, roots, 1)
	assert.Contains(t, roots, repoA)
	_, hasEmpty := roots[""]
	assert.False(t, hasEmpty, "empty repo root should be skipped")
}

// TestCollectRepoRootsSurfacesUnreadableDir verifies that an unreadable
// instances directory is surfaced as an error rather than masquerading as
// "no sessions". Before #868, GetAllInstances swallowed the directory read
// error into an empty map, so `af reset` would skip worktree cleanup for
// every repo and leave orphaned worktrees behind.
func TestCollectRepoRootsSurfacesUnreadableDir(t *testing.T) {
	ms := newMockStorage()
	// Seed a repo so a silent-empty result would be observably wrong.
	seedDisk(t, ms, "/tmp/repo-a", []InstanceData{
		{Title: "a1", Path: "/tmp/repo-a", Worktree: GitWorktreeData{RepoPath: "/tmp/repo-a"}},
	})
	ms.readAllErr = errors.New("permission denied")

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	roots, err := storage.CollectRepoRoots()
	require.Error(t, err, "an unreadable instances directory must surface an error")
	assert.Nil(t, roots, "no roots should be returned when the directory is unreadable")
}

// TestCollectRepoRootsSkipsCorruptedRepo verifies that one repo's corrupted
// instances.json does not abort the whole reset: CollectRepoRoots skips the
// corrupted repo (with a warning) and still returns the roots for the others,
// so `af reset` can clean up every other repo (#869).
func TestCollectRepoRootsSkipsCorruptedRepo(t *testing.T) {
	const repoA = "/tmp/repo-a"
	const repoBad = "/tmp/repo-bad"
	ms := newMockStorage()

	seedDisk(t, ms, repoA, []InstanceData{
		{Title: "a1", Path: repoA, Worktree: GitWorktreeData{RepoPath: repoA}},
	})
	// Corrupt the second repo's stored JSON.
	ms.data[config.RepoIDFromRoot(repoBad)] = json.RawMessage(`{ this is not valid json`)

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	roots, err := storage.CollectRepoRoots()
	require.NoError(t, err, "a single corrupted repo must not abort root collection")
	assert.Len(t, roots, 1, "the healthy repo's root should still be collected")
	assert.Contains(t, roots, repoA)
}

// TestLoadInstancesDaemonSurfacesUnreadableDir verifies that daemon-mode
// LoadInstances (repoID == "") surfaces an unreadable instances directory as
// an error rather than presenting an empty session list that looks like a
// fresh install while live sessions sit unreadable on disk (#868).
func TestLoadInstancesDaemonSurfacesUnreadableDir(t *testing.T) {
	ms := newMockStorage()
	ms.readAllErr = errors.New("permission denied")

	storage, err := NewStorage(ms, "")
	require.NoError(t, err)

	instances, err := storage.LoadInstances()
	require.Error(t, err, "the daemon must not hide an unreadable instances directory")
	assert.Nil(t, instances)
}

// --- Issue #808: instances.json held byte-identical duplicate records -----
//
// One logical session can be written twice when the sidebar briefly holds two
// Instance objects with the same title (a disk-built copy swapped in by
// refreshExternalInstances plus the started instance re-added by the
// instanceStartedMsg handler). The storage layer dedupes by title at every
// save/load chokepoint so neither mode can persist a duplicate, and an
// existing on-disk duplicate collapses on the next clean save.

func TestDedupeInstanceDataKeepsFreshest(t *testing.T) {
	base := time.Date(2026, 6, 10, 11, 38, 47, 75861804, time.UTC)
	older := InstanceData{Title: "scripts", Path: "/repo/stale", UpdatedAt: base}
	newer := InstanceData{Title: "scripts", Path: "/repo/fresh", UpdatedAt: base.Add(time.Second)}
	other := InstanceData{Title: "other", Path: "/repo", UpdatedAt: base}

	out := dedupeInstanceData([]InstanceData{older, newer, other})
	require.Len(t, out, 2)
	assert.Equal(t, "scripts", out[0].Title)
	assert.Equal(t, "/repo/fresh", out[0].Path, "the record with the newest UpdatedAt must win")
	assert.Equal(t, "other", out[1].Title)

	// Byte-identical duplicates (equal UpdatedAt) collapse to the first
	// occurrence — the order both save paths put in-memory records first.
	out = dedupeInstanceData([]InstanceData{older, older})
	require.Len(t, out, 1)
	assert.Equal(t, "/repo/stale", out[0].Path)
}

// TestTUISaveCollapsesOnDiskDuplicate is the one-time collapse: a file
// already containing a byte-identical duplicate is rewritten clean by the
// next save, even with nothing in memory.
func TestTUISaveCollapsesOnDiskDuplicate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoPath = "/tmp/test-repo-808"
	ms := newMockStorage()

	created := time.Date(2026, 6, 10, 11, 38, 47, 75861804, time.UTC)
	dup := InstanceData{Title: "scripts", Path: repoPath, CreatedAt: created, UpdatedAt: created}
	seedDisk(t, ms, repoPath, []InstanceData{dup, dup})

	storage, err := NewStorage(ms, config.RepoIDFromRoot(repoPath))
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances(nil))

	result := readDisk(t, ms, repoPath)
	require.Len(t, result, 1, "byte-identical duplicate must collapse on save")
	assert.Equal(t, "scripts", result[0].Title)
}

// TestTUISaveCollapsesDuplicateInMemoryInstances covers the #808 write path:
// the sidebar holds two live Instance objects for one session; TUI-mode save
// must persist exactly one record.
func TestTUISaveCollapsesDuplicateInMemoryInstances(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoPath = "/tmp/test-repo-808"
	ms := newMockStorage()

	// The daemon persisted the session before the TUI's start RPC returned.
	seedDisk(t, ms, repoPath, []InstanceData{{Title: "scripts", Path: repoPath}})

	storage, err := NewStorage(ms, config.RepoIDFromRoot(repoPath))
	require.NoError(t, err)

	diskCopy := makeInstance("scripts", repoPath, true)
	startedTwin := makeInstance("scripts", repoPath, true)
	require.NoError(t, storage.SaveInstances([]*Instance{diskCopy, startedTwin}))

	result := readDisk(t, ms, repoPath)
	require.Len(t, result, 1, "two in-memory objects for one session must persist as one record")
	assert.Equal(t, "scripts", result[0].Title)
}

// TestDaemonSaveCollapsesOnDiskDuplicate covers daemon-mode save: an
// externally-added session that exists twice on disk (and is unknown to the
// daemon) must be preserved exactly once.
func TestDaemonSaveCollapsesOnDiskDuplicate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoPath = "/tmp/test-repo-808"
	ms := newMockStorage()

	created := time.Date(2026, 6, 10, 11, 38, 47, 75861804, time.UTC)
	dup := InstanceData{Title: "scripts", Path: repoPath, CreatedAt: created, UpdatedAt: created}
	seedDisk(t, ms, repoPath, []InstanceData{
		{Title: "instance-A", Path: repoPath},
		dup,
		dup,
	})

	storage, err := NewStorage(ms, "") // daemon mode
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{makeInstance("instance-A", repoPath, true)}))

	result := readDisk(t, ms, repoPath)
	counts := make(map[string]int)
	for _, d := range result {
		counts[d.Title]++
	}
	assert.Equal(t, map[string]int{"instance-A": 1, "scripts": 1}, counts)
}

// TestLoadInstanceDataCollapsesDuplicates: the data feed used by
// refreshExternalInstances must never present the same title twice.
func TestLoadInstanceDataCollapsesDuplicates(t *testing.T) {
	const repoPath = "/tmp/test-repo-808"
	ms := newMockStorage()

	dup := InstanceData{Title: "scripts", Path: repoPath}
	seedDisk(t, ms, repoPath, []InstanceData{dup, dup})

	storage, err := NewStorage(ms, config.RepoIDFromRoot(repoPath))
	require.NoError(t, err)

	data, err := storage.LoadInstanceData()
	require.NoError(t, err)
	require.Len(t, data, 1)
	assert.Equal(t, "scripts", data[0].Title)
}
