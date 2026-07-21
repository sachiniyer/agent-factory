package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadConfigSecuresDefaultAFHome is the #2197 regression. The default
// first-run path materializes config.toml before token creation, so it must
// create the secret-bearing AF home owner-only and repair homes that an older
// version already created 0755. A later MkdirAll(0700) does not tighten an
// existing directory.
func TestLoadConfigSecuresDefaultAFHome(t *testing.T) {
	for _, tc := range []struct {
		name           string
		legacyConfig   string
		defaultProgram string
	}{
		{name: "fresh home"},
		{
			name:           "legacy 0755 home with existing config",
			legacyConfig:   "default_program = 'codex'\n",
			defaultProgram: "codex",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userHome := t.TempDir()
			afHome := filepath.Join(userHome, ".agent-factory")
			t.Setenv("HOME", userHome)
			t.Setenv("AGENT_FACTORY_HOME", "")
			fastShell(t)

			if tc.legacyConfig != "" {
				require.NoError(t, os.Mkdir(afHome, 0o755))
				require.NoError(t, os.Chmod(afHome, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(afHome, TomlConfigFileName),
					[]byte(tc.legacyConfig), 0o644))
			}

			cfg, err := LoadConfig()
			require.NoError(t, err)
			if tc.defaultProgram != "" {
				require.Equal(t, tc.defaultProgram, cfg.DefaultProgram,
					"securing a legacy home must not replace its existing config")
			}

			info, err := os.Stat(afHome)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
				"the AF home contains world-readable config/state files and bearer credentials")
		})
	}
}

func TestAtomicWriteFileCreatesMissingCustomAFHomePrivate(t *testing.T) {
	userHome := t.TempDir()
	afHome := filepath.Join(userHome, "custom-af-home")
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	require.NoError(t, AtomicWriteFile(filepath.Join(afHome, "state.json"), []byte("{}"), 0o644))
	requireMode(t, afHome, 0o700)
}

// TestAtomicWriteFileSecuresOnlyAFHome pins the shared primitive that keeps a
// state/task/token writer from recreating the same bug through another first
// write. Its permission tightening is scoped: AtomicWriteFile also serves
// autostart, upgrade, and repo-plugin paths whose parent modes it must preserve.
func TestAtomicWriteFileSecuresOnlyAFHome(t *testing.T) {
	userHome := t.TempDir()
	afHome := filepath.Join(userHome, ".agent-factory")
	outside := filepath.Join(userHome, "outside")
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "")

	require.NoError(t, AtomicWriteFile(filepath.Join(afHome, "state.json"), []byte("{}"), 0o644))
	requireMode(t, afHome, 0o700)

	require.NoError(t, os.Mkdir(outside, 0o755))
	require.NoError(t, os.Chmod(outside, 0o755))
	require.NoError(t, AtomicWriteFile(filepath.Join(outside, "artifact"), []byte("ok"), 0o644))
	requireMode(t, outside, 0o755)
}

// AGENT_FACTORY_HOME explicitly supports broad caller-owned directories such
// as "~". Securing the default must not chmod that directory as a side effect.
func TestAtomicWriteFilePreservesExistingCustomAFHomeMode(t *testing.T) {
	userHome := t.TempDir()
	require.NoError(t, os.Chmod(userHome, 0o755))
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "~")

	path := filepath.Join(userHome, "state.json")
	require.NoError(t, AtomicWriteFile(path, []byte("{}"), 0o644))
	require.FileExists(t, path)
	requireMode(t, userHome, 0o755)
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm())
}
