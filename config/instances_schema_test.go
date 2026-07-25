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
	schemaAtomicWriteFile = func(string, []byte, os.FileMode) error {
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
