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
// empty — the fingerprint the bug report names. Each must recover at startup
// (in-memory defaults, file untouched — #4483) and report EmptyStub (not an
// error) from the read-only diagnostics.
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
// SAME verdict. Startup recovers in memory (defaults, file untouched — #4483:
// a load can never tell a crashed write from one still in flight, so it must
// not remove or rewrite the stub); the no-write diagnostic returns
// EmptyStub=true with a nil error. Before the fix the diagnostic raised a hard
// "config is empty" error, so `af doctor`/`af config validate` rejected a
// state af boots on.
func TestLoadConfigReadOnly_EmptyStubAgreesWithStartup(t *testing.T) {
	for name, content := range effectivelyEmptyTomlInputs() {
		t.Run(name, func(t *testing.T) {
			fastShell(t)

			// Read-only on a fresh home: the diagnostic verdict.
			homeRO := seedHome(t, content)
			tomlPath := filepath.Join(homeRO, TomlConfigFileName)
			loaded, roErr := LoadConfigReadOnly()
			require.NoError(t, roErr, "read-only diagnostic must not fail a state startup recovers from")
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

			// Startup on a separate identical home: the recovery verdict —
			// defaults in memory, the file left exactly as found (#4483).
			homeLC := seedHome(t, content)
			cfg, lcErr := LoadConfig()
			require.NoError(t, lcErr, "startup must recover past the empty stub")
			require.NotNil(t, cfg, "startup must return a real (default) config")
			lcAfter, err := os.ReadFile(filepath.Join(homeLC, TomlConfigFileName))
			require.NoError(t, err)
			assert.Equal(t, content, string(lcAfter), "startup must not remove or rewrite the stub (#4483)")
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

// TestLoadConfigReadOnly_EmptyStubUnremovableDirIsStillEmptyStub pins the
// #4483 consequence the old access gate could not express: startup never
// removes the stub, so directory writability is irrelevant to the verdict. A
// readable stub in a non-writable home is still EmptyStub — af runs on
// in-memory defaults and leaves the file untouched either way.
func TestLoadConfigReadOnly_EmptyStubUnremovableDirIsStillEmptyStub(t *testing.T) {
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

	// Make the home directory read+execute only: a removal would fail here,
	// which is exactly why the load must not attempt one (#4483).
	require.NoError(t, os.Chmod(home, 0o500))
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	loaded, roErr := LoadConfigReadOnly()
	require.NoError(t, roErr, "an empty stub in a non-writable home is still EmptyStub — nothing is removed")
	assert.True(t, loaded.EmptyStub, "EmptyStub no longer depends on directory writability")
	assert.NotNil(t, loaded.Config, "EmptyStub carries DefaultConfig()")
	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# placeholder\n", string(got), "the stub must be left intact")
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

// TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnly keeps the no-write
// contract on the arrangement that used to need the repair-predicate: a
// contentless config.toml in an owner-owned default ~/.agent-factory tightened
// to a write-less mode (0500). Since #4483 nothing is removed or written for a
// stub, so the mode is simply irrelevant — EmptyStub, no error, and the
// diagnostic itself must not chmod-probe the home it reports on.
func TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnly(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0500 home cannot be staged as non-writable")
	}
	afHome := stageDefaultAFHome(t, "# placeholder; fill in later\n", 0o500)
	tomlPath := filepath.Join(afHome, TomlConfigFileName)

	loaded, err := LoadConfigReadOnly()
	require.NoError(t, err, "an empty stub is EmptyStub regardless of home writability (#4483)")
	assert.True(t, loaded.EmptyStub, "an effectively-empty stub must surface as EmptyStub")
	assert.False(t, loaded.Missing)
	assert.NotNil(t, loaded.Config, "EmptyStub carries DefaultConfig() so downstream diagnostics can evaluate the next-start posture")
	assert.Equal(t, tomlPath, loaded.Path)

	// No-write contract: the stub is intact, the home stays read-only, nothing
	// was created — and no chmod probe ran (the read-only path is side-effect
	// free now that it does not predict a removal).
	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# placeholder; fill in later\n", string(got), "LoadConfigReadOnly must not remove or rewrite the stub")
	info, err := os.Stat(afHome)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "the diagnostic must not chmod the home it reports on")
	entries, err := os.ReadDir(afHome)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the diagnostic must not materialize any file beside the stub")
}

// TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyAgreesWithStartup pins the
// full guarantee: the read-only diagnostic and startup must agree on identical
// INPUT states. On a default home @0500 the diagnostic returns EmptyStub=true
// (no error) AND startup recovers (no error) — the same verdict. Startup's
// secureAFHomeForPath still chmod-repairs the HOME to 0700 (that hardening is
// unrelated to the stub), but the stub file itself is left untouched (#4483).
func TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyAgreesWithStartup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0500 home cannot be staged as non-writable")
	}
	for name, content := range effectivelyEmptyTomlInputs() {
		t.Run(name, func(t *testing.T) {
			// Read-only on a fresh default home @0500: the diagnostic verdict.
			afHomeRO := stageDefaultAFHome(t, content, 0o500)
			loaded, roErr := LoadConfigReadOnly()
			require.NoError(t, roErr, "read-only diagnostic must not fail a state startup recovers from")
			assert.True(t, loaded.EmptyStub)
			got, err := os.ReadFile(filepath.Join(afHomeRO, TomlConfigFileName))
			require.NoError(t, err)
			assert.Equal(t, content, string(got), "LoadConfigReadOnly must not mutate the stub")

			// Startup on a separate identical default home @0500: the
			// recovery verdict. secureAFHomeForPath chmod-repairs the home to
			// 0700 (unrelated AF-home hardening), then the stub is left
			// exactly as found — defaults come back in memory only.
			afHomeLC := stageDefaultAFHome(t, content, 0o500)
			cfg, lcErr := LoadConfig()
			require.NoError(t, lcErr, "startup must recover past the empty stub")
			require.NotNil(t, cfg)
			info, err := os.Stat(afHomeLC)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "startup still chmod-repairs the default home itself")
			lcAfter, err := os.ReadFile(filepath.Join(afHomeLC, TomlConfigFileName))
			require.NoError(t, err)
			assert.Equal(t, content, string(lcAfter), "startup must not remove or rewrite the stub (#4483)")
		})
	}
}

// TestLoadConfigReadOnly_EmptyStubDefaultHomeReadOnlyAliasSymlink covers the
// alias-symlink arrangement: an AGENT_FACTORY_HOME that is a symlink whose
// target IS the concrete default home. The verdict is the same — EmptyStub,
// and neither the stub nor the home's mode is touched.
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
	require.NoError(t, err, "an alias symlink into the concrete default still reports EmptyStub")
	assert.True(t, loaded.EmptyStub)
	assert.NotNil(t, loaded.Config)
	// No-write: the stub and the home's mode are untouched.
	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# placeholder\n", string(got))
	info, err := os.Stat(afHome)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "the diagnostic must not chmod the concrete default it reports on")
}
