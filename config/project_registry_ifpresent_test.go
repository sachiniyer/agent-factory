package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListProjectsIfPresentDistinguishesAbsenceFromEmpty: absence reports
// present=false with no error — the distinction the daemon's fail-closed
// recovery keys on (#3315 review) — while a present registry reports its true
// contents.
func TestListProjectsIfPresentDistinguishesAbsenceFromEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	_, present, err := ListProjectsIfPresent()
	require.NoError(t, err)
	assert.False(t, present, "an absent registry must not read as an empty one")

	dir, err := ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	projects, present, err := ListProjectsIfPresent()
	require.NoError(t, err)
	assert.True(t, present)
	assert.Empty(t, projects)

	root := initProjectRegistryRepo(t, filepath.Join(t.TempDir(), "repo"))
	registered, err := RegisterProject(root)
	require.NoError(t, err)
	projects, present, err = ListProjectsIfPresent()
	require.NoError(t, err)
	assert.True(t, present)
	require.Len(t, projects, 1)
	assert.Equal(t, registered.ID, projects[0].ID)
}

// TestListProjectsIfPresentRejectsDanglingSymlink pins #3317: a registry
// symlink whose target is temporarily unavailable is not a present, empty
// registry. Recovery must keep treating it as unavailable until the resolved
// target is a directory again.
func TestListProjectsIfPresentRejectsDanglingSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	dir, err := ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "unavailable"), dir))

	projects, present, err := ListProjectsIfPresent()
	require.NoError(t, err)
	assert.False(t, present, "a dangling registry symlink must remain unavailable")
	assert.Empty(t, projects)
}
