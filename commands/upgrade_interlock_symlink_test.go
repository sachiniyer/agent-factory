package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The in-place installer refuses a symlinked destination (#3672).
//
// Both production callers hand this an already-EvalSymlinks'd path, so a link at
// the final component means it BECAME one after that resolution — the state
// where swapping an executable is least safe, and exactly the kind of thing this
// file's fail-closed polarity exists for. Replacing it would also silently
// destroy a `~/.local/bin/af -> …` arrangement some installs use.
func TestWriteExecutableInPlace_RefusesASymlinkedDestination(t *testing.T) {
	upgradeHome(t)

	real := filepath.Join(t.TempDir(), "af-real")
	require.NoError(t, os.WriteFile(real, []byte("old binary"), 0o755))
	link := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.Symlink(real, link))

	err := writeExecutableInPlace(link, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag)
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link, "the error names the link")
	assert.Contains(t, err.Error(), real, "and the target it points at")

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink, "the link survives")

	on, rerr := os.ReadFile(real)
	require.NoError(t, rerr)
	assert.Equal(t, "old binary", string(on),
		"and the binary it points at was not swapped underneath it either")
}
