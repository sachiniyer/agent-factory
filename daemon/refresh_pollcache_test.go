package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file deliberately use only APIs that exist WITHOUT the
// #5169 fix — refreshDaemonInstances, config.SaveRepoInstances, plain file
// writes, Chtimes, and the file lock — so the same file runs unchanged on a
// probe/5169-* revert branch, where the lock test must fail first.

// rewriteInstancesFileInPlace rewrites a repo's instances.json WITHOUT the
// atomic rename SaveRepoInstances performs — the external-editor shape that
// keeps the inode and only moves mtime/size/ctime.
func rewriteInstancesFileInPlace(t *testing.T, repoID string, data []byte) string {
	t.Helper()
	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

// seedPollRepo saves one repo's instances.json at the current schema with
// fully-identified rows: rows without ids would take the legacy-ID backfill
// write path, which is not what these fixtures are about.
func seedPollRepo(t *testing.T, repoID string, titles ...string) {
	t.Helper()
	rows := make([]session.InstanceData, 0, len(titles))
	for _, title := range titles {
		rows = append(rows, session.InstanceData{
			ID:       session.NewInstanceID(),
			Title:    title,
			Liveness: session.LiveArchived,
		})
	}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, raw))
}

// repoInstancesRaw reads the file's current bytes — the fixture baseline for
// same-size rewrites.
func repoInstancesRaw(t *testing.T, repoID string) []byte {
	t.Helper()
	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// TestRefreshDaemonInstances_UnchangedFileDoesNotTakeMigrationLock is the
// fail-first probe for #5169. Before the fix every poll tick ran the
// migration sweep, which takes the per-file flock — holding that lock between
// ticks turned the tick itself into an error. After the fix an unchanged file
// is a stat-gated cache hit that never touches the lock at all.
func TestRefreshDaemonInstances_UnchangedFileDoesNotTakeMigrationLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)
	seedPollRepo(t, "repo-a", "one")

	prev := config.SchemaMigrationLockTimeout
	config.SchemaMigrationLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { config.SchemaMigrationLockTimeout = prev })

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.NotNil(t, first[daemonInstanceKey("repo-a", "one")])

	path, err := config.RepoInstancesPath("repo-a")
	require.NoError(t, err)
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB),
		"fixture must hold the schema-migration lock the per-tick sweep takes")
	t.Cleanup(func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	})

	second, _, _, _, err := refreshDaemonInstances(first)
	require.NoError(t, err,
		"a poll over an unchanged file must not reach for the migration lock — "+
			"before the fix the sweep took it every tick and this blocked until timeout")
	require.NotNil(t, second[daemonInstanceKey("repo-a", "one")])
}

// TestRefreshDaemonInstances_ExternalWriteObserved: an in-place external write
// (same inode, truncated and rewritten) between ticks is observed by the next
// refresh — the correctness contract the cache must not trade away.
func TestRefreshDaemonInstances_ExternalWriteObserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)
	seedPollRepo(t, "repo-a", "one")

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.NotNil(t, first[daemonInstanceKey("repo-a", "one")])

	external, err := json.Marshal([]session.InstanceData{{
		ID: session.NewInstanceID(), Title: "two", Liveness: session.LiveArchived,
	}})
	require.NoError(t, err)
	envelope, err := json.Marshal(map[string]json.RawMessage{
		"schema_version": json.RawMessage("1"),
		"instances":      external,
	})
	require.NoError(t, err)
	rewriteInstancesFileInPlace(t, "repo-a", envelope)
	second, _, _, _, err := refreshDaemonInstances(first)
	require.NoError(t, err)
	assert.Nil(t, second[daemonInstanceKey("repo-a", "one")])
	assert.NotNil(t, second[daemonInstanceKey("repo-a", "two")],
		"an in-place external write must be observed on the next poll")
}

// TestRefreshDaemonInstances_SameSizeRewriteObserved is the rewrite a
// size-keyed or coarse signature could hide: identical byte length, same
// inode, different content.
func TestRefreshDaemonInstances_SameSizeRewriteObserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)
	seedPollRepo(t, "repo-a", "one")

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.NotNil(t, first[daemonInstanceKey("repo-a", "one")])

	raw := repoInstancesRaw(t, "repo-a")
	rewritten := bytes.Replace(raw, []byte(`"one"`), []byte(`"two"`), 1)
	require.Len(t, rewritten, len(raw), "fixture must be a same-size rewrite")
	require.NotEqual(t, raw, rewritten)
	rewriteInstancesFileInPlace(t, "repo-a", rewritten)

	second, _, _, _, err := refreshDaemonInstances(first)
	require.NoError(t, err)
	assert.NotNil(t, second[daemonInstanceKey("repo-a", "two")],
		"a same-size in-place rewrite must be observed")
	assert.Nil(t, second[daemonInstanceKey("repo-a", "one")])
}

// TestRefreshDaemonInstances_BackDatedRewriteObserved is the coarse-mtime
// case: an in-place rewrite whose mtime is forged back to the cached value.
// Only the non-settable ctime can still see this write.
func TestRefreshDaemonInstances_BackDatedRewriteObserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)
	seedPollRepo(t, "repo-a", "one")

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.NotNil(t, first[daemonInstanceKey("repo-a", "one")])

	path, err := config.RepoInstancesPath("repo-a")
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	raw := repoInstancesRaw(t, "repo-a")
	rewritten := bytes.Replace(raw, []byte(`"one"`), []byte(`"two"`), 1)
	require.Len(t, rewritten, len(raw), "fixture must be a same-size rewrite")
	require.NoError(t, os.WriteFile(path, rewritten, 0o644))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()),
		"forge mtime back: only the non-settable ctime can catch this write")

	second, _, _, _, err := refreshDaemonInstances(first)
	require.NoError(t, err)
	assert.NotNil(t, second[daemonInstanceKey("repo-a", "two")],
		"a same-size rewrite with a back-dated mtime must still be observed")
}

// TestRefreshDaemonInstances_CaseVariantSchemaVersionRefuses is the fail-first
// for the Codex finding on #5170: encoding/json fold-matches schema_version
// and takes the LAST match, so this document decodes as version 99 — which the
// full migration path refuses with "schema_version = 99, want 1". A byte-scan
// proof that only counts the exact-spelled key would take the 1 and report the
// repo as EMPTY: fail-open where the old pipeline aborted the refresh. The
// tick must refuse the same way.
func TestRefreshDaemonInstances_CaseVariantSchemaVersionRefuses(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	rewriteInstancesFileInPlace(t, "case-r",
		[]byte(`{"schema_version":1,"SCHEMA_VERSION":99,"instances":[]}`))

	_, _, _, _, err := refreshDaemonInstances(nil)
	require.Error(t, err,
		"a document the full decode reads as schema_version=99 must be refused, not fast-pathed as current")
	assert.Contains(t, err.Error(), "schema_version = 99")
}

// TestRefreshDaemonInstances_CorruptVerdictPersistsAcrossTicks: a corrupt file
// stays skipped on every tick — the cache must replay the verdict, warning and
// all, rather than clearing it or silently healing.
func TestRefreshDaemonInstances_CorruptVerdictPersistsAcrossTicks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	warnings := captureWarnings(t)
	seedCorruptedRepo(t, "corrupt-r")

	first, _, skipped1, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON}}, skipped1)

	_, _, skipped2, _, err := refreshDaemonInstances(first)
	require.NoError(t, err)
	assert.Equal(t, skipped1, skipped2, "a corrupt file must stay skipped on the tick that replays it")
	assert.Equal(t, 2, strings.Count(warnings.String(), "corrupt-r"),
		"the corrupt-repo warning must still be logged on the replayed tick")
}
