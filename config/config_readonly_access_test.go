package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestLoadConfigReadOnly_EmptyStubOldKernelACL(t *testing.T) {
	if os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
		t.Skip("regression exercises matching real and effective credentials")
	}
	fastShell(t)
	home := seedHome(t, "# placeholder\n")
	original := configDirectoryFaccessat
	t.Cleanup(func() { configDirectoryFaccessat = original })

	for _, tc := range []struct {
		name      string
		kernelErr error
	}{
		{name: "ACL grants access"},
		{name: "ACL denies access", kernelErr: unix.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			configDirectoryFaccessat = func(dirfd int, path string, mode uint32, flags int) error {
				called = true
				assert.Equal(t, unix.AT_FDCWD, dirfd)
				assert.Equal(t, home, path)
				assert.Equal(t, uint32(unix.W_OK|unix.X_OK), mode)
				// Model x/sys on Linux without faccessat2 (ENOSYS or EPERM):
				// AT_EACCESS falls back to mode bits that deny this user, even
				// when an ACL grants access. Flags 0 asks the kernel instead.
				if flags != 0 {
					return unix.EACCES
				}
				return tc.kernelErr
			}
			loaded, err := LoadConfigReadOnly()
			assert.True(t, called)
			if tc.kernelErr == nil {
				assert.NoError(t, err, "an ACL grant must survive the older-kernel fallback")
				assert.True(t, loaded.EmptyStub)
				assert.NotNil(t, loaded.Config)
			} else {
				require.ErrorIs(t, err, os.ErrPermission)
				assert.Contains(t, err.Error(), "cannot write to config directory "+prettyHomePath(home))
				assert.False(t, loaded.EmptyStub)
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
