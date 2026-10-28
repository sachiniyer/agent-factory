package autoupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The check cache refuses a symlinked path (#3672). It is af's own throttle
// bookkeeping at a path af chose — nobody authors it, so a link there is a
// surprise to report rather than an arrangement to honour in either direction.
func TestCheckCacheRecordRefusesASymlinkedCacheFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "someone-elses.json")
	require.NoError(t, os.WriteFile(target, []byte("{\"keep\":true}\n"), 0644))
	link := filepath.Join(t.TempDir(), "update-check.json")
	require.NoError(t, os.Symlink(target, link))

	err := NewCheckCache(link).Record("stable", "v1.2.3", "1.2.2", time.Now())
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), target)

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink)

	on, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, "{\"keep\":true}\n", string(on))
}
