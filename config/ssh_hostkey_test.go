package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateConfigSSHHostKeyVerification pins the warn-and-default behavior for
// ssh_host_key_verification (#2556): empty means "unset → default strict" (no
// warning), a valid value passes through, and any invalid hand-edited value falls
// back to strict — a bad edit never silently WEAKENS host-key verification.
func TestValidateConfigSSHHostKeyVerification(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", SSHHostKeyStrict},
		{SSHHostKeyStrict, SSHHostKeyStrict},
		{SSHHostKeyAcceptNew, SSHHostKeyAcceptNew},
		{SSHHostKeyInsecure, SSHHostKeyInsecure},
		{"bogus", SSHHostKeyStrict},
		{"STRICT", SSHHostKeyStrict}, // case-sensitive: unknown → default
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.SSHHostKeyVerification = tc.in
			out, err := validateConfig(cfg, "test-config")
			require.NoError(t, err)
			assert.Equal(t, tc.want, out.SSHHostKeyVerification)
		})
	}
	// The value we fall back to must itself be a valid mode, or the fallback is a
	// lie (the same guarantee TestSanitizeDetachKeys makes for detach_keys).
	assert.True(t, IsValidSSHHostKeyVerification(SSHHostKeyStrict), "default fallback must be valid")
	// And the manifest/DefaultConfig default must be that same safe value.
	assert.Equal(t, SSHHostKeyStrict, DefaultConfig().SSHHostKeyVerification)
}
