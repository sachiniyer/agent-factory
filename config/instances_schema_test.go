package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateRepoInstancesForDaemonLoadRoundTripsLegacyArrayFile(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/repo/alpha")
	path, err := repoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))

	legacy := []byte(`[
  {"title":"alpha","path":"/repo/alpha","unknown":{"keep":true}},
  {"title":"beta","path":"/repo/alpha","extra":[1,2,3]}
]`)
	require.NoError(t, os.WriteFile(path, legacy, 0644))

	result, err := MigrateRepoInstancesForDaemonLoad(repoID)
	require.NoError(t, err)
	assert.True(t, result.Migrated)
	assert.Equal(t, LegacySchemaVersion, result.OriginalVersion)
	assert.Equal(t, InstancesSchemaVersion, result.FinalVersion)
	assert.NotEmpty(t, result.BackupPath)

	raw, err := LoadRepoInstances(repoID)
	require.NoError(t, err)
	assert.JSONEq(t, string(legacy), string(raw))

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	var envelope struct {
		SchemaVersion int               `json:"schema_version"`
		Instances     []json.RawMessage `json:"instances"`
	}
	require.NoError(t, json.Unmarshal(onDisk, &envelope))
	assert.Equal(t, InstancesSchemaVersion, envelope.SchemaVersion)
	require.Len(t, envelope.Instances, 2)
	assert.JSONEq(t, `{"title":"alpha","path":"/repo/alpha","unknown":{"keep":true}}`, string(envelope.Instances[0]))
	assert.JSONEq(t, `{"title":"beta","path":"/repo/alpha","extra":[1,2,3]}`, string(envelope.Instances[1]))

	backup, err := os.ReadFile(result.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, legacy, backup)
}

func TestMigrateRepoInstancesWriteFailurePreservesLegacyArray(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/repo/alpha")
	path, err := repoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	legacy := []byte(`[{"title":"alpha","path":"/repo/alpha"}]`)
	require.NoError(t, os.WriteFile(path, legacy, 0644))

	prevWrite := schemaAtomicWriteFile
	writeErr := errors.New("forced write failure")
	schemaAtomicWriteFile = func(SchemaWriteLinkPolicy, string, []byte, os.FileMode) error {
		return writeErr
	}
	t.Cleanup(func() { schemaAtomicWriteFile = prevWrite })

	result, err := MigrateRepoInstancesForDaemonLoad(repoID)
	require.ErrorIs(t, err, writeErr)
	assert.NotEmpty(t, result.BackupPath)

	onDisk, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, legacy, onDisk)
	backup, readErr := os.ReadFile(result.BackupPath)
	require.NoError(t, readErr)
	assert.Equal(t, legacy, backup)
}

// RepoInstancesMigrateOnLoadPaths must list exactly the per-repo instances.json
// files the daemon-load migrator walks — one per repo subdirectory, non-directory
// entries skipped — because the upgrade manifest snapshots this set. Under-listing
// would leave a rolled-back daemon on a file it cannot read (#2212 R3).
func TestRepoInstancesMigrateOnLoadPaths(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	alpha := RepoIDFromRoot("/repo/alpha")
	beta := RepoIDFromRoot("/repo/beta")
	alphaPath, err := repoInstancesPath(alpha)
	require.NoError(t, err)
	betaPath, err := repoInstancesPath(beta)
	require.NoError(t, err)
	for _, path := range []string{alphaPath, betaPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, []byte(`[]`), 0644))
	}

	// A stray non-directory entry in the instances dir must not appear: the migrator
	// skips it, so the manifest must too.
	dir, err := instancesDirPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "not-a-repo"), []byte("x"), 0644))

	paths, err := RepoInstancesMigrateOnLoadPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{alphaPath, betaPath}, paths,
		"the manifest must list exactly the per-repo instances.json files the migrator walks")
}

// A daemon with no per-repo state (no instances directory) has nothing to migrate,
// so the manifest is empty rather than an error.
func TestRepoInstancesMigrateOnLoadPathsNoInstancesDir(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	paths, err := RepoInstancesMigrateOnLoadPaths()
	require.NoError(t, err)
	assert.Empty(t, paths)
}

// TestMigrateAndManifestSkipInvalidRepoIDSubdir pins the fix for the
// bad-directory-name class: a subdirectory under instances/ whose name is not a
// valid repoID (contains ".", "/", spaces, etc.) must NOT abort the migration
// sweep or the upgrade manifest, mirroring the all-repo loader's skip-and-warn
// posture (state.go). Pre-fix, the unvalidated walk fed such a name straight to
// repoInstancesPath, whose ValidateRepoID failure hit the migrator's default
// branch and aborted every repo's restore — and the manifest's resolve step
// returned the same error, blocking binary-only upgrades. af itself only ever
// creates valid-named subdirectories (SaveRepoInstances → repoInstancesPath →
// ValidateRepoID), so this class is always an externally-created stray.
func TestMigrateAndManifestSkipInvalidRepoIDSubdir(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	healthyA := RepoIDFromRoot("/repo/alpha")
	healthyB := RepoIDFromRoot("/repo/beta")
	for _, repoID := range []string{healthyA, healthyB} {
		rows, err := json.Marshal([]map[string]string{{"title": "s", "path": "/r"}})
		require.NoError(t, err)
		require.NoError(t, SaveRepoInstances(repoID, json.RawMessage(rows)))
	}
	alphaPath, err := repoInstancesPath(healthyA)
	require.NoError(t, err)
	betaPath, err := repoInstancesPath(healthyB)
	require.NoError(t, err)

	dir, err := instancesDirPath()
	require.NoError(t, err)
	// A stray directory whose name fails ValidateRepoID (contains "."). Pre-fix
	// this aborted the whole sweep under both the migrator and the manifest.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "stray.backup.dir"), 0755))

	// Migrator: must NOT abort on the stray dir; the healthy repos still migrate.
	require.NoError(t, MigrateAllRepoInstancesForDaemonLoad(),
		"a stray subdirectory with an invalid repoID must not abort migration of every repo")

	// Manifest: must list exactly the healthy repos' paths, skipping the stray
	// dir so the manifest enumerates the same set the migrator walks.
	paths, err := RepoInstancesMigrateOnLoadPaths()
	require.NoError(t, err,
		"a stray subdirectory with an invalid repoID must not abort manifest generation")
	assert.ElementsMatch(t, []string{alphaPath, betaPath}, paths,
		"the manifest must enumerate exactly the repos the migrator walks, excluding the invalid-named dir the walk skips")

	// Loader: the same stray dir is handled gracefully — loaded 2, skipped 1.
	// The three paths now agree on the skip for this class.
	result, skipped, _, loadErr := LoadAllRepoInstancesReportingMissing()
	require.NoError(t, loadErr)
	assert.Len(t, result, 2)
	assert.Len(t, skipped, 1)
}

// TestRepoInstanceIDsForDaemonLoadSkipsOnlyInvalidNames pins that the walk keeps
// every directory name ValidateRepoID accepts and rejects exactly the ones it
// does not, so the filter is "is this a repoID-shaped name" rather than a
// broader exclusion that could silently drop real repos.
func TestRepoInstanceIDsForDaemonLoadSkipsOnlyInvalidNames(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	dir, err := instancesDirPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0755))

	for _, name := range []string{"valid-repo", "d-abcdef123456", "UPPER_123"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0755))
	}
	// Each of these is a real single directory entry whose name fails
	// ValidateRepoID: "." traverses, " " breaks the pattern, "\" is rejected
	// even though Linux allows it as a filename character.
	for _, name := range []string{"stray.backup.dir", "has space", "back\\slash"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0755))
	}
	// A non-directory entry is still skipped by the IsDir filter, unchanged.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain-file"), []byte("x"), 0644))

	ids, err := repoInstanceIDsForDaemonLoad()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"valid-repo", "d-abcdef123456", "UPPER_123"}, ids,
		"only valid repoID-shaped directory names are enumerated; invalid-named dirs and files are skipped")
}

// TestMigrateAllRepoInstancesForDaemonLoadStillRefusesUnreadableValidRepo pins
// that the invalid-name skip did NOT relax the hard-refuse the unreadable-records
// gate (daemon/unreadable_records_test.go) depends on: a valid-repoID repo whose
// instances.json cannot be read must still abort the migration sweep. The fix
// filters directories whose NAMES are not valid repoIDs; a readable-name repo
// with an I/O failure still reaches MigrateRepoInstancesForDaemonLoad, fails at
// os.ReadFile, and hits the migrator's default branch. A bad directory name is a
// pre-condition failure (the name describes no repo, no file is opened); a read
// failure on a real repo is the class the default branch was written for.
func TestMigrateAllRepoInstancesForDaemonLoadStillRefusesUnreadableValidRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.NoError(t, SaveRepoInstances("readable", json.RawMessage("[]")))
	require.NoError(t, SaveRepoInstances("blocked", json.RawMessage("[]")))
	blocked, err := repoInstancesPath("blocked")
	require.NoError(t, err)

	// chmod 0000 is the realistic shape, but modes inherit the ambient umask and
	// root ignores them, so apply it and then PROVE the file is unreadable —
	// falling back to a directory in its place, which os.ReadFile refuses for
	// everyone. A missing file would not do: the migrator maps that to a no-op.
	require.NoError(t, os.Chmod(blocked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
	if _, err := os.ReadFile(blocked); err == nil {
		require.NoError(t, os.Remove(blocked))
		require.NoError(t, os.Mkdir(blocked, 0o755))
	}
	_, readErr := os.ReadFile(blocked)
	require.Error(t, readErr, "fixture did not take: %s is still readable", blocked)
	require.False(t, os.IsNotExist(readErr), "fixture must produce a READ error, not a missing file")

	err = MigrateAllRepoInstancesForDaemonLoad()
	require.Error(t, err,
		"an unreadable instances.json under a valid repoID must still abort the migration sweep")
	assert.Contains(t, err.Error(), "blocked",
		"the refusal must name the repo whose file could not be read")
}

func TestSaveRepoInstancesWritesEnvelopeAndLoadReturnsArray(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/repo/alpha")
	payload := json.RawMessage(`[{"title":"alpha","path":"/repo/alpha"}]`)
	require.NoError(t, SaveRepoInstances(repoID, payload))

	got, err := LoadRepoInstances(repoID)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(got))

	path, err := repoInstancesPath(repoID)
	require.NoError(t, err)
	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":1,"instances":[{"title":"alpha","path":"/repo/alpha"}]}`, string(onDisk))
}
