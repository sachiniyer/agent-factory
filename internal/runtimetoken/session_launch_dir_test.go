//go:build !windows

package runtimetoken

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two sessions on one host share one cached af binary (#5571 W2.2). Each
// session's <sessionDir>/af is a symlink to it, and each session dir holds its
// own token. Neither may read the other's, and the host-wide cache directory is
// never consulted.
func TestSuppliedAgentServerTokenIsKeyedToTheSessionLaunchPath(t *testing.T) {
	// Resolved at creation: the product compares resolved paths, and macOS
	// hands out /var/... for what is really /private/var/... (#5903).
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cache := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	shared := filepath.Join(cache, "af-1.0.328-linux-amd64")
	require.NoError(t, os.WriteFile(shared, []byte("binary"), 0o755))
	// A token in the shared cache directory must never be discovered.
	require.NoError(t, os.WriteFile(filepath.Join(cache, TokenFileName), []byte("host-wide-token\n"), 0o600))

	sessions := map[string]string{"a": "token-of-session-a", "b": "token-of-session-b"}
	launch := map[string]string{}
	for name, tok := range sessions {
		dir := filepath.Join(root, "sess-"+name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Symlink(shared, filepath.Join(dir, "af")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, TokenFileName), []byte(tok+"\n"), 0o600))
		launch[name] = filepath.Join(dir, "af")
	}

	prevExe, prevArgv0 := executable, argv0
	t.Cleanup(func() { executable, argv0 = prevExe, prevArgv0 })
	// os.Executable on Linux resolves the symlink: both sessions report the
	// shared cache path.
	resolved, err := filepath.EvalSymlinks(shared)
	require.NoError(t, err)
	executable = func() (string, error) { return resolved, nil }

	for name, want := range sessions {
		argv0 = func() string { return launch[name] }
		got, err := SuppliedAgentServerToken()
		require.NoError(t, err)
		assert.Equal(t, want, got, "session %s must read its own token", name)
		other := "token-of-session-b"
		if name == "b" {
			other = "token-of-session-a"
		}
		assert.NotEqual(t, other, got, "session %s must never read the other session's token", name)
		assert.NotEqual(t, "host-wide-token", got, "the shared cache directory is never a token source")
	}

	// A launch path that does not name the running binary (spoofed or stale)
	// is ignored: the lookup falls back to the executable's directory, which
	// holds the host-wide file — documented pre-W2.2 behavior — not to a
	// directory the process merely claims.
	stranger := filepath.Join(root, "stranger")
	require.NoError(t, os.MkdirAll(stranger, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stranger, "af"), []byte("different binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stranger, TokenFileName), []byte("stranger-token\n"), 0o600))
	assert.Equal(t, cache, filepath.Dir(resolved))
	assert.Equal(t, filepath.Dir(resolved), sessionLaunchDir(resolved, filepath.Join(stranger, "af")))
	assert.Equal(t, filepath.Dir(resolved), sessionLaunchDir(resolved, "af"), "a relative launch name is not trusted")
}
