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
		explicitHome   bool
	}{
		{name: "fresh home"},
		{
			name:           "legacy 0755 home with existing config",
			legacyConfig:   "default_program = 'codex'\n",
			defaultProgram: "codex",
		},
		{
			name:           "legacy 0755 home explicitly pinned as AGENT_FACTORY_HOME",
			legacyConfig:   "default_program = 'codex'\n",
			defaultProgram: "codex",
			explicitHome:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userHome := t.TempDir()
			afHome := filepath.Join(userHome, ".agent-factory")
			t.Setenv("HOME", userHome)
			if tc.explicitHome {
				t.Setenv("AGENT_FACTORY_HOME", afHome)
			} else {
				t.Setenv("AGENT_FACTORY_HOME", "")
			}
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

// An explicit AGENT_FACTORY_HOME may spell the default through a symlinked
// ancestor (including macOS /var -> /private/var). Path identity, not the raw
// spelling, decides whether the legacy default-home repair applies.
func TestLoadConfigSecuresExplicitDefaultAFHomeThroughSymlink(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real-home")
	require.NoError(t, os.Mkdir(realHome, 0o755))
	linkHome := filepath.Join(base, "link-home")
	require.NoError(t, os.Symlink(realHome, linkHome))

	afHome := filepath.Join(realHome, ".agent-factory")
	require.NoError(t, os.Mkdir(afHome, 0o755))
	require.NoError(t, os.Chmod(afHome, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(afHome, TomlConfigFileName),
		[]byte("default_program = 'codex'\n"), 0o644))
	t.Setenv("HOME", realHome)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(linkHome, ".agent-factory"))
	fastShell(t)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, "codex", cfg.DefaultProgram,
		"securing the symlink-spelled default must preserve its config")
	requireMode(t, afHome, 0o700)
}

// A final-component alias pointing INTO a concrete default home is another
// spelling of the directory AF owns. Repair the concrete default path rather
// than refusing the alias merely because chmod would follow it.
func TestLoadConfigRepairsExplicitAliasToConcreteDefaultAFHome(t *testing.T) {
	base := t.TempDir()
	userHome := filepath.Join(base, "home")
	require.NoError(t, os.Mkdir(userHome, 0o755))
	afHome := filepath.Join(userHome, ".agent-factory")
	require.NoError(t, os.Mkdir(afHome, 0o755))
	require.NoError(t, os.Chmod(afHome, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(afHome, TomlConfigFileName),
		[]byte("default_program = 'codex'\n"), 0o644))
	alias := filepath.Join(base, "af-home-alias")
	require.NoError(t, os.Symlink(afHome, alias))
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", alias)
	fastShell(t)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, "codex", cfg.DefaultProgram)
	requireMode(t, afHome, 0o700)
}

// Direction matters when the default name is itself a symlink. Its target is
// caller-owned; pointing the default name at a broad directory must not grant AF
// permission to chmod that directory when AGENT_FACTORY_HOME names it directly.
func TestAtomicWriteFilePreservesCustomTargetOfDefaultSymlink(t *testing.T) {
	base := t.TempDir()
	userHome := filepath.Join(base, "home")
	require.NoError(t, os.Mkdir(userHome, 0o755))
	customHome := filepath.Join(base, "shared-af-home")
	require.NoError(t, os.Mkdir(customHome, 0o755))
	require.NoError(t, os.Chmod(customHome, 0o755))
	require.NoError(t, os.Symlink(customHome, filepath.Join(userHome, ".agent-factory")))
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", customHome)

	path := filepath.Join(customHome, "state.json")
	require.NoError(t, AtomicWriteFile(path, []byte("{}"), 0o644))
	require.FileExists(t, path)
	requireMode(t, customHome, 0o755)
}

// When AF is addressed through a default-name symlink, preserve the permissive
// target rather than chmodding caller-owned storage or failing startup. The safe
// alias repair above must not reverse this direction.
func TestAtomicWriteFilePreservesPermissiveDefaultSymlinkTarget(t *testing.T) {
	base := t.TempDir()
	userHome := filepath.Join(base, "home")
	require.NoError(t, os.Mkdir(userHome, 0o755))
	customHome := filepath.Join(base, "shared-af-home")
	require.NoError(t, os.Mkdir(customHome, 0o755))
	require.NoError(t, os.Chmod(customHome, 0o755))
	defaultHome := filepath.Join(userHome, ".agent-factory")
	require.NoError(t, os.Symlink(customHome, defaultHome))
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "")

	path := filepath.Join(defaultHome, "state.json")
	require.NoError(t, AtomicWriteFile(path, []byte("{}"), 0o644))
	require.FileExists(t, path)
	requireMode(t, customHome, 0o755)
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
