package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// migrateHome seeds a hermetic AF home with the given config.toml and returns
// its path.
func migrateHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// LoadConfig probes the user's shell for a claude alias; a plain sh takes
	// the fast `which` path instead of an interactive bash.
	t.Setenv("SHELL", "/bin/sh")
	leaveAmbientRepo(t)
	path := filepath.Join(home, config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(body), 0644))
	return path
}

func runConfigMigrate(t *testing.T) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := configMigrateCmd.RunE(cmd, nil)
	return out.String(), err
}

// bothDeprecationsConfig carries a migratable key AND the one key that has no
// in-file migration — the exact shape of the config in the bug report.
const bothDeprecationsConfig = `schema_version = 1
listen_addr = '0.0.0.0:8443'
require_token = false

[root_agents.'/home/me/two']
[root_agents.'/home/me/one']
`

// TestConfigMigrateReportsMigratedAndLeftInPlace pins the report for the config
// #3624 was filed about: one un-rewritable key does NOT block the migration
// that is available. The aliases move, root_agents is reported with the manual
// step, and the command succeeds — a refusal here would leave the reader with
// nothing.
func TestConfigMigrateReportsMigratedAndLeftInPlace(t *testing.T) {
	path := migrateHome(t, bothDeprecationsConfig)

	out, err := runConfigMigrate(t)
	require.NoError(t, err, "an un-migratable key must not fail the run")

	assert.Contains(t, out, "migrated 2 keys in "+path)
	assert.Contains(t, out, "backup "+path+".bak")
	assert.Contains(t, out, "  listen_addr → network.listen_addr")
	assert.Contains(t, out, "  require_token → network.require_token")
	assert.Contains(t, out, "--- "+path)
	assert.Contains(t, out, "-listen_addr = '0.0.0.0:8443'")
	assert.Contains(t, out, "+[network]")
	assert.Contains(t, out, "the effective configuration is unchanged")
	assert.Contains(t, out, path+".bak holds the original",
		"the recovery hint must name the backup this run wrote, which availableBackupPath numbers when one already exists")
	assert.Less(t, strings.Index(out, "the effective configuration is unchanged"), strings.Index(out, "--- "+path),
		"the summary must frame the diff, not follow it — a moved key renders as a removed and an added line, "+
			"so a reader who meets the raw diff first is alarmed before they are told what it means")
	assert.Contains(t, out,
		"left in place — root_agents has no in-file migration · register the path as a project, set enabled = true plus the optional program in its personal [root_agent], then remove its root_agents entry")
	assert.Contains(t, out, "  /home/me/one")
	assert.Contains(t, out, "  /home/me/two")

	// And the file itself: aliases moved, root_agents untouched.
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "[network]")
	assert.Contains(t, string(content), "[root_agents.'/home/me/one']")
}

// TestConfigMigrateSecondRunIsIdempotentAndStillNamesRootAgents pins the
// re-run: it says there was nothing to migrate, and still ends by naming the
// one deprecation that remains and what to do about it.
func TestConfigMigrateSecondRunIsIdempotentAndStillNamesRootAgents(t *testing.T) {
	path := migrateHome(t, bothDeprecationsConfig)

	_, err := runConfigMigrate(t)
	require.NoError(t, err)
	migrated, err := os.ReadFile(path)
	require.NoError(t, err)

	out, err := runConfigMigrate(t)
	require.NoError(t, err)
	assert.Contains(t, out, "nothing to migrate in "+path)
	assert.NotContains(t, out, "migrated ")
	assert.NotContains(t, out, "backup ")
	assert.Contains(t, out, "left in place — root_agents has no in-file migration · register the path as a project, set enabled = true plus the optional program in its personal [root_agent], then remove its root_agents entry")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(migrated), string(after), "a second run must not touch the file")
}

// TestConfigMigrateOnACleanConfigSaysNothingToMigrate is the quiet case: a
// config with no deprecated key at all gets one line and no ceremony.
func TestConfigMigrateOnACleanConfigSaysNothingToMigrate(t *testing.T) {
	path := migrateHome(t, "schema_version = 1\n\n[network]\nlisten_addr = '127.0.0.1:8443'\n")

	out, err := runConfigMigrate(t)
	require.NoError(t, err)
	assert.Equal(t, "nothing to migrate in "+path+"\n", out)
}

// TestConfigMigrateRefusesAmbiguousSpellings pins the refuse case at the CLI:
// a non-zero exit, the key named, and a file left exactly as it was.
func TestConfigMigrateRefusesAmbiguousSpellings(t *testing.T) {
	const body = `schema_version = 1
listen_addr = '0.0.0.0:8443'

[network]
listen_addr = '127.0.0.1:8443'
`
	path := migrateHome(t, body)

	out, err := runConfigMigrate(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"listen_addr"`)
	assert.Contains(t, err.Error(), "Nothing was rewritten")
	assert.Empty(t, out, "a refusal is the error, not a report")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, body, string(content))
}

// TestConfigMigrateJSONEnvelope keeps the group's --json contract: the result
// on stdout in the shared {data,error} envelope.
func TestConfigMigrateJSONEnvelope(t *testing.T) {
	migrateHome(t, bothDeprecationsConfig)
	prev := configJSONFlag
	configJSONFlag = true
	t.Cleanup(func() { configJSONFlag = prev })

	out, err := runConfigMigrate(t)
	require.NoError(t, err)

	var envelope struct {
		Data  config.MigrationResult `json:"data"`
		Error string                 `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &envelope), "output was %q", out)
	assert.Empty(t, envelope.Error)
	assert.Len(t, envelope.Data.Migrated, 2)
	require.Len(t, envelope.Data.Left, 1)
	assert.Equal(t, "root_agents", envelope.Data.Left[0].Key)
	assert.NotEmpty(t, envelope.Data.Backup)
}

// TestConfigMigrateHelpNamesItsTwoRefusals keeps `af config migrate --help`
// honest about the two things it will not do, so the behavior is discoverable
// before the reader hits it.
func TestConfigMigrateHelpNamesItsTwoRefusals(t *testing.T) {
	help := configMigrateCmd.Long
	assert.Contains(t, help, "BOTH spellings")
	assert.Contains(t, help, "root_agents")
	assert.Contains(t, help, "config.toml.bak")
	assert.True(t, strings.Contains(help, "idempotent") || strings.Contains(help, "twice is safe"),
		"the help must say a second run is safe")
}

// TestConfigMigrateHintNamesTheNumberedBackup pins the #3624-review P2: when
// config.toml.bak already exists the copy is numbered, and a hardcoded
// "config.toml.bak" in the recovery hint would point a reader recovering from a
// bad migration at an OLDER file than the one this run just saved.
func TestConfigMigrateHintNamesTheNumberedBackup(t *testing.T) {
	path := migrateHome(t, bothDeprecationsConfig)
	require.NoError(t, os.WriteFile(path+".bak", []byte("an older backup\n"), 0644))

	out, err := runConfigMigrate(t)
	require.NoError(t, err)
	assert.Contains(t, out, path+".bak.1 holds the original")
	assert.NotContains(t, out, path+".bak holds the original")

	preserved, err := os.ReadFile(path + ".bak")
	require.NoError(t, err)
	assert.Equal(t, "an older backup\n", string(preserved), "the existing backup is never overwritten")
}

// TestConfigMigrateRendersTheDowngradeCaution pins that the security caution
// reaches the terminal, not just the JSON envelope.
func TestConfigMigrateRendersTheDowngradeCaution(t *testing.T) {
	migrateHome(t, "schema_version = 1\nrequire_token = true\nrequire_loopback_token = true\n")

	out, err := runConfigMigrate(t)
	require.NoError(t, err)
	assert.Contains(t, out, "caution — ")
	assert.Contains(t, out, "network.require_loopback_token")
	assert.Contains(t, out, "no token")
}
