package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRepoInstancesPath_RejectsTraversal exercises the path-construction
// helper directly. Every malicious id must produce an error before the path
// is returned, so callers can never read or write outside the instances
// directory (#515).
func TestRepoInstancesPath_RejectsTraversal(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	malicious := []string{
		"",
		"..",
		"../../../etc/passwd",
		"foo/../bar",
		"/etc/passwd",
		"foo/bar",
		"foo\\bar",
		".",
		".hidden",
		"foo\x00bar",
	}
	for _, id := range malicious {
		t.Run(id, func(t *testing.T) {
			_, err := repoInstancesPath(id)
			assert.Error(t, err, "expected %q to be rejected", id)
		})
	}
}

// TestRepoInstancesPath_AcceptsLegitimate locks in the success path for the
// real production id shape plus existing test fixtures.
func TestRepoInstancesPath_AcceptsLegitimate(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	legitimate := []string{
		RepoIDFromRoot("/some/path"),
		"test-repo",
		"test-repo-id",
		"abc123def456",
	}
	for _, id := range legitimate {
		t.Run(id, func(t *testing.T) {
			path, err := repoInstancesPath(id)
			require.NoError(t, err)
			expectedParent, perr := instancesDirPath()
			require.NoError(t, perr)
			// The resolved path must live under the instances directory.
			assert.True(t, strings.HasPrefix(path, expectedParent+string(filepath.Separator)),
				"path %q did not stay under %q", path, expectedParent)
			assert.True(t, strings.HasSuffix(path, InstancesFileName))
		})
	}
}

// TestLoadSaveRepoInstances_RejectsTraversal covers the public API surface
// that the daemon RPC ultimately calls. Both Load and Save must surface an
// error and must not touch the filesystem outside the instances directory.
func TestLoadSaveRepoInstances_RejectsTraversal(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	// Plant a sentinel file outside the instances directory. A successful
	// traversal would either expose its bytes or clobber it; both are
	// observable by inspecting the file afterwards.
	sentinelDir := filepath.Join(tempHome, "sentinel")
	require.NoError(t, os.MkdirAll(sentinelDir, 0o755))
	sentinelPath := filepath.Join(sentinelDir, "secret.json")
	originalBytes := []byte(`{"do-not":"clobber"}`)
	require.NoError(t, os.WriteFile(sentinelPath, originalBytes, 0o644))

	malicious := []string{
		"..",
		"../../../etc/passwd",
		"foo/../bar",
		"/etc/passwd",
		"../sentinel",
		"",
	}
	for _, id := range malicious {
		t.Run("load_"+id, func(t *testing.T) {
			_, err := LoadRepoInstances(id)
			assert.Error(t, err, "LoadRepoInstances(%q) should fail", id)
		})
		t.Run("save_"+id, func(t *testing.T) {
			err := SaveRepoInstances(id, json.RawMessage(`[]`))
			assert.Error(t, err, "SaveRepoInstances(%q) should fail", id)
		})
		t.Run("update_"+id, func(t *testing.T) {
			err := UpdateRepoInstances(id, func(raw json.RawMessage) (json.RawMessage, error) {
				return raw, nil
			})
			assert.Error(t, err, "UpdateRepoInstances(%q) should fail", id)
		})
		t.Run("delete_"+id, func(t *testing.T) {
			err := DeleteRepoInstances(id)
			assert.Error(t, err, "DeleteRepoInstances(%q) should fail", id)
		})
	}

	// Sentinel must be untouched.
	after, err := os.ReadFile(sentinelPath)
	require.NoError(t, err)
	assert.Equal(t, originalBytes, after, "sentinel file should not have been modified")
}

// TestLoadRepoInstances_SurfacesReadError verifies that an existing-but-
// unreadable instances.json produces an error rather than a silent empty list.
// This is the load-side guarantee behind #766: callers must be able to tell
// "no sessions" apart from "couldn't read sessions" so read-modify-write paths
// don't clobber present-but-unreadable data. A missing file is a separate case
// (covered elsewhere) and must continue to yield "[]" with no error.
func TestLoadRepoInstances_SurfacesReadError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based permission denial is ineffective when running as root")
	}
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/path/to/repo")
	require.NoError(t, SaveRepoInstances(repoID, json.RawMessage(`[{"title":"keep-me"}]`)))

	path, err := repoInstancesPath(repoID)
	require.NoError(t, err)

	// Make the file unreadable, simulating a transient permission/I/O error.
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err = LoadRepoInstances(repoID)
	require.Error(t, err, "an unreadable instances.json must surface an error, not an empty list")
	assert.False(t, os.IsNotExist(err), "error must be a read/permission failure, not a missing-file case")
}

// TestSaveRepoInstances_RoundTrip is a smoke test that the legitimate path
// still works end-to-end after the validation change.
func TestSaveRepoInstances_RoundTrip(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/path/to/repo")
	payload := json.RawMessage(`[{"title":"hello"}]`)

	require.NoError(t, SaveRepoInstances(repoID, payload))
	got, err := LoadRepoInstances(repoID)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(got))
}

func TestSaveStateWritesSchemaVersion(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	require.NoError(t, SaveState(&State{HelpScreensSeen: 7}))

	raw, err := os.ReadFile(filepath.Join(tempHome, StateFileName))
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":1,"help_screens_seen":7}`, string(raw))

	loaded := LoadState()
	assert.Equal(t, StateSchemaVersion, loaded.SchemaVersion)
	assert.Equal(t, uint32(7), loaded.HelpScreensSeen)
}
