package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestLoadConfigReadOnly_DirectoryAccessAdvisory(t *testing.T) {
	fastShell(t)
	original := configDirectoryFaccessat2
	t.Cleanup(func() { configDirectoryFaccessat2 = original })
	for _, tc := range []struct {
		name       string
		err        error
		unverified bool
	}{
		{"checked", nil, false},
		{"unavailable", unix.ENOSYS, true},
		{"ambiguous EPERM", unix.EPERM, true},
		{"denied", unix.EACCES, false},
		{"read-only mount", unix.EROFS, false},
		{"IO failure", unix.EIO, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := seedHome(t, "# placeholder\n")
			calls := 0
			configDirectoryFaccessat2 = func(dirfd int, path string, mode uint32, flags int) error {
				calls++
				assert.Equal(t, unix.AT_FDCWD, dirfd)
				assert.Equal(t, home, path)
				assert.Equal(t, uint32(unix.W_OK|unix.X_OK), mode)
				assert.Equal(t, unix.AT_EACCESS, flags)
				return tc.err
			}
			loaded, err := LoadConfigReadOnly()
			assert.Equal(t, 1, calls)
			switch {
			case tc.unverified:
				require.NoError(t, err)
				assert.True(t, loaded.EmptyStub)
				assert.NotNil(t, loaded.Config)
				assert.Contains(t, loaded.DirectoryAccessWarning, prettyHomePath(home))
				assert.Contains(t, loaded.DirectoryAccessWarning, tc.err.Error())
				assert.Contains(t, loaded.DirectoryAccessWarning, "could not be verified")
				assert.Contains(t, loaded.DirectoryAccessWarning, "startup will attempt regeneration")
			case tc.err != nil:
				require.ErrorIs(t, err, tc.err)
				assert.Contains(t, err.Error(), prettyHomePath(home))
				assert.False(t, loaded.EmptyStub)
				assert.Empty(t, loaded.DirectoryAccessWarning)
			default:
				require.NoError(t, err)
				assert.True(t, loaded.EmptyStub)
				assert.Empty(t, loaded.DirectoryAccessWarning, "checked and unverified must stay distinct")
			}
			got, err := os.ReadFile(filepath.Join(home, TomlConfigFileName))
			require.NoError(t, err)
			assert.Equal(t, "# placeholder\n", string(got))
			probes, err := filepath.Glob(filepath.Join(home, ".af-stub-check-*"))
			require.NoError(t, err)
			assert.Empty(t, probes)
		})
	}
}
