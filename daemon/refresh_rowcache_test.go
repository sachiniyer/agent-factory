package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefreshDaemonInstances_UnchangedPollDoesNotReadOrReparse is the #5169
// property end to end: the second refresh over unchanged files performs zero
// file reads (the loader's cache hit) and zero row decodes (the outcome
// replay), so the tick's cost is O(repo dirs) rather than O(rows).
func TestRefreshDaemonInstances_UnchangedPollDoesNotReadOrReparse(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)
	seedPollRepo(t, "repo-a", "one", "two")
	seedPollRepo(t, "repo-b", "three")

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)

	_, readsBefore := refreshRepoFileCache.Stats()
	hitsBefore := refreshRowOutcomes.hitsTotal()
	second, _, _, _, err := refreshDaemonInstances(first)
	require.NoError(t, err)
	_, readsAfter := refreshRepoFileCache.Stats()
	hitsAfter := refreshRowOutcomes.hitsTotal()

	assert.Equal(t, readsBefore, readsAfter,
		"the second poll over unchanged files must not read a single instances.json")
	assert.Equal(t, int64(2), hitsAfter-hitsBefore,
		"both repos' parse outcomes must replay on the unchanged tick")
	assert.NotNil(t, second[daemonInstanceKey("repo-a", "one")])
	assert.NotNil(t, second[daemonInstanceKey("repo-b", "three")])
}

// TestRefreshDaemonInstances_ExistingInstanceWinsOnCachedRows pins the
// existing-wins rule through the replay path: whatever lives in the manager's
// map for a key is what the refreshed map keeps, even when the row itself came
// from the outcome cache rather than a fresh decode.
func TestRefreshDaemonInstances_ExistingInstanceWinsOnCachedRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)
	seedPollRepo(t, "repo-a", "one")

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	key := daemonInstanceKey("repo-a", "one")
	require.NotNil(t, first[key])

	sentinel := &session.Instance{ID: "sentinel", Title: "one"}
	existing := map[string]*session.Instance{key: sentinel}
	second, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	assert.Same(t, sentinel, second[key],
		"a live in-memory instance must keep winning over the persisted row on a cached tick")
}

// TestRefreshDaemonInstances_FailedRowStillRetriesOnCachedTicks: only the
// PARSE is cached — a row that cannot materialize is still retried against the
// live backend seam on every tick, so a transient failure heals exactly like
// before.
func TestRefreshDaemonInstances_FailedRowStillRetriesOnCachedTicks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	seedPollRepo(t, "repo-a", "flaky")

	failures := 0
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		if d.Title == "flaky" {
			failures++
			return nil, errors.New("backend not ready")
		}
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.Equal(t, 1, failures)
	_, _, _, _, err = refreshDaemonInstances(first)
	require.NoError(t, err)
	assert.Equal(t, 2, failures,
		"a cached parse must not stop the per-tick materialize retry of an unloadable row")
}

// TestRefreshDaemonInstances_ArchivedRowsAreNotReparsed: the fleet's bulk is
// archived records — on an unchanged tick the cached row set replays and the
// instances array is never re-read or re-decoded.
func TestRefreshDaemonInstances_ArchivedRowsAreNotReparsed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	rows := make([]session.InstanceData, 0, 200)
	for i := 0; i < 200; i++ {
		rows = append(rows, session.InstanceData{
			ID:       session.NewInstanceID(),
			Title:    fmt.Sprintf("archived-%03d", i),
			Liveness: session.LiveArchived,
		})
	}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repo-arch", raw))

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	_, readsAfterFirst := refreshRepoFileCache.Stats()

	second, _, _, _, err := refreshDaemonInstances(first)
	require.NoError(t, err)
	_, readsAfterSecond := refreshRepoFileCache.Stats()
	assert.Equal(t, readsAfterFirst, readsAfterSecond,
		"200 archived rows must not cost a file read on the unchanged tick")
	assert.Equal(t, len(first), len(second), "the cached row set materializes identically")
}

// BenchmarkRefreshDaemonInstances_UnchangedTick measures one steady-state poll
// over 4 repos × 3400 archived rows — the shape #5169's profile showed burning
// ~90% of a core. Run under -bench; not run as a test.
func BenchmarkRefreshDaemonInstances_UnchangedTick(b *testing.B) {
	b.Setenv("AGENT_FACTORY_HOME", b.TempDir())
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	defer func() { fromInstanceDataForRefresh = prev }()

	rows := make([]session.InstanceData, 0, 3400)
	for i := 0; i < 3400; i++ {
		rows = append(rows, session.InstanceData{
			ID:       session.NewInstanceID(),
			Title:    fmt.Sprintf("archived-%04d", i),
			Path:     "/repo/alpha",
			Branch:   fmt.Sprintf("siyer/session-%d", i),
			Status:   session.Status(6),
			Liveness: session.LiveArchived,
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := config.SaveRepoInstances(fmt.Sprintf("repo-%d", i), raw); err != nil {
			b.Fatal(err)
		}
	}

	existing, _, _, _, err := refreshDaemonInstances(nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		existing, _, _, _, err = refreshDaemonInstances(existing)
		if err != nil {
			b.Fatal(err)
		}
	}
}
