package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RepoInstancesMigrateOnLoadPaths must list exactly the per-repo instances.json
// files under the instances directory — one per repo subdirectory, non-directory
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

	// A stray non-directory entry in the instances dir must not appear: the walk
	// skips it, so the manifest must too.
	dir, err := instancesDirPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "not-a-repo"), []byte("x"), 0644))

	paths, err := RepoInstancesMigrateOnLoadPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{alphaPath, betaPath}, paths,
		"the manifest must list exactly the per-repo instances.json files under the instances directory")
}

// A daemon with no per-repo state (no instances directory) has nothing to migrate,
// so the manifest is empty rather than an error.
func TestRepoInstancesMigrateOnLoadPathsNoInstancesDir(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	paths, err := RepoInstancesMigrateOnLoadPaths()
	require.NoError(t, err)
	assert.Empty(t, paths)
}

// TestManifestAndLoaderSkipInvalidRepoIDSubdir pins the fix for the
// bad-directory-name class: a subdirectory under instances/ whose name is not a
// valid repoID (contains ".", "/", spaces, etc.) must NOT abort the upgrade
// manifest, mirroring the all-repo loader's skip-and-warn posture (state.go).
// Pre-fix, the unvalidated walk fed such a name straight to repoInstancesPath,
// whose ValidateRepoID failure hit the manifest's resolve step and blocked
// binary-only upgrades. af itself only ever creates valid-named subdirectories
// (SaveRepoInstances → repoInstancesPath → ValidateRepoID), so this class is
// always an externally-created stray.
func TestManifestAndLoaderSkipInvalidRepoIDSubdir(t *testing.T) {
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
	// this aborted the manifest's resolve step.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "stray.backup.dir"), 0755))

	// Manifest: must list exactly the healthy repos' paths, skipping the stray
	// dir so an invalid-named subdirectory does not block manifest generation.
	paths, err := RepoInstancesMigrateOnLoadPaths()
	require.NoError(t, err,
		"a stray subdirectory with an invalid repoID must not abort manifest generation")
	assert.ElementsMatch(t, []string{alphaPath, betaPath}, paths,
		"the manifest must enumerate only valid repoID-shaped subdirectories, excluding the invalid-named dir the walk skips")

	// Loader: the same stray dir is handled gracefully — loaded 2, skipped 1.
	// The two paths agree on the skip for this class.
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
