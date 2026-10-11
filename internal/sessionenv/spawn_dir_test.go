//go:build !windows

package sessionenv

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/shellquote"

	"github.com/stretchr/testify/require"
)

// TestSpawnDirPrefix pins the launch-line contract: the directory travels as a
// leading NAME=value assignment — the one channel every supported tmux
// carries — resolved by the daemon's cwd (filepath.Abs) so the pane-side
// shim's chdir names the directory af admitted, not whatever the pane's
// fallback cwd would resolve a relative path against (#5174 review).
func TestSpawnDirPrefix(t *testing.T) {
	require.Empty(t, SpawnDirPrefix(""), "no directory, no assignment")

	prefix := SpawnDirPrefix("rel dir")
	abs, err := filepath.Abs("rel dir")
	require.NoError(t, err)
	require.Equal(t, SpawnDirEnv+"="+shellquote.Quote(abs)+" ", prefix,
		"absolute, shell-quoted, trailing-space-separated")
}

// TestEnterSpawnDirChdir binds the shim's behavior to the variable: unset —
// a launcher that predates the prefix — does nothing; set — the pane must be
// moved into exactly that directory before the command execs; a chdir failure
// is reported so the caller can stamp the refusal verdict.
func TestEnterSpawnDirChdir(t *testing.T) {
	require.NoError(t, os.Unsetenv(SpawnDirEnv))
	require.NoError(t, enterSpawnDir(), "an unset SpawnDirEnv enforces nothing")

	want := t.TempDir()
	t.Setenv(SpawnDirEnv, want)

	var got string
	prev := spawnDirChdir
	spawnDirChdir = func(dir string) error {
		got = dir
		return nil
	}
	t.Cleanup(func() { spawnDirChdir = prev })

	require.NoError(t, enterSpawnDir())
	require.Equal(t, want, got, "the shim chdirs to the recorded directory")

	failure := errors.New("permission denied")
	spawnDirChdir = func(string) error { return failure }
	require.ErrorIs(t, enterSpawnDir(), failure,
		"a failed chdir reports — the caller exits with the refusal verdict")
}
