package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// migrateHome gives the test a hermetic AF home holding the given config.toml
// and returns the file's path.
func migrateHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// LoadConfig probes the user's shell for a claude alias; a plain sh takes
	// the fast `which` path instead of an interactive bash.
	t.Setenv("SHELL", "/bin/sh")
	path := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(body), 0644))
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// everyFlatAliasConfig writes every deprecated flat spelling at once, with a
// distinguishable non-default value for each, so the migration is exercised
// against the whole table rather than the two keys that happened to appear in
// the bug report.
const everyFlatAliasConfig = `# a leading comment
schema_version = 1
default_program = 'codex'
ssh_host_key_verification = 'accept-new'
docker_mount_agent_credentials = true
sandbox_ssh = 'ssh sandbox-host'
listen_addr = '0.0.0.0:8443'
preview_listen_addr = '127.0.0.1:8444'
require_token = true
require_loopback_token = true
cors_allowed_origins = ['https://af.example.com']
`

// TestMigrateRewritesEveryDeprecatedAliasKey is the core of #3624: every
// deprecated spelling this repo warns about has a migration, and running it
// leaves the same configuration behind under the current spelling.
func TestMigrateRewritesEveryDeprecatedAliasKey(t *testing.T) {
	path := migrateHome(t, everyFlatAliasConfig)
	before, err := parseConfigTOML([]byte(everyFlatAliasConfig), path)
	require.NoError(t, err)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)

	moved := map[string]string{}
	for _, migrated := range result.Migrated {
		moved[migrated.From] = migrated.To
		assert.False(t, migrated.Redundant, "%s was not already written in both spellings", migrated.From)
	}
	content := readFile(t, path)
	shape, err := metadataForSource([]byte(content), path, FormatTOML)
	require.NoError(t, err)
	for _, alias := range configKeyAliases {
		assert.Equal(t, alias.canonical, moved[alias.legacy],
			"every deprecated key in the shared table must have been migrated")
		assert.NotContains(t, shape.shape, alias.legacy,
			"the flat spelling must be gone from the root block")
		grouped, present := aliasGroupedValue(shape.shape, alias)
		assert.True(t, present, "%s must now be written as %s", alias.legacy, alias.canonical)
		assert.NotNil(t, grouped)
	}
	for _, section := range []string{"[network]", "[ssh]", "[docker]", "[sandbox]"} {
		assert.Contains(t, content, section)
	}
	// The values must have MOVED, not been re-derived: the same bytes, and an
	// identical effective configuration.
	assert.Contains(t, content, "listen_addr = '0.0.0.0:8443'")
	assert.Contains(t, content, "cors_allowed_origins = ['https://af.example.com']")
	after, err := parseConfigTOML([]byte(content), path)
	require.NoError(t, err)
	assert.Empty(t, configValueDrift(before, after),
		"a spelling migration must not change a single effective value")
	assert.Contains(t, content, "# a leading comment", "unrelated comments survive")
	assert.Contains(t, content, "default_program = 'codex'", "unrelated keys survive")
}

// TestMigratedFileLoadsSilently is the pin the issue asks for: after migrating,
// the load that used to warn on every single config read says nothing at all.
func TestMigratedFileLoadsSilently(t *testing.T) {
	path := migrateHome(t, everyFlatAliasConfig)

	beforeWarnings := captureLog(t, &aflog.WarningLog)
	_, err := parseConfigTOML([]byte(everyFlatAliasConfig), path)
	require.NoError(t, err)
	require.Contains(t, beforeWarnings.String(), "deprecated config key",
		"the fixture must actually be a deprecated config, or the pin proves nothing")

	_, err = MigrateGlobalConfig()
	require.NoError(t, err)

	afterWarnings := captureLog(t, &aflog.WarningLog)
	_, err = parseConfigTOML([]byte(readFile(t, path)), path)
	require.NoError(t, err)
	assert.Empty(t, afterWarnings.String(), "a migrated file must load in silence")
}

// TestMigrateIsIdempotent pins the second run: nothing to migrate, and — just
// as important — nothing written, so re-running cannot churn the file or bury
// the backup that holds the pre-migration original.
func TestMigrateIsIdempotent(t *testing.T) {
	path := migrateHome(t, everyFlatAliasConfig)

	first, err := MigrateGlobalConfig()
	require.NoError(t, err)
	require.True(t, first.Changed())
	migrated := readFile(t, path)

	second, err := MigrateGlobalConfig()
	require.NoError(t, err)
	assert.False(t, second.Changed())
	assert.Empty(t, second.Migrated)
	assert.Empty(t, second.Backup, "an unchanged run must not write a backup")
	assert.Empty(t, second.Diff)
	assert.Equal(t, migrated, readFile(t, path), "the second run must not touch the file")
	entries, err := filepath.Glob(path + ".bak*")
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the second run must not leave a second backup")
}

// TestMigrateBacksUpTheOriginal pins the .bak: it holds the pre-migration
// bytes, and a later migration numbers a new copy rather than overwriting the
// one that already exists.
func TestMigrateBacksUpTheOriginal(t *testing.T) {
	path := migrateHome(t, everyFlatAliasConfig)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)
	require.Equal(t, path+".bak", result.Backup)
	assert.Equal(t, everyFlatAliasConfig, readFile(t, result.Backup),
		"the backup must be the file exactly as it was before the migration")

	// Re-deprecate the file by hand, then migrate again.
	require.NoError(t, os.WriteFile(path, []byte("schema_version = 1\nlisten_addr = '127.0.0.1:9000'\n"), 0644))
	second, err := MigrateGlobalConfig()
	require.NoError(t, err)
	assert.Equal(t, path+".bak.1", second.Backup, "an existing backup is never overwritten")
	assert.Equal(t, everyFlatAliasConfig, readFile(t, path+".bak"),
		"the first backup still holds the original")
}

// TestMigrateRefusesWhenBothSpellingsDisagree is the refuse case. af has a
// documented winner at LOAD time — the grouped value — but a migration that
// also rewrites and backs up the file must not quietly make that tie-break
// permanent, so it stops, names the key, and writes nothing.
func TestMigrateRefusesWhenBothSpellingsDisagree(t *testing.T) {
	const body = `schema_version = 1
listen_addr = '0.0.0.0:8443'
require_token = true

[network]
listen_addr = '127.0.0.1:8443'
`
	path := migrateHome(t, body)

	_, err := MigrateGlobalConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"listen_addr"`, "the refusal names the key")
	assert.Contains(t, err.Error(), `"network.listen_addr"`)
	assert.Contains(t, err.Error(), "127.0.0.1:8443", "and the value af currently uses")
	assert.Contains(t, err.Error(), "Nothing was rewritten")

	assert.Equal(t, body, readFile(t, path), "a refused run leaves the file untouched")
	entries, err := filepath.Glob(path + ".bak*")
	require.NoError(t, err)
	assert.Empty(t, entries, "a refused run writes no backup either")
}

// TestMigrateDropsTheFlatSpellingWhenBothAgree is the other half of the
// both-spellings question. When the two carry the SAME value there is nothing
// to choose between, so the redundant flat line is simply dropped.
func TestMigrateDropsTheFlatSpellingWhenBothAgree(t *testing.T) {
	path := migrateHome(t, `schema_version = 1
listen_addr = '127.0.0.1:8443'

[network]
listen_addr = '127.0.0.1:8443'
`)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)
	require.Len(t, result.Migrated, 1)
	assert.True(t, result.Migrated[0].Redundant)
	assert.Equal(t, "listen_addr", result.Migrated[0].From)

	content := readFile(t, path)
	assert.NotContains(t, content, "\nlisten_addr = '127.0.0.1:8443'\n\n[network]")
	assert.Contains(t, content, "[network]\nlisten_addr = '127.0.0.1:8443'")
	cfg, err := parseConfigTOML([]byte(content), path)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8443", cfg.ListenAddr)
}

// TestMigrateLeavesRootAgentsInPlaceAndNamesTheStep is the reason migrate does
// not refuse wholesale on an un-rewritable key. root_agents' successor is a
// REGISTERED project's personal [root_agent], so migrating it would mean
// inventing project registrations — state outside this file, and visible in the
// project list. It is reported and left alone; the keys that can move still
// move, because one un-rewritable key must not block an available migration.
func TestMigrateLeavesRootAgentsInPlaceAndNamesTheStep(t *testing.T) {
	const body = `schema_version = 1
listen_addr = '0.0.0.0:8443'
require_token = false

[root_agents.'/home/me/two']
[root_agents.'/home/me/one']
program = 'codex'
`
	path := migrateHome(t, body)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)

	require.Len(t, result.Migrated, 2, "the migratable keys still migrate")
	require.Len(t, result.Left, 1)
	assert.Equal(t, LegacyRootAgentsKey, result.Left[0].Key)
	assert.Equal(t, "register <path> as a project, then set [root_agent] there", result.Left[0].Step)
	assert.Equal(t, []string{"/home/me/one", "/home/me/two"}, result.Left[0].Detail,
		"each legacy path is named, sorted, so the reader need not open the file")

	content := readFile(t, path)
	assert.Contains(t, content, "[root_agents.'/home/me/one']", "root_agents is left exactly where it was")
	assert.Contains(t, content, "program = 'codex'")
	assert.Contains(t, content, "[network]")

	// It still loads, and still resolves the legacy entries.
	cfg, err := parseConfigTOML([]byte(content), path)
	require.NoError(t, err)
	assert.Contains(t, cfg.RootAgents, "/home/me/one")
	assert.Equal(t, "0.0.0.0:8443", cfg.ListenAddr)
}

// TestMigrateReportsNoRootAgentsWhenTheMapIsEmpty keeps migrate's presence test
// identical to the warning's: an empty [root_agents] table configures nothing,
// warns about nothing, and so must not be reported as left behind either.
func TestMigrateReportsNoRootAgentsWhenTheMapIsEmpty(t *testing.T) {
	migrateHome(t, "schema_version = 1\nlisten_addr = '0.0.0.0:8443'\n\n[root_agents]\n")

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)
	assert.Empty(t, result.Left)
}

// TestMigrateConvertsLegacyJSONFirst covers the format half of the ask. The
// flat keys are the PERMANENT spelling in JSON, so there is nothing to rewrite
// there; the remedy is the conversion to TOML that af performs anyway, after
// which the ordinary key migration applies.
func TestMigrateConvertsLegacyJSONFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName),
		[]byte(`{"listen_addr":"0.0.0.0:8443","require_token":true}`), 0644))

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, TomlConfigFileName), result.Path)

	content := readFile(t, result.Path)
	cfg, err := parseConfigTOML([]byte(content), result.Path)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8443", cfg.ListenAddr)
	assert.True(t, cfg.RequireToken)

	warnings := captureLog(t, &aflog.WarningLog)
	_, err = parseConfigTOML([]byte(readFile(t, result.Path)), result.Path)
	require.NoError(t, err)
	assert.Empty(t, warnings.String(), "the converted-and-migrated file loads in silence")
}

// TestConfigDeprecationsDeclareExactlyOneRemedy is the invariant that makes a
// silent regression impossible: a deprecation cannot be added to the warning
// side without also declaring what ends it. An entry with neither remedy would
// warn forever with no answer; one with both would let the warning and the
// migration describe different remedies.
func TestConfigDeprecationsDeclareExactlyOneRemedy(t *testing.T) {
	deprecations := configDeprecations()
	require.NotEmpty(t, deprecations)
	seen := map[string]bool{}
	for _, deprecation := range deprecations {
		assert.NotEmpty(t, deprecation.key)
		assert.False(t, seen[deprecation.key], "duplicate deprecation entry for %q", deprecation.key)
		seen[deprecation.key] = true

		hasRewrite := deprecation.alias != nil
		hasManual := deprecation.manual != ""
		assert.NotEqual(t, hasRewrite, hasManual,
			"%q must declare exactly one remedy: a mechanical rewrite or the manual step that replaces it", deprecation.key)
		assert.NotEmpty(t, deprecation.migrationRemedy(), "%q must name its remedy", deprecation.key)
		if hasRewrite {
			assert.Equal(t, deprecation.key, deprecation.alias.legacy)
		}
	}
	// Every flat alias is a deprecation, so a new alias cannot appear without
	// one — the aliases ARE the table's rewrite half.
	for _, alias := range configKeyAliases {
		assert.True(t, seen[alias.legacy], "config alias %q is missing from the deprecation table", alias.legacy)
	}
	assert.True(t, seen[LegacyRootAgentsKey])
}

// TestDeprecatedKeyWarningsNameTheMigrateVerb is the other half of #3624. The
// complaint was not the warning, it was that the warning named no command; every
// deprecated-key warning must now name one.
func TestDeprecatedKeyWarningsNameTheMigrateVerb(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		format     ConfigFormat
	}{
		{name: "flat alias only", body: everyFlatAliasConfig, format: FormatTOML},
		{name: "both spellings", body: "schema_version = 1\nlisten_addr = 'a'\n\n[network]\nlisten_addr = 'b'\n", format: FormatTOML},
		{name: "legacy root agents", body: "schema_version = 1\n\n[root_agents.'/home/me/repo']\n", format: FormatTOML},
		{name: "legacy json", body: `{"listen_addr":"0.0.0.0:8443"}`, format: FormatJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			var err error
			if tc.format == FormatJSON {
				_, err = parseConfig([]byte(tc.body), "config.json")
			} else {
				_, err = parseConfigTOML([]byte(tc.body), "config.toml")
			}
			require.NoError(t, err)
			logged := warnings.String()
			require.NotEmpty(t, logged, "the fixture must warn, or the pin proves nothing")
			for _, line := range strings.Split(strings.TrimRight(logged, "\n"), "\n") {
				assert.Contains(t, line, "af config migrate",
					"every deprecated-key warning must name the verb that ends it")
			}
			assert.NotContains(t, logged, "no file was rewritten",
				"the tail that named an obstruction and no remedy is gone")
		})
	}
}

// TestConfigValueDriftNamesAChangedField keeps the migration's safety net from
// passing vacuously. configValueDrift is the only thing standing between a
// spelling rewrite and a silently changed setting, so it has to actually report
// a difference when there is one.
func TestConfigValueDriftNamesAChangedField(t *testing.T) {
	before, err := parseConfigTOML([]byte("schema_version = 1\nlisten_addr = '127.0.0.1:8443'\n"), "a.toml")
	require.NoError(t, err)
	after, err := parseConfigTOML([]byte("schema_version = 1\nlisten_addr = '0.0.0.0:8443'\n"), "b.toml")
	require.NoError(t, err)

	drift := configValueDrift(before, after)
	require.NotEmpty(t, drift)
	assert.Contains(t, drift, "listen_addr")
	assert.Contains(t, drift, "0.0.0.0:8443")
	assert.Empty(t, configValueDrift(before, before), "a config never drifts from itself")

	// The grouped spelling of the same value is exactly the case the migration
	// relies on being reported as NO drift.
	grouped, err := parseConfigTOML([]byte("schema_version = 1\n\n[network]\nlisten_addr = '127.0.0.1:8443'\n"), "c.toml")
	require.NoError(t, err)
	assert.Empty(t, configValueDrift(before, grouped))
}

// TestMigrateRelocatesAMultilineValue covers the values tomlRootScalarRawValue
// declines to move as raw bytes: they are re-encoded from the decoded config
// instead, which must still produce the same effective list.
func TestMigrateRelocatesAMultilineValue(t *testing.T) {
	const body = `schema_version = 1
cors_allowed_origins = [
  'https://one.example.com',
  'https://two.example.com',
]
`
	path := migrateHome(t, body)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err)
	require.Len(t, result.Migrated, 1)

	cfg, err := parseConfigTOML([]byte(readFile(t, path)), path)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://one.example.com", "https://two.example.com"}, cfg.CORSAllowedOrigins)
}

// TestDeprecatedKeysAreGlobalOnlySoMigrateNeedsNoScope pins why `af config
// migrate` takes no --project or --repo: no other config layer can carry a
// deprecated key. An in-repo or personal-project file naming one is a hard load
// ERROR, not a deprecation warning, so there is no second file to migrate.
//
// If a deprecated key is ever admitted to one of those layers, this fails —
// which is the point. Migrate would then need a scope, and the deprecation
// warning would be naming a verb that could not reach the file it fired on.
func TestDeprecatedKeysAreGlobalOnlySoMigrateNeedsNoScope(t *testing.T) {
	for _, deprecation := range configDeprecations() {
		t.Run(deprecation.key, func(t *testing.T) {
			assert.False(t, isProjectPersonalKey(deprecation.key),
				"a personal project config must not admit a deprecated key")
			for _, allowed := range inRepoAllowedKeys {
				assert.NotEqual(t, deprecation.key, allowed,
					"an in-repo config must not admit a deprecated key")
			}

			// And the loader must SAY so, rather than accepting it quietly:
			// a key that merely warned here would need migrating here too.
			repoRoot := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			body := deprecation.key + " = 'x'\n"
			if deprecation.key == LegacyRootAgentsKey {
				body = "[" + deprecation.key + ".'/x']\n"
			}
			writeInRepoTomlConfig(t, repoRoot, body)
			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err, "an in-repo config carrying a deprecated key must fail to load")
			assert.Contains(t, err.Error(), deprecation.key)
			assert.Contains(t, err.Error(), "global setting")
		})
	}
}

// TestMigrateBackupKeepsTheOriginalMode pins the backup's permissions. The
// .bak exists to be copied back over config.toml, so a restore must not
// silently change the file's mode. The fixture chmods explicitly rather than
// trusting t.TempDir(), whose mode follows the process umask.
func TestMigrateBackupKeepsTheOriginalMode(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0644} {
		t.Run(mode.String(), func(t *testing.T) {
			path := migrateHome(t, everyFlatAliasConfig)
			require.NoError(t, os.Chmod(path, mode))

			result, err := MigrateGlobalConfig()
			require.NoError(t, err)
			require.NotEmpty(t, result.Backup)

			info, err := os.Stat(result.Backup)
			require.NoError(t, err)
			assert.Equal(t, mode, info.Mode().Perm(),
				"the backup must carry the mode the original had")
		})
	}
}
