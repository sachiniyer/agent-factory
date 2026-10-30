package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// stubRestoreSave swaps saveRestoredRepoRecords for the test and returns the
// groups it was asked to write, keyed by call order.
func stubRestoreSave(t *testing.T, err error) *[][]*session.Instance {
	t.Helper()
	var calls [][]*session.Instance
	prev := saveRestoredRepoRecords
	saveRestoredRepoRecords = func(_ *Manager, _ string, insts []*session.Instance, _ time.Duration) ([]string, error) {
		calls = append(calls, append([]*session.Instance(nil), insts...))
		if err != nil {
			return nil, err
		}
		titles := make([]string, 0, len(insts))
		for _, inst := range insts {
			titles = append(titles, inst.Title)
		}
		return titles, nil
	}
	t.Cleanup(func() { saveRestoredRepoRecords = prev })
	return &calls
}

func markObservedMissing(repoID string) {
	preservedMissingFileRepos.mu.Lock()
	preservedMissingFileRepos.repos[repoID] = true
	preservedMissingFileRepos.mu.Unlock()
}

// The preservation ends only when the records are back on disk: a preserved
// session set is written from memory, and only then is the episode forgotten.
func TestRestoreMissingRecords_WritesPreservedSessionsBackAndEndsTheEpisode(t *testing.T) {
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)
	calls := stubRestoreSave(t, nil)

	a, b, other := &session.Instance{}, &session.Instance{}, &session.Instance{}
	markObservedMissing("gone-repo")
	m := &Manager{}
	m.restoreMissingRecordsLocked(map[string]*session.Instance{
		daemonInstanceKey("gone-repo", "a"):    a,
		daemonInstanceKey("gone-repo", "b"):    b,
		daemonInstanceKey("healthy-repo", "c"): other,
	})

	require.Len(t, *calls, 1, "one write per restored repo")
	require.ElementsMatch(t, []*session.Instance{a, b}, (*calls)[0], "every preserved session of the repo, and no other repo's")
	require.False(t, preservedMissingFileRepos.observed()["gone-repo"], "a restored repo's episode must end")
}

// A failed write keeps the protection: the sessions stay preserved in memory
// and the restore is retried, rather than preservation ending with records gone.
func TestRestoreMissingRecords_FailedWriteKeepsPreservingAndRetries(t *testing.T) {
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)
	calls := stubRestoreSave(t, errors.New("disk full"))

	markObservedMissing("gone-repo")
	m := &Manager{}
	live := map[string]*session.Instance{daemonInstanceKey("gone-repo", "a"): {}}
	m.restoreMissingRecordsLocked(live)
	m.restoreMissingRecordsLocked(live)

	require.Len(t, *calls, 2, "the restore is retried on the next poll")
	require.True(t, preservedMissingFileRepos.observed()["gone-repo"], "protection must outlive a failed restore")
}

// A repo with nothing left in memory has nothing to restore.
func TestRestoreMissingRecords_NothingInMemoryClosesTheEpisode(t *testing.T) {
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)
	calls := stubRestoreSave(t, nil)

	markObservedMissing("gone-repo")
	(&Manager{}).restoreMissingRecordsLocked(map[string]*session.Instance{})

	require.Empty(t, *calls)
	require.False(t, preservedMissingFileRepos.observed()["gone-repo"])
}

// End to end across polls: the file vanishes and the sessions are preserved and
// restored; af then writes a genuinely empty file (every session gone) and the
// daemon treats it as authoritative instead of as the deleted file again; and a
// new session's write between polls no longer drops the other preserved ones,
// because their rows are back on disk.
func TestMissingFile_RestoredThenGenuinelyEmptyIsAuthoritative(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) { return &session.Instance{}, nil }
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	repoID := "restore-then-empty-repo"
	rows, err := json.Marshal([]session.InstanceData{{Title: "a"}, {Title: "b"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, rows))
	keyA, keyB := daemonInstanceKey(repoID, "a"), daemonInstanceKey(repoID, "b")
	a, b := &session.Instance{}, &session.Instance{}
	existing := map[string]*session.Instance{keyA: a, keyB: b}

	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	// Poll 1 sees the file missing: both sessions are preserved, then restored.
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, a, got[keyA])
	require.Same(t, b, got[keyB])
	// The real storage write is out of scope here; stand in for it by writing the
	// rows the checkpoint would, and check the episode ends on success.
	stubRestoreSave(t, nil)
	(&Manager{}).restoreMissingRecordsLocked(got)
	require.NoError(t, config.SaveRepoInstances(repoID, rows))

	// af itself later writes an empty roster (every session gone). That is the
	// authoritative state, not a sequel of the deletion.
	require.NoError(t, config.SaveRepoInstances(repoID, json.RawMessage("[]")))
	got2, _, _, _, err := refreshDaemonInstances(got)
	require.NoError(t, err)
	require.NotContains(t, got2, keyA, "a genuinely empty file af wrote must not resurrect sessions")
	require.NotContains(t, got2, keyB)
}

// A lock that stays held past the budget is an explicit UNKNOWN: preservation
// stays on, and every repo's wait draws on ONE shared budget rather than each
// getting a fresh one.
func TestRestoreMissingRecords_LockTimeoutKeepsPreservingWithinOneBudget(t *testing.T) {
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)
	prevBudget := missingRecordsRestoreBudget
	missingRecordsRestoreBudget = 300 * time.Millisecond
	t.Cleanup(func() { missingRecordsRestoreBudget = prevBudget })

	var budgets []time.Duration
	prev := saveRestoredRepoRecords
	saveRestoredRepoRecords = func(_ *Manager, _ string, _ []*session.Instance, budget time.Duration) ([]string, error) {
		budgets = append(budgets, budget)
		time.Sleep(budget) // a held lock: the wait consumes its whole budget
		return nil, config.ErrLockTimeout
	}
	t.Cleanup(func() { saveRestoredRepoRecords = prev })

	markObservedMissing("repo-a")
	markObservedMissing("repo-b")
	live := map[string]*session.Instance{
		daemonInstanceKey("repo-a", "x"): {},
		daemonInstanceKey("repo-b", "y"): {},
	}
	start := time.Now()
	(&Manager{}).restoreMissingRecordsLocked(live)

	require.Less(t, time.Since(start), 2*time.Second, "the pass must be bounded by one budget, not one per repo")
	require.Len(t, budgets, 2)
	require.LessOrEqual(t, budgets[0], 300*time.Millisecond)
	require.Less(t, budgets[1], budgets[0], "the second repo draws on what the first left")
	require.True(t, preservedMissingFileRepos.observed()["repo-a"], "a timed-out restore keeps preservation on")
	require.True(t, preservedMissingFileRepos.observed()["repo-b"])
}

// The checkpoint projection omits in-flight rows (a Deleting or pre-push
// archive). With one running and one mid-archive session when the file vanishes,
// the restore writes only the running one; BOTH must survive the following
// polls, and protection ends only once the in-flight record lands too (#6048).
func TestMissingFile_InFlightArchiveSurvivesRestoreOfItsRunningSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{Title: d.Title}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	repoID := "inflight-archive-repo"
	running := json.RawMessage(`[{"title":"running"}]`)
	both := json.RawMessage(`[{"title":"running"},{"title":"archiving"}]`)
	require.NoError(t, config.SaveRepoInstances(repoID, both))
	keyRun, keyArch := daemonInstanceKey(repoID, "running"), daemonInstanceKey(repoID, "archiving")
	run, arch := &session.Instance{Title: "running"}, &session.Instance{Title: "archiving"}
	existing := map[string]*session.Instance{keyRun: run, keyArch: arch}

	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	// Poll 1: the file is gone; both live sessions are preserved.
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, run, got[keyRun])
	require.Same(t, arch, got[keyArch])

	// The restore checkpoint writes only the running row (the archiving row is
	// excluded by the projection): the save "succeeds" but one record is pending.
	prevSave := saveRestoredRepoRecords
	saveRestoredRepoRecords = func(_ *Manager, id string, _ []*session.Instance, _ time.Duration) ([]string, error) {
		require.NoError(t, config.SaveRepoInstances(id, running))
		return []string{"running"}, nil
	}
	t.Cleanup(func() { saveRestoredRepoRecords = prevSave })
	(&Manager{}).restoreMissingRecordsLocked(got)
	require.True(t, preservedMissingFileRepos.observed()[repoID],
		"a successful save that omitted an in-flight row must not end the protection")

	// Poll 2 and 3: the file carries only the running row, yet the mid-archive
	// session is still held — it must not be dropped.
	for i := 0; i < 2; i++ {
		got, _, _, _, err = refreshDaemonInstances(got)
		require.NoError(t, err)
		require.Contains(t, got, keyRun)
		require.Same(t, arch, got[keyArch], "the in-flight archive must survive the poll after the restore (poll %d)", i+2)
	}
	require.True(t, preservedMissingFileRepos.observed()[repoID])

	// Its record lands (the archive's own write): protection ends, both load
	// through the normal path.
	require.NoError(t, config.SaveRepoInstances(repoID, both))
	got, _, _, _, err = refreshDaemonInstances(got)
	require.NoError(t, err)
	require.Contains(t, got, keyRun)
	require.Contains(t, got, keyArch)
	require.False(t, preservedMissingFileRepos.observed()[repoID], "protection ends once every record is on disk")
}

// Protection is per owed identity, not per repo-wide episode: once a session's
// record has landed, removing it (another process killing it, an administrator
// rewriting the roster) is authoritative and must not be undone by the partial
// recovery still owed to a different, in-flight session (#6048 review).
func TestMissingFile_RestoredSessionRemovedLaterStaysRemoved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{Title: d.Title}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	repoID := "partial-recovery-removal-repo"
	all := json.RawMessage(`[{"title":"keep"},{"title":"killed"},{"title":"archiving"}]`)
	require.NoError(t, config.SaveRepoInstances(repoID, all))
	keyKeep, keyKilled, keyArch := daemonInstanceKey(repoID, "keep"), daemonInstanceKey(repoID, "killed"), daemonInstanceKey(repoID, "archiving")
	keep, killed, arch := &session.Instance{Title: "keep"}, &session.Instance{Title: "killed"}, &session.Instance{Title: "archiving"}

	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	got, _, _, _, err := refreshDaemonInstances(map[string]*session.Instance{keyKeep: keep, keyKilled: killed, keyArch: arch})
	require.NoError(t, err)
	require.Len(t, got, 3)

	// The restore lands keep and killed; the in-flight archive stays owed.
	var written [][]string
	prevSave := saveRestoredRepoRecords
	saveRestoredRepoRecords = func(_ *Manager, id string, insts []*session.Instance, _ time.Duration) ([]string, error) {
		titles := make([]string, 0, len(insts))
		for _, inst := range insts {
			titles = append(titles, inst.Title)
		}
		written = append(written, titles)
		require.NoError(t, config.SaveRepoInstances(id, json.RawMessage(`[{"title":"keep"},{"title":"killed"}]`)))
		return []string{"keep", "killed"}, nil
	}
	t.Cleanup(func() { saveRestoredRepoRecords = prevSave })
	(&Manager{}).restoreMissingRecordsLocked(got)
	require.True(t, preservedMissingFileRepos.observed()[repoID], "the archive's record is still owed")

	// Another process kills the already-restored session: the file now carries
	// only keep.
	require.NoError(t, config.SaveRepoInstances(repoID, json.RawMessage(`[{"title":"keep"}]`)))
	got, _, _, _, err = refreshDaemonInstances(got)
	require.NoError(t, err)
	require.NotContains(t, got, keyKilled, "a restored session removed afterwards must stay removed")
	require.Same(t, arch, got[keyArch], "the still-owed in-flight session is still protected")
	require.Contains(t, got, keyKeep)

	// And the next restore pass never writes the removed session back.
	(&Manager{}).restoreMissingRecordsLocked(got)
	require.Len(t, written, 2)
	require.NotContains(t, written[1], "killed", "the removed session must not be written back on the same poll")
}
