package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// This tests syscall routing, not live Linux capability enforcement. In
// particular, a mocked success must not be described as a capability test.
func TestLoadConfigReadOnly_UsesEffectiveKernelAccess(t *testing.T) {
	fastShell(t)
	home := seedHome(t, "# placeholder\n")
	oldEffective, oldFallback := configDirectoryFaccessat2, configDirectoryFaccessat
	t.Cleanup(func() {
		configDirectoryFaccessat2, configDirectoryFaccessat = oldEffective, oldFallback
	})
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"permission denied", unix.EACCES},
		{"read-only filesystem", unix.EROFS},
		{"IO error", unix.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			configDirectoryFaccessat2 = func(dirfd int, path string, mode uint32, flags int) error {
				calls++
				assert.Equal(t, unix.AT_FDCWD, dirfd)
				assert.Equal(t, home, path)
				assert.Equal(t, uint32(unix.W_OK|unix.X_OK), mode)
				assert.Equal(t, unix.AT_EACCESS, flags)
				return tc.err
			}
			configDirectoryFaccessat = func(int, string, uint32, int) error {
				t.Error("must not fall back after a supported effective-access check")
				return tc.err
			}
			loaded, err := LoadConfigReadOnly()
			assert.Equal(t, 1, calls, "must ask the kernel about effective access even when IDs match")
			if tc.err == nil {
				require.NoError(t, err)
				assert.True(t, loaded.EmptyStub)
			} else {
				require.ErrorIs(t, err, tc.err)
				assert.False(t, loaded.EmptyStub)
			}
		})
	}
}

// This checks fallback selection with injected capability metadata; it does
// not provision capabilities or test their enforcement by the Linux kernel.
func TestConfigDirectoryAccess_FallbackRetainsEffectiveRouting(t *testing.T) {
	oldEffective, oldFallback, oldCapget := configDirectoryFaccessat2, configDirectoryFaccessat, configDirectoryCapget
	t.Cleanup(func() {
		configDirectoryFaccessat2, configDirectoryFaccessat, configDirectoryCapget = oldEffective, oldFallback, oldCapget
	})
	for _, unavailable := range []error{unix.ENOSYS, unix.EPERM} {
		for _, tc := range []struct {
			name string
			data unix.CapUserData
			err  error
		}{
			{"effective capability", unix.CapUserData{Effective: 1 << unix.CAP_DAC_OVERRIDE}, nil},
			{"permitted capability", unix.CapUserData{Permitted: 1 << unix.CAP_DAC_OVERRIDE}, nil},
			{"capget denied", unix.CapUserData{}, unix.EPERM},
		} {
			t.Run(unavailable.Error()+"/"+tc.name, func(t *testing.T) {
				configDirectoryFaccessat2 = func(int, string, uint32, int) error { return unavailable }
				configDirectoryCapget = func(_ *unix.CapUserHeader, data *unix.CapUserData) error {
					*data = tc.data
					return tc.err
				}
				called := false
				configDirectoryFaccessat = func(_ int, _ string, _ uint32, flags int) error {
					called = true
					assert.Equal(t, unix.AT_EACCESS, flags, "must not switch to real-ID capability semantics")
					return unix.EACCES
				}
				require.ErrorIs(t, checkConfigDirectoryAccess(t.TempDir()), unix.EACCES)
				assert.True(t, called)
			})
		}
	}
}
