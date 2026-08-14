package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the #3297 partial-read contract at its source: err reports
// only a failed ENUMERATION, per-record failures are isolated and named,
// strays are reported without failing anything — and the repair tooling keeps
// working while a bad record exists.

func registryGranularityFixture(t *testing.T) (home string, goodRoot string, good Project, badRoot string, bad Project) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	goodRoot = initProjectRegistryRepo(t, filepath.Join(t.TempDir(), "good"))
	badRoot = initProjectRegistryRepo(t, filepath.Join(t.TempDir(), "bad"))
	var err error
	good, err = RegisterProject(goodRoot)
	require.NoError(t, err)
	bad, err = RegisterProject(badRoot)
	require.NoError(t, err)
	dir, err := ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, bad.ID, projectMetadataFileName), []byte("{ not json"), 0o644))
	return home, goodRoot, good, badRoot, bad
}

// TestDeregisterProjectSurvivesUnrelatedCorruptRecord: removing a readable
// record must work while an unrelated record is corrupt — the registry's own
// repair tooling must not be bricked by the thing it would repair. A target
// that is NOT found while failures exist stays an error naming them, because
// "nothing to remove" is unprovable there.
func TestDeregisterProjectSurvivesUnrelatedCorruptRecord(t *testing.T) {
	_, goodRoot, good, _, bad := registryGranularityFixture(t)

	removed, err := DeregisterProject(goodRoot)
	require.NoError(t, err, "an unrelated corrupt record must not brick deregistration")
	assert.True(t, removed)
	dir, err := ProjectRegistryDir()
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, good.ID))
	assert.True(t, os.IsNotExist(statErr), "the readable record must be gone")

	// The corrupt record cannot be matched by path, so a lookup that finds
	// nothing readable must refuse with the failures named.
	_, err = DeregisterProject(filepath.Join(t.TempDir(), "elsewhere"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), bad.ID)
}

// TestRegisterProjectRefusesNamingCorruptRecords: registration scans every
// record for collisions, so an unreadable record makes uniqueness unprovable
// — the refusal names the exact directory to repair instead of a bare parse
// error.
func TestRegisterProjectRefusesNamingCorruptRecords(t *testing.T) {
	_, _, _, _, bad := registryGranularityFixture(t)
	fresh := initProjectRegistryRepo(t, filepath.Join(t.TempDir(), "fresh"))

	_, err := RegisterProject(fresh)
	require.Error(t, err)
	assert.Contains(t, err.Error(), bad.ID)
	assert.Contains(t, err.Error(), "repair or remove")
}
