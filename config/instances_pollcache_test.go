package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRepoInstancesFileForCacheTest writes bytes directly to a repo's
// instances.json, in place — the same path os.WriteFile would take if an
// external tool edited the file rather than renaming over it. It returns the
// path so callers can also forge mtimes.
func writeRepoInstancesFileForCacheTest(t testing.TB, repoID string, data []byte) string {
	t.Helper()
	path, err := RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

// pollCacheFixture seeds repoID with one session row and returns its path.
func pollCacheFixture(t testing.TB, repoID, title string) string {
	t.Helper()
	raw, err := json.Marshal([]map[string]any{{"title": title, "id": "6b1f0000-d978-4397-9dd3-2a70dc42bd34"}})
	require.NoError(t, err)
	require.NoError(t, SaveRepoInstances(repoID, raw))
	path, err := RepoInstancesPath(repoID)
	require.NoError(t, err)
	return path
}

func pollCacheTitles(t testing.TB, result RepoInstancesPollResult, repoID string) []string {
	t.Helper()
	var rows []struct {
		Title string `json:"title"`
	}
	require.NoError(t, json.Unmarshal(result.Instances[repoID], &rows))
	titles := make([]string, 0, len(rows))
	for _, row := range rows {
		titles = append(titles, row.Title)
	}
	return titles
}

// TestRepoInstancesFileCache_UnchangedFileIsNotReread is the #5169 property at
// the file layer: a second poll over an unchanged file performs no read, no
// validation, no normalization, no migration — the cached extraction comes back
// and the read counter stays where the first poll left it.
func TestRepoInstancesFileCache_UnchangedFileIsNotReread(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	pollCacheFixture(t, "repo-a", "alpha")

	cache := NewRepoInstancesFileCache()
	first, err := cache.LoadAll()
	require.NoError(t, err)
	_, reads := cache.Stats()
	require.EqualValues(t, 1, reads)

	second, err := cache.LoadAll()
	require.NoError(t, err)
	hits, reads := cache.Stats()
	assert.EqualValues(t, 1, hits, "second poll over an unchanged file must be a cache hit")
	assert.EqualValues(t, 1, reads, "second poll over an unchanged file performed a file read")
	assert.Equal(t, first.Instances["repo-a"], second.Instances["repo-a"])
	assert.Contains(t, second.Signatures, "repo-a",
		"the loader must keep reporting the signature that attests to the cached bytes")
}

// TestRepoInstancesFileCache_ExternalWriteObserved covers an in-place writer
// (os.WriteFile truncates and rewrites the same inode): the very next poll
// must observe the new rows.
func TestRepoInstancesFileCache_ExternalWriteObserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	pollCacheFixture(t, "repo-a", "alpha")

	cache := NewRepoInstancesFileCache()
	first, err := cache.LoadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"alpha"}, pollCacheTitles(t, first, "repo-a"))

	writeRepoInstancesFileForCacheTest(t, "repo-a",
		[]byte(`{"schema_version":1,"instances":[{"title":"beta","id":"6b1f0000-d978-4397-9dd3-2a70dc42bd34"}]}`))
	second, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"beta"}, pollCacheTitles(t, second, "repo-a"),
		"an in-place external write must be observed on the next poll")
}

// TestRepoInstancesFileCache_SameSizeRewriteObserved replaces the file with
// different content of identical length — the case a size+mtime signature is
// most tempted to miss.
func TestRepoInstancesFileCache_SameSizeRewriteObserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	pollCacheFixture(t, "repo-a", "alpha")

	cache := NewRepoInstancesFileCache()
	first, err := cache.LoadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"alpha"}, pollCacheTitles(t, first, "repo-a"))

	path, err := RepoInstancesPath("repo-a")
	require.NoError(t, err)
	orig, err := os.ReadFile(path)
	require.NoError(t, err)
	rewritten := bytes_replaceTitle(t, orig, "alpha", "omega")
	require.Len(t, rewritten, len(orig), "fixture must be a same-size rewrite")
	require.NoError(t, os.WriteFile(path, rewritten, 0o644))

	second, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"omega"}, pollCacheTitles(t, second, "repo-a"),
		"a same-size rewrite must be observed on the next poll")
}

// TestRepoInstancesFileCache_BackDatedRewriteObserved is the coarse-mtime and
// timestamp-preserving-restore case: the rewrite keeps the same size AND the
// same mtime, forged back with Chtimes — only the inode-change time still moves.
// A signature without ctime could not see this write.
func TestRepoInstancesFileCache_BackDatedRewriteObserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	pollCacheFixture(t, "repo-a", "alpha")

	cache := NewRepoInstancesFileCache()
	_, err := cache.LoadAll()
	require.NoError(t, err)

	path, err := RepoInstancesPath("repo-a")
	require.NoError(t, err)
	orig, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	rewritten := bytes_replaceTitle(t, orig, "alpha", "omega")
	require.Len(t, rewritten, len(orig), "fixture must be a same-size rewrite")
	require.NoError(t, os.WriteFile(path, rewritten, 0o644))
	require.NoError(t, os.Chtimes(path, time.Now(), info.ModTime()),
		"forge the mtime back to the cached value, leaving ctime as the only moved field")

	second, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"omega"}, pollCacheTitles(t, second, "repo-a"),
		"a same-size rewrite with a back-dated mtime must still be observed")
}

func bytes_replaceTitle(t testing.TB, raw []byte, old, new string) []byte {
	t.Helper()
	require.Len(t, new, len(old), "titles must be same length for a same-size rewrite")
	out := bytes.Replace(raw, []byte(old), []byte(new), 1)
	require.NotEqual(t, string(raw), string(out), "fixture must contain the title being replaced")
	return out
}

// TestRepoInstancesFileCache_MissingAndEmptyFiles keeps the loader's two
// different empty answers distinct: a repo directory with no file is "missing"
// (so the daemon will not count it as a re-read), while a present-but-empty
// file is a real read of nothing.
func TestRepoInstancesFileCache_MissingAndEmptyFiles(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	require.NoError(t, SaveRepoInstances("empty-r", json.RawMessage("[]")))
	path, err := RepoInstancesPath("empty-r")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Dir(mustRepoInstancesPath(t, "gone-r")), 0o755))

	cache := NewRepoInstancesFileCache()
	result, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage("[]"), result.Instances["empty-r"])
	assert.False(t, result.Missing["empty-r"], "an empty file was read; it is not missing")
	assert.True(t, result.Missing["gone-r"], "a repo dir with no instances.json is missing")

	_, err = cache.LoadAll()
	require.NoError(t, err)
	_, reads := cache.Stats()
	assert.EqualValues(t, 1, reads,
		"the second poll re-stats the missing and empty files but reads neither again")
}

func mustRepoInstancesPath(t testing.TB, repoID string) string {
	t.Helper()
	path, err := RepoInstancesPath(repoID)
	require.NoError(t, err)
	return path
}

// TestRepoInstancesFileCache_CorruptFileServesRawBytes keeps the
// decode-and-report contract: a malformed file hands the daemon its own bytes,
// and the cached hit serves the same bytes on the next tick without reading.
func TestRepoInstancesFileCache_CorruptFileServesRawBytes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corrupt := []byte(`{"schema_version":1,"instances":{"not":"an array"}}`)
	writeRepoInstancesFileForCacheTest(t, "bad-r", corrupt)

	cache := NewRepoInstancesFileCache()
	first, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(corrupt), first.Instances["bad-r"],
		"a corrupt file's own bytes go to the daemon for decode-and-report")

	_, err = cache.LoadAll()
	require.NoError(t, err)
	hits, reads := cache.Stats()
	assert.EqualValues(t, 1, hits)
	assert.EqualValues(t, 1, reads, "a corrupt file's verdict is cached too — no re-read to re-report it")
}

// TestRepoInstancesFileCache_LegacyFileMigratesOnce: a legacy array-root file
// is migrated in place by the first poll (the sweep behavior the cache fused
// in), and the second poll rides the fast path without another rewrite.
func TestRepoInstancesFileCache_LegacyFileMigratesOnce(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	path := writeRepoInstancesFileForCacheTest(t, "old-r",
		[]byte(`[{"title":"legacy","id":"6b1f0000-d978-4397-9dd3-2a70dc42bd34"}]`))

	cache := NewRepoInstancesFileCache()
	first, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"legacy"}, pollCacheTitles(t, first, "old-r"))
	assert.True(t, ProveJSONSchemaVersion(mustRead(t, path), InstancesSchemaVersion),
		"the first poll must still migrate the legacy file in place")

	// The migrate tick deliberately reports no signature — the file changed
	// underneath the parse, so nothing can be cached yet. The next poll
	// re-observes the migrated file through the fast path and caches it, and
	// the poll after that is the hit.
	_, err = cache.LoadAll()
	require.NoError(t, err)
	hits, _ := cache.Stats()
	assert.EqualValues(t, 0, hits, "the tick after a migration still re-observes the file")
	_, err = cache.LoadAll()
	require.NoError(t, err)
	hits, _ = cache.Stats()
	assert.EqualValues(t, 1, hits, "the migrated file is served from cache once steady state is reached")
}

func mustRead(t testing.TB, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// TestRepoInstancesFileCache_CaseVariantSchemaVersionRefuses is the fail-first
// for the Codex finding on #5170: encoding/json matches the schema_version
// field case-insensitively and takes the LAST fold-match, so this document
// decodes as version 99 and the full path refuses it. A byte-scan proof that
// only counts the exact-spelled member would take the 1 and let a file the
// real pipeline rejects sail through as "[]" — fail-open on the version guard.
// The loader must refuse the same way the migration machinery does.
func TestRepoInstancesFileCache_CaseVariantSchemaVersionRefuses(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeRepoInstancesFileForCacheTest(t, "case-r",
		[]byte(`{"schema_version":1,"SCHEMA_VERSION":99,"instances":[]}`))

	cache := NewRepoInstancesFileCache()
	_, err := cache.LoadAll()
	require.Error(t, err,
		"a document the full decode reads as schema_version=99 must be refused, not fast-pathed as current")
	assert.Contains(t, err.Error(), "schema_version = 99")
	assert.Contains(t, err.Error(), "failed to migrate instances for repo case-r")
}

// TestRepoInstancesFileCache_CaseVariantLastFoldMatchLoads is the same
// ambiguity with the members ordered the other way: the last fold-match is the
// exactly-spelled key, so the full decode accepts the file as version 1 and
// the loader must too — same verdict, reached through the real decode rather
// than the proof.
func TestRepoInstancesFileCache_CaseVariantLastFoldMatchLoads(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeRepoInstancesFileForCacheTest(t, "case-ok",
		[]byte(`{"SCHEMA_VERSION":99,"schema_version":1,"instances":[{"title":"ok","id":"6b1f0000-d978-4397-9dd3-2a70dc42bd34"}]}`))

	cache := NewRepoInstancesFileCache()
	result, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"ok"}, pollCacheTitles(t, result, "case-ok"),
		"last fold-match wins in the decoder — this document IS version 1")
}

// TestRepoInstancesFileCache_CaseVariantInstancesMemberUsesLastFoldMatch: the
// same case-insensitivity applies to the instances member itself. Two
// fold-matching members decode last-one-wins into the envelope struct, so the
// extractor must not return the earlier exact-spelled member.
func TestRepoInstancesFileCache_CaseVariantInstancesMemberUsesLastFoldMatch(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeRepoInstancesFileForCacheTest(t, "inst-case",
		[]byte(`{"schema_version":1,"instances":[{"title":"a"}],"INSTANCES":[{"title":"b"}]}`))

	cache := NewRepoInstancesFileCache()
	result, err := cache.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, pollCacheTitles(t, result, "inst-case"),
		"encoding/json takes the last case-insensitive match; the scan must defer")
}

// TestRepoInstancesFileCache_NewerSchemaAborts keeps the sweep's hard-fail on
// a file a newer binary wrote: the whole load errors rather than reporting a
// per-repo skip, so the daemon never overwrites what it cannot parse.
func TestRepoInstancesFileCache_NewerSchemaAborts(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeRepoInstancesFileForCacheTest(t, "newer-r",
		[]byte(`{"schema_version":99,"instances":[]}`))

	cache := NewRepoInstancesFileCache()
	_, err := cache.LoadAll()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to migrate instances for repo newer-r")
}

// TestInstancesArrayInCurrentEnvelope_NoNormalization is the "already current"
// fast-path contract: the member bytes come back VERBATIM — spacing included —
// so nothing can have unmarshalled and re-marshalled the array. The reject
// cases are routed to the same errors the full decode produced.
func TestInstancesArrayInCurrentEnvelope_NoNormalization(t *testing.T) {
	raw := []byte(`{ "schema_version": 1, "instances": [ { "title": "a" } ] }`)
	require.True(t, ProveJSONSchemaVersion(raw, InstancesSchemaVersion), "fixture must be provably current")

	got, err := instancesArrayInCurrentEnvelope(raw)
	require.NoError(t, err)
	assert.Equal(t, `[ { "title": "a" } ]`, string(got),
		"whitespace survives verbatim: the array was sliced out, not normalized")

	normalized, err := decodeInstancesEnvelope(raw)
	require.NoError(t, err)
	var wantRows, gotRows []map[string]any
	require.NoError(t, json.Unmarshal(normalized, &wantRows))
	require.NoError(t, json.Unmarshal(got, &gotRows))
	assert.Equal(t, wantRows, gotRows,
		"fast and slow paths must decode to the same rows — verbatim member bytes "+
			"differ from normalized bytes only in whitespace, which no consumer sees")
}

func TestInstancesArrayInCurrentEnvelope_ErrorParity(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"no member", `{"schema_version":1}`},
		{"null member", `{"schema_version":1,"instances":null}`},
		{"object member", `{"schema_version":1,"instances":{"a":1}}`},
		{"string member", `{"schema_version":1,"instances":"x"}`},
		{"number member", `{"schema_version":1,"instances":5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			require.True(t, ProveJSONSchemaVersion(raw, InstancesSchemaVersion), "fixture must be provably current")
			want, wantErr := decodeInstancesEnvelope(raw)
			got, gotErr := instancesArrayInCurrentEnvelope(raw)
			if wantErr == nil {
				require.NoError(t, gotErr)
				assert.Equal(t, string(want), string(got))
				return
			}
			require.Error(t, gotErr)
			assert.Equal(t, wantErr.Error(), gotErr.Error(), "error text diverged")
			assert.Equal(t, errorShape(wantErr), errorShape(gotErr), "error classification diverged")
		})
	}
}

// pollBenchRows builds the bare instances array (not the envelope — that is
// SaveRepoInstances' job) holding n archived-shaped rows at real-box size: a
// persisted row carries path, worktree, branch, tabs, timestamps and task
// fields, ~1.4 kB each, so n=3400 reproduces the ~4.7 MB instances.json the
// #5169 profile was captured against.
func pollBenchRows(b *testing.B, n int) json.RawMessage {
	b.Helper()
	items := make([]json.RawMessage, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, json.RawMessage(fmt.Sprintf(
			`{"id":"6b1f%04d-d978-4397-9dd3-2a70dc42bd34","title":"session %d","path":"/home/user/repos/agent-factory-%d",`+
				`"worktree_path":"/home/user/repos/agent-factory-%d/.worktrees/session-%d",`+
				`"branch":"siyer/session-%d","status":6,"liveness":5,"created_at":"2026-09-%02dT10:%02d:%02dZ",`+
				`"updated_at":"2026-09-%02dT11:%02d:%02dZ","program":"claude","task_id":"task-%04d",`+
				`"tabs":[{"kind":"agent","title":"agent","command":"claude --dangerously-skip-permissions"},`+
				`{"kind":"term","title":"build","command":"go build ./..."},{"kind":"term","title":"test"}],`+
				`"account":"primary","resume_session_id":"sess-%032d","pending_handoff_mission":"",`+
				`"env_passthrough":["PATH","HOME","SSH_AUTH_SOCK","GH_TOKEN","AF_HOME"],`+
				`"recovery_log":"row %d archived after task completion; checkpoint saved to %s; `+
				`worktree clean at archive time; last agent turn ended stop_reason=end_turn; `+
				`runtime cleanup settled; transcript retained under session dir for audit; `+
				`daemon restored this session twice across upgrades without losing its task slot; `+
				`final review pass landed after codex sign-off and CI green on the release branch; `+
				`no follow-up issues filed from this session's changes; safe to prune after retention",`+
				`"archive_reason":"task completed and merged; record kept for the retention window"}`,
			i, i, i%90, i, i, i, i%28+1, i%60, i%60, i%28+1, i%60, i%60, i%100, i, i,
			fmt.Sprintf("/home/user/.config/agent-factory/checkpoints/sess-%032d.json", i))))
	}
	raw, err := json.Marshal(items)
	require.NoError(b, err)
	return raw
}

// BenchmarkRepoInstancesFileCacheLoadAllUnchanged measures one poll tick over
// the #5169 steady state — a ~4.7 MB instances.json holding ~3,400 archived
// rows, unchanged between ticks.
func BenchmarkRepoInstancesFileCacheLoadAllUnchanged(b *testing.B) {
	b.Setenv("AGENT_FACTORY_HOME", b.TempDir())
	raw := pollBenchRows(b, 3400)
	require.NoError(b, SaveRepoInstances("repo-alpha", raw))
	cache := NewRepoInstancesFileCache()
	if _, err := cache.LoadAll(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cache.LoadAll(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRepoInstancesPollLegacyBaseline is the pre-#5169 pair this change
// fuses and caches over: a full migration sweep plus a full reporting load of
// every repo file, every tick.
func BenchmarkRepoInstancesPollLegacyBaseline(b *testing.B) {
	b.Setenv("AGENT_FACTORY_HOME", b.TempDir())
	raw := pollBenchRows(b, 3400)
	require.NoError(b, SaveRepoInstances("repo-alpha", raw))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := MigrateAllRepoInstancesForDaemonLoad(); err != nil {
			b.Fatal(err)
		}
		if _, _, _, err := LoadAllRepoInstancesReportingMissing(); err != nil {
			b.Fatal(err)
		}
	}
}
