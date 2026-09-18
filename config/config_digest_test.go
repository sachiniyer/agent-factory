package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The digest replaces a post-apply readback, and it rests on two premises that
// were argued from reading the code rather than from running it (#4247):
//
//  1. a save and a later LoadConfig hash the SAME followed symlink target, so a
//     config.toml that is a link into someone's dotfiles still compares (#3688);
//  2. a save always leaves LoadConfig on its canonical path — the single
//     os.ReadFile of config.toml — so the load has a digest to report at all.
//
// A digest comparing different bytes fails silently while looking rigorous,
// which would be strictly worse than the readback it replaces, so both premises
// are pinned here by execution rather than by argument.

// digestTestHome points a fresh AGENT_FACTORY_HOME at a temp dir and returns the
// config dir. A save's own precondition LoadConfig materializes config.toml, so
// nothing needs seeding.
func digestTestHome(t *testing.T) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fastShell(t)
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0755))
	return configDir
}

// Premise 2, the base case: a save writes, a load reads, and the two digests
// agree — which is the entire claim `applied` now rests on.
func TestSaveDigestMatchesTheNextLoad(t *testing.T) {
	digestTestHome(t)

	result, wrote, err := SetGlobalConfigValueWithDigest("default_program", "codex")
	require.NoError(t, err)
	require.Equal(t, "codex", result.Value)
	require.True(t, wrote.Known(), "a committed write must report its bytes")

	cfg, loaded, err := LoadConfigWithDigest()
	require.NoError(t, err)
	require.Equal(t, "codex", cfg.DefaultProgram)
	assert.True(t, loaded.Known(), "a save must leave LoadConfig on its canonical config.toml read")
	assert.True(t, loaded.Matches(wrote), "the load did not hash the bytes the save wrote")
}

// Premise 1 (#3688): with config.toml a symlink into a dotfiles directory, the
// writer writes through the pinned target while LoadConfig reads by path. Both
// must still end up hashing the same bytes, or every save through a linked
// config would report unconfirmed forever.
func TestSaveAndLoadHashTheSameFollowedSymlinkTarget(t *testing.T) {
	configDir := digestTestHome(t)
	dotfiles := t.TempDir()
	linkTarget := filepath.Join(dotfiles, "af-config.toml")
	require.NoError(t, os.WriteFile(linkTarget, []byte("schema_version = 1\ndefault_program = 'claude'\n"), 0644))
	linkPath := filepath.Join(configDir, TomlConfigFileName)
	require.NoError(t, os.RemoveAll(linkPath))
	require.NoError(t, os.Symlink(linkTarget, linkPath))

	_, wrote, err := SetGlobalConfigValueWithDigest("default_program", "codex")
	require.NoError(t, err)

	// The write really did go through the link rather than replacing it.
	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "premise: config.toml is still a symlink after the save")
	through, err := os.ReadFile(linkTarget)
	require.NoError(t, err)
	require.Contains(t, string(through), "codex", "premise: the save landed in the link's target")

	cfg, loaded, err := LoadConfigWithDigest()
	require.NoError(t, err)
	require.Equal(t, "codex", cfg.DefaultProgram)
	assert.True(t, loaded.Matches(wrote), "a linked config.toml must hash identically on both sides")
}

// Premise 2, the case that could have broken it: a shadowing config.json makes
// the load log a warning, and it must still take the canonical branch.
func TestSaveLeavesTheCanonicalPathWithAShadowingJSON(t *testing.T) {
	configDir := digestTestHome(t)

	_, wrote, err := SetGlobalConfigValueWithDigest("default_program", "codex")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(`{"default_program":"claude"}`), 0644))

	_, loaded, err := LoadConfigWithDigest()
	require.NoError(t, err)
	assert.True(t, loaded.Matches(wrote), "a shadowed config.json must not move the load off config.toml")
}

// The mechanism's whole job: a writer landing between the save and the load
// breaks the match. This is the #8 misreport the readback existed to catch, and
// the digest catches it without reading any value back — including from a
// hand-edit, which takes no lock and bumps no counter.
func TestAHandEditBetweenSaveAndLoadBreaksTheDigest(t *testing.T) {
	configDir := digestTestHome(t)
	tomlPath := filepath.Join(configDir, TomlConfigFileName)

	_, wrote, err := SetGlobalConfigValueWithDigest("default_program", "codex")
	require.NoError(t, err)

	edited, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tomlPath, append(edited, []byte("\nbranch_prefix = 'other/'\n")...), 0644))

	_, loaded, err := LoadConfigWithDigest()
	require.NoError(t, err)
	assert.True(t, loaded.Known(), "premise: the edited file still loads canonically")
	assert.False(t, loaded.Matches(wrote), "a file that moved under the save must not confirm it")
}

// An unset reports the bytes it left behind on both of its outcomes: the one
// that removed a line, and the no-op on an already-absent key. The no-op case
// matters most — it is the common `af config unset` — and reporting unknown
// there would make every such command say "unconfirmed".
func TestUnsetDigestMatchesTheNextLoad(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unsetTwice  bool
		wantRemoved bool
	}{
		{name: "removes the key", wantRemoved: true},
		// The second unset finds nothing to remove, so it writes nothing and
		// must report the bytes it READ. This is the common `af config unset`,
		// and reporting unknown here would make it always say "unconfirmed".
		{name: "no-op on an absent key", unsetTwice: true, wantRemoved: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digestTestHome(t)
			_, err := SetGlobalConfigValue("network.require_token", "true")
			require.NoError(t, err)
			if tc.unsetTwice {
				first, err := UnsetGlobalConfigValue("network.require_token")
				require.NoError(t, err)
				require.True(t, first.Removed, "premise: the first unset removed the key")
			}

			result, wrote, err := UnsetGlobalConfigValueWithDigest("network.require_token")
			require.NoError(t, err)
			require.Equal(t, tc.wantRemoved, result.Removed)
			require.True(t, wrote.Known(), "an unset must stand behind the bytes it left on disk")

			_, loaded, err := LoadConfigWithDigest()
			require.NoError(t, err)
			assert.True(t, loaded.Matches(wrote), "the load did not hash the bytes the unset left")
		})
	}
}

// A load that never reached the canonical read has nothing to compare, and must
// say so rather than report a digest of something else. First run is that load:
// it materializes defaults instead of parsing a config.toml a save wrote.
func TestFirstRunLoadReportsNoDigest(t *testing.T) {
	digestTestHome(t)

	cfg, loaded, err := LoadConfigWithDigest()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.False(t, loaded.Known(), "a materializing load parsed no saved config.toml")
}

// Unknown never matches — including against another unknown. "I recorded no
// bytes" is not evidence that two files agree, and a match here would let the
// skewed fallback and a non-canonical load both claim `applied`.
func TestUnknownDigestNeverMatches(t *testing.T) {
	known := digestConfigBytes([]byte("schema_version = 1\n"))
	var unknown ConfigDigest

	assert.False(t, unknown.Known())
	assert.False(t, unknown.Matches(unknown), "unknown must not match unknown")
	assert.False(t, unknown.Matches(known))
	assert.False(t, known.Matches(unknown))
	assert.True(t, known.Matches(digestConfigBytes([]byte("schema_version = 1\n"))))
	assert.False(t, known.Matches(digestConfigBytes([]byte("schema_version = 2\n"))))
}
