package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedHome writes content to config.toml in a fresh AGENT_FACTORY_HOME and
// returns that home. When json is non-empty it also writes config.json so the
// shadow case can be exercised.
func seedHome(t *testing.T, content string, json ...string) string {
	t.Helper()
	home := t.TempDir()
	tomlPath := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte(content), 0o644))
	for _, j := range json {
		require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(j), 0o644))
	}
	t.Setenv("AGENT_FACTORY_HOME", home)
	return home
}

// effectivelyEmptyTomlInputs is every shape isEffectivelyEmptyToml treats as
// empty — the fingerprint the bug report names. Each must self-heal at
// startup and report EmptyStub (not an error) from the read-only diagnostics.
func effectivelyEmptyTomlInputs() map[string]string {
	return map[string]string{
		"zero bytes":      "",
		"whitespace":      "   \n\n\t ",
		"bom only":        utf8BOM,
		"comment only":    "# TODO fill this in\n",
		"comments only":   "# header\n# another reminder\n",
		"bom and comment": utf8BOM + "# just a note\n",
	}
}

// TestLoadConfigReadOnly_EmptyStubAgreesWithStartup is the core guarantee: for
// every effectively-empty config.toml with no shadowing config.json, the read
// -only diagnostic (LoadConfigReadOnly) and startup (LoadConfig) must return the
// SAME verdict. Startup self-heals (removes the stub, materializes defaults,
// returns (*Config, nil)); the no-write diagnostic returns EmptyStub=true with a
// nil error. Before the fix the diagnostic raised a hard "config is empty"
// error, so `af doctor`/`af config validate` rejected a state af boots on.
//
// The two loaders run against separate identical homes because LoadConfig
// mutates disk (os.Remove + materializeDefaultConfig): the assertion is that
// the two functions return the same verdict on the same INPUT state.
func TestLoadConfigReadOnly_EmptyStubAgreesWithStartup(t *testing.T) {
	for name, content := range effectivelyEmptyTomlInputs() {
		t.Run(name, func(t *testing.T) {
			fastShell(t)

			// Read-only on a fresh home: the diagnostic verdict.
			homeRO := seedHome(t, content)
			tomlPath := filepath.Join(homeRO, TomlConfigFileName)
			loaded, roErr := LoadConfigReadOnly()
			require.NoError(t, roErr, "read-only diagnostic must not fail a state startup self-heals")
			assert.True(t, loaded.EmptyStub, "an effectively-empty stub with no config.json must surface as EmptyStub")
			assert.False(t, loaded.Missing, "a present stub is not Missing")
			assert.NotNil(t, loaded.Config, "EmptyStub carries DefaultConfig() so downstream diagnostics can evaluate the next-start posture")
			assert.Equal(t, tomlPath, loaded.Path)

			// No-write contract: the stub is intact and nothing was created.
			got, err := os.ReadFile(tomlPath)
			require.NoError(t, err)
			assert.Equal(t, content, string(got), "LoadConfigReadOnly must not remove or rewrite the stub")
			_, statErr := os.Stat(filepath.Join(homeRO, ConfigFileName))
			assert.True(t, os.IsNotExist(statErr), "LoadConfigReadOnly must not materialize a config.json")

			// Startup on a separate identical home: the self-heal verdict.
			homeLC := seedHome(t, content)
			cfg, lcErr := LoadConfig()
			require.NoError(t, lcErr, "startup must self-heal the empty stub")
			require.NotNil(t, cfg, "startup must return a real (default) config after self-healing")
			lcAfter, err := os.ReadFile(filepath.Join(homeLC, TomlConfigFileName))
			require.NoError(t, err)
			assert.NotEmpty(t, lcAfter, "startup must re-materialize non-empty defaults over the stub")
		})
	}
}

// TestLoadConfigReadOnly_EmptyStubDoesNotMutate pins the read-only contract on a
// representative effectively-empty stub and asserts nothing on disk changed:
// the stub stays, no config.json appears, and no lock/backup artifacts were
// left behind. LoadConfigReadOnly backs `af config validate`, whose stated
// contract is "It writes nothing and materializes nothing — a read-only check".
func TestLoadConfigReadOnly_EmptyStubDoesNotMutate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, TomlConfigFileName)
	content := []byte("# placeholder; fill in later\n")
	require.NoError(t, os.WriteFile(tomlPath, content, 0o644))

	before, err := os.ReadDir(home)
	require.NoError(t, err)

	loaded, err := LoadConfigReadOnly()
	require.NoError(t, err)
	require.True(t, loaded.EmptyStub)

	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	require.Equal(t, content, got, "validate must not rewrite the stub it checks")

	after, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Len(t, after, len(before), "validate must leave no new files (no config.json, no lock, no backup)")
	probes, err := filepath.Glob(filepath.Join(home, ".af-stub-check-*"))
	require.NoError(t, err)
	assert.Empty(t, probes, "read-only validation must leave no probe files")
}

// TestLoadConfigReadOnly_EmptyStubWithShadowStaysLoudError mirrors loadConfig:
// an effectively-empty config.toml sitting beside a real config.json is a
// hand-made shadow, and re-materializing would silently discard the config.json
// settings — so BOTH paths keep it a loud error. The fix must NOT downgrade the
// shadow case to EmptyStub.
func TestLoadConfigReadOnly_EmptyStubWithShadowStaysLoudError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, TomlConfigFileName)
	jsonPath := filepath.Join(home, ConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte("# oops, empty\n"), 0o644))
	// A real, parseable config.json so the shadow contains actual settings.
	require.NoError(t, os.WriteFile(jsonPath, []byte(`{"schema_version":1,"default_program":"claude"}`), 0o644))

	loaded, roErr := LoadConfigReadOnly()
	require.Error(t, roErr, "an empty toml shadowing a real config.json must stay a loud error")
	assert.False(t, loaded.EmptyStub, "the shadow case must not be downgraded to EmptyStub")
	assert.Contains(t, roErr.Error(), "is empty")

	// And the verdict matches startup, which also keeps it a loud error.
	fastShell(t)
	_, lcErr := LoadConfig()
	require.Error(t, lcErr, "startup must also keep the shadow case a loud error (do not self-heal over config.json)")

	// Neither loader mutated the shadow.
	got, err := os.ReadFile(jsonPath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "default_program", "the config.json must be left intact")
	stub, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# oops, empty\n", string(stub), "the empty toml stub must be left intact")
}

// TestLoadConfigReadOnly_EmptyStubUnremovableDirIsError pins the case where
// config.toml is readable but the containing directory is not writable, so
// startup's os.Remove(tomlPath) would fail. The diagnostic must NOT return
// EmptyStub (which implies "af will self-heal") when self-heal is impossible;
// it must identify the directory permission failure without claiming a removal.
func TestLoadConfigReadOnly_EmptyStubUnremovableDirIsError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a read-only directory cannot be staged here")
	}
	// Place the config inside a custom home whose parent is not writable.
	outer := t.TempDir()
	home := filepath.Join(outer, "af-home")
	require.NoError(t, os.Mkdir(home, 0o755))
	tomlPath := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte("# placeholder\n"), 0o644))
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Make the home directory read+execute only so os.Remove(tomlPath) would
	// fail (remove requires write permission on the containing directory).
	require.NoError(t, os.Chmod(home, 0o500))
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	loaded, roErr := LoadConfigReadOnly()
	require.Error(t, roErr, "an empty stub in a non-writable home must be a loud error, not EmptyStub")
	assert.False(t, loaded.EmptyStub, "EmptyStub must not be set when the home is not writable (self-heal would fail)")
	assert.Contains(t, roErr.Error(), "cannot write to config directory "+prettyHomePath(home))
	assert.ErrorIs(t, roErr, os.ErrPermission)
	assert.NotContains(t, roErr.Error(), TomlConfigFileName)
	assert.NotContains(t, roErr.Error(), ".af-stub-check-")
	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# placeholder\n", string(got))
	probes, err := filepath.Glob(filepath.Join(home, ".af-stub-check-*"))
	require.NoError(t, err)
	assert.Empty(t, probes, "failed validation must leave no probe files")
}

// TestLoadConfigReadOnly_EmptySymlinkStubStaysLoudError mirrors loadConfig: a
// symlink config.toml whose target is effectively empty (and no config.json)
// is a hand-made arrangement, not a failed first-run write — so BOTH paths keep
// it a loud error rather than unlinking the operator's dotfiles. The fix must
// NOT upgrade the symlink-stub case to EmptyStub.
func TestLoadConfigReadOnly_EmptySymlinkStubStaysLoudError(t *testing.T) {
	home, dotfiles := t.TempDir(), t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	target := filepath.Join(dotfiles, "empty.toml")
	require.NoError(t, os.WriteFile(target, []byte(""), 0o644))
	link := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(target, link))

	loaded, roErr := LoadConfigReadOnly()
	require.Error(t, roErr, "a symlink stub to an empty target must stay a loud error")
	assert.False(t, loaded.EmptyStub, "the symlink-stub case must not be downgraded to EmptyStub")
	assert.False(t, loaded.Missing)
	assert.Contains(t, roErr.Error(), "is empty")

	_, lcErr := LoadConfig()
	require.Error(t, lcErr, "startup must also keep the symlink stub a loud error")

	// Neither loader unlinked the operator's symlink.
	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink, "the symlink must be left in place")
	_, err = os.Stat(target)
	require.NoError(t, err, "the dotfiles target must be left in place")
}

// stageDefaultAFHome stages the CONCRETE default ~/.agent-factory under a fake
// $HOME with AGENT_FACTORY_HOME empty (treated as unset by ConfigDirFor), so
// concreteDefaultAFHome is genuinely exercised against the real default name —
// the arrangement seedHome cannot reach, because seedHome sets
// AGENT_FACTORY_HOME and so pins a CUSTOM home the diagnostic never repairs.
// When mode is more restrictive than the default it also registers a cleanup
// restoring writability so t.TempDir can remove the tree. Returns the AF home
// path (the .agent-factory directory itself).
func stageDefaultAFHome(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	fastShell(t)
	userHome := t.TempDir()
	afHome := filepath.Join(userHome, ".agent-factory")
	require.NoError(t, os.Mkdir(afHome, 0o755))
	tomlPath := filepath.Join(afHome, TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte(content), 0o644))
	require.NoError(t, os.Chmod(afHome, mode))
	if mode&0o200 == 0 {
		// A read-only home blocks t.TempDir's recursive cleanup; restore
		// owner-write first so the temp tree can be removed.
		t.Cleanup(func() { _ = os.Chmod(afHome, 0o755) })
	}
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "")
	return afHome
}

// TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyRepairable is the headline
// fix: a contentless config.toml in an owner-owned default ~/.agent-factory
// tightened to a write-less mode (0500) is a state startup self-heals —
// secureAFHomeForPath chmod-repairs the home to 0700 before its os.Remove. The
// no-write diagnostic, which never runs secureAFHomeForPath, must NOT report a
// removal failure here: it must agree with startup and return EmptyStub=true
// with no error. Before the fix the directory-access gate probed the unrepaired
// 0500 mode and errored with "cannot write to config directory: permission
// denied" on a state af boots cleanly on.
func TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyRepairable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0500 home cannot be staged as non-writable")
	}
	afHome := stageDefaultAFHome(t, "# placeholder; fill in later\n", 0o500)
	tomlPath := filepath.Join(afHome, TomlConfigFileName)

	loaded, err := LoadConfigReadOnly()
	require.NoError(t, err, "a chmod-repairable default home self-heals at startup; the diagnostic must agree")
	assert.True(t, loaded.EmptyStub, "an effectively-empty stub in a repairable default home must surface as EmptyStub")
	assert.False(t, loaded.Missing)
	assert.NotNil(t, loaded.Config, "EmptyStub carries DefaultConfig() so downstream diagnostics can evaluate the next-start posture")
	assert.Empty(t, loaded.DirectoryAccessWarning, "startup will repair the home; there is no access uncertainty to report")
	assert.Equal(t, tomlPath, loaded.Path)

	// No-write contract: the stub is intact, the home stays read-only, nothing
	// was created.
	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# placeholder; fill in later\n", string(got), "LoadConfigReadOnly must not remove or rewrite the stub")
	info, err := os.Stat(afHome)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "the diagnostic must not chmod-repair the home it reports on")
	entries, err := os.ReadDir(afHome)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the diagnostic must not materialize any file beside the stub")
}

// TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyAgreesWithStartup pins the
// full guarantee the bug broke: the read-only diagnostic and startup must agree
// on identical INPUT states. The two run against separate staged default homes
// because LoadConfig mutates disk. On a default home @0500 the diagnostic must
// return EmptyStub=true (no error) AND startup must self-heal (no error) — the
// same verdict — rather than the diagnostic erroring while startup succeeds.
func TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyAgreesWithStartup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0500 home cannot be staged as non-writable")
	}
	for name, content := range effectivelyEmptyTomlInputs() {
		t.Run(name, func(t *testing.T) {
			// Read-only on a fresh default home @0500: the diagnostic verdict.
			afHomeRO := stageDefaultAFHome(t, content, 0o500)
			loaded, roErr := LoadConfigReadOnly()
			require.NoError(t, roErr, "read-only diagnostic must not fail a state startup self-heals")
			assert.True(t, loaded.EmptyStub)
			got, err := os.ReadFile(filepath.Join(afHomeRO, TomlConfigFileName))
			require.NoError(t, err)
			assert.Equal(t, content, string(got), "LoadConfigReadOnly must not mutate the stub")

			// Startup on a separate identical default home @0500: the
			// self-heal verdict. secureAFHomeForPath chmod-repairs to 0700,
			// removes the stub, and materializes non-empty defaults.
			afHomeLC := stageDefaultAFHome(t, content, 0o500)
			cfg, lcErr := LoadConfig()
			require.NoError(t, lcErr, "startup must self-heal the empty stub in a repairable default home")
			require.NotNil(t, cfg)
			info, err := os.Stat(afHomeLC)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "startup chmod-repairs the default home before removing the stub")
			lcAfter, err := os.ReadFile(filepath.Join(afHomeLC, TomlConfigFileName))
			require.NoError(t, err)
			assert.NotEmpty(t, lcAfter, "startup must re-materialize non-empty defaults over the stub")
		})
	}
}

// TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyAliasSymlink covers the
// other repair arrangement secureAFHomeForPath chmods: an AGENT_FACTORY_HOME
// that is an alias symlink whose target IS the concrete default home (the
// "pin the default explicitly" case its own comments anticipate). The gate must
// stand down here too, so the diagnostic agrees with startup's symlink-target
// chmod repair.
func TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyAliasSymlink(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0500 home cannot be staged as non-writable")
	}
	fastShell(t)
	userHome := t.TempDir()
	afHome := filepath.Join(userHome, ".agent-factory")
	require.NoError(t, os.Mkdir(afHome, 0o755))
	tomlPath := filepath.Join(afHome, TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte("# placeholder\n"), 0o644))
	require.NoError(t, os.Chmod(afHome, 0o500))
	t.Cleanup(func() { _ = os.Chmod(afHome, 0o755) })

	// An alias symlink pointing into the concrete default home.
	aliasBase := t.TempDir()
	alias := filepath.Join(aliasBase, "af-home-alias")
	require.NoError(t, os.Symlink(afHome, alias))
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", alias)

	loaded, err := LoadConfigReadOnly()
	require.NoError(t, err, "an alias symlink into the concrete default is repairable; the diagnostic must agree with startup")
	assert.True(t, loaded.EmptyStub)
	assert.NotNil(t, loaded.Config)
	assert.Empty(t, loaded.DirectoryAccessWarning)
	// No-write: the stub and the home's mode are untouched.
	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# placeholder\n", string(got))
	info, err := os.Stat(afHome)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "the diagnostic must not chmod-repair the concrete default it reports on")
}

// TestStartupWouldRepairHome pins the predicate that decides whether the
// directory-access gate stands down. It must return true only for the cases
// secureAFHomeForPath chmod-repairs (concrete default, or an alias into it) that
// the current user owns, and false for everything startup leaves to its raw
// remove — custom homes, default-names-that-themselves-are-symlinks, and
// foreign-owned defaults where the chmod startup attempts would fail.
func TestStartupWouldRepairHome(t *testing.T) {
	t.Run("custom home is not repaired", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		// A custom home never matches the default name, so concreteDefaultAFHome
		// returns "" regardless of ownership or mode.
		assert.False(t, startupWouldRepairHome(home))
	})

	t.Run("concrete default home owned by current user is repaired", func(t *testing.T) {
		afHome := stageDefaultAFHome(t, "", 0o755)
		assert.True(t, startupWouldRepairHome(afHome), "owner-owned concrete default is chmod-repairable by startup")
	})

	t.Run("alias symlink into concrete default is repaired", func(t *testing.T) {
		fastShell(t)
		userHome := t.TempDir()
		afHome := filepath.Join(userHome, ".agent-factory")
		require.NoError(t, os.Mkdir(afHome, 0o755))
		aliasBase := t.TempDir()
		alias := filepath.Join(aliasBase, "af-home-alias")
		require.NoError(t, os.Symlink(afHome, alias))
		t.Setenv("HOME", userHome)
		t.Setenv("AGENT_FACTORY_HOME", alias)
		assert.True(t, startupWouldRepairHome(alias), "an alias into the concrete default is chmod-repairable on its target")
	})

	t.Run("default name itself is a symlink is not repaired", func(t *testing.T) {
		fastShell(t)
		userHome := t.TempDir()
		customDir := t.TempDir()
		// The default name ~/.agent-factory is itself a symlink to a
		// caller-owned directory; its target is caller-owned and not repairable.
		require.NoError(t, os.Symlink(customDir, filepath.Join(userHome, ".agent-factory")))
		t.Setenv("HOME", userHome)
		t.Setenv("AGENT_FACTORY_HOME", "")
		assert.False(t, startupWouldRepairHome(filepath.Join(userHome, ".agent-factory")),
			"a default-name whose target is caller-owned is not chmod-repairable")
	})

	t.Run("foreign-owned default home is repairable by root", func(t *testing.T) {
		if os.Getuid() != 0 {
			t.Skip("staging a foreign-owned directory requires chown, which needs root")
		}
		afHome := stageDefaultAFHome(t, "", 0o755)
		// Re-own the concrete default to another user. Root retains CAP_FOWNER
		// and can chmod any directory regardless of ownership, so startup's
		// secureAFHomeForPath succeeds and startupWouldRepairHome must agree.
		require.NoError(t, os.Chmod(afHome, 0o700)) // restore writability for chown/cleanup
		require.NoError(t, os.Chown(afHome, 65534, 65534))
		t.Cleanup(func() {
			_ = os.Chmod(afHome, 0o755)
			_ = os.Chown(afHome, 0, 0)
		})
		assert.True(t, startupWouldRepairHome(afHome), "root can chmod a foreign-owned default home, so it is repairable")
	})
}
