package config

import (
	"strings"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWarnUnknownTomlKeysSkipsApprovedDottedAliasLeaves is the regression test for
// the false "unknown key ... is ignored" warning warnUnknownTomlKeys' strict-mode
// loop emitted for a canonical grouped alias written in root-dotted form (e.g.
// `network.allowed_hosts = [...]`) rather than as a [section] header.
//
// go-toml's strict probe reports a [section] header as a single-component key
// (["network"]), which the existing len(key)==1 guard already skipped, but a
// root-dotted spelling arrives as two components (["network","allowed_hosts"]),
// which bypassed that guard and warned about a setting that
// applyGroupedConfigAliases actually loads and applies. The fix extends the skip
// to two-component keys that are an alias section + an approved alias leaf.
func TestWarnUnknownTomlKeysSkipsApprovedDottedAliasLeaves(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		assert func(*testing.T, *Config)
	}{
		{
			name: "network.allowed_hosts (the bug report's exact reproduction)",
			body: "schema_version = 1\ndefault_program = 'claude'\nnetwork.allowed_hosts = ['a.b']\n",
			assert: func(t *testing.T, cfg *Config) {
				assert.Equal(t, []string{"a.b"}, cfg.AllowedHosts)
			},
		},
		{
			name:   "ssh.host_key_verification",
			body:   "schema_version = 1\nssh.host_key_verification = 'accept-new'\n",
			assert: func(t *testing.T, cfg *Config) { assert.Equal(t, SSHHostKeyAcceptNew, cfg.SSHHostKeyVerification) },
		},
		{
			name:   "docker.mount_agent_credentials",
			body:   "schema_version = 1\ndocker.mount_agent_credentials = true\n",
			assert: func(t *testing.T, cfg *Config) { assert.True(t, cfg.DockerMountAgentCredentials) },
		},
		{
			name:   "sandbox.ssh",
			body:   "schema_version = 1\nsandbox.ssh = 'ssh new'\n",
			assert: func(t *testing.T, cfg *Config) { assert.Equal(t, "ssh new", cfg.SandboxSSH) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fastShell(t)
			warnings := captureLog(t, &aflog.WarningLog)
			cfg, err := parseConfigTOML([]byte(tc.body), "dotted-alias.toml")
			require.NoError(t, err)
			tc.assert(t, cfg)
			assert.NotContains(t, warnings.String(), "unknown key",
				"a root-dotted canonical alias spelling must not emit an unknown-key warning (it loads and applies; the warning would be false)")
		})
	}
}

// TestWarnUnknownTomlKeysDottedAliasMatchesHeaderFormSilence pins the parity
// guarantee named in the bug report: the dotted spelling and the [section]
// header spelling of the same canonical alias must produce the same (zero)
// warnings. Before the fix the header form was silent and the dotted form
// warned once per leaf.
func TestWarnUnknownTomlKeysDottedAliasMatchesHeaderFormSilence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dotted string
		header string
		assert func(*testing.T, *Config)
	}{
		{
			name:   "network.allowed_hosts",
			dotted: "schema_version = 1\nnetwork.allowed_hosts = ['box.tailnet.ts.net']\n",
			header: "schema_version = 1\n[network]\nallowed_hosts = ['box.tailnet.ts.net']\n",
			assert: func(t *testing.T, cfg *Config) {
				assert.Equal(t, []string{"box.tailnet.ts.net"}, cfg.AllowedHosts)
			},
		},
		{
			name:   "ssh.host_key_verification",
			dotted: "schema_version = 1\nssh.host_key_verification = 'accept-new'\n",
			header: "schema_version = 1\n[ssh]\nhost_key_verification = 'accept-new'\n",
			assert: func(t *testing.T, cfg *Config) { assert.Equal(t, SSHHostKeyAcceptNew, cfg.SSHHostKeyVerification) },
		},
	} {
		t.Run(tc.name+"/dotted", func(t *testing.T) {
			fastShell(t)
			warnings := captureLog(t, &aflog.WarningLog)
			cfg, err := parseConfigTOML([]byte(tc.dotted), "parity.toml")
			require.NoError(t, err)
			tc.assert(t, cfg)
			assert.NotContains(t, warnings.String(), "unknown key",
				"the dotted spelling must not emit an unknown-key warning")
		})
		t.Run(tc.name+"/header", func(t *testing.T) {
			fastShell(t)
			warnings := captureLog(t, &aflog.WarningLog)
			cfg, err := parseConfigTOML([]byte(tc.header), "parity.toml")
			require.NoError(t, err)
			tc.assert(t, cfg)
			assert.NotContains(t, warnings.String(), "unknown key",
				"the header spelling must not emit an unknown-key warning")
		})
	}
}

// TestWarnUnknownTomlKeysStillWarnsUnknownDottedAliasLeaves confirms the fix
// does not open a silent-typo hole: a dotted key under an alias section whose
// leaf is NOT an approved alias still emits the "unknown key ... is ignored"
// warning. (The bug report notes such a typo warns twice — once from the strict
// loop and once from the leaf loop; that pre-existing double emission is a
// separate, optional cleanup and is intentionally preserved here.)
func TestWarnUnknownTomlKeysStillWarnsUnknownDottedAliasLeaves(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "network typo", body: "schema_version = 1\nnetwork.listem_addr = '1.2.3.4:1'\n", want: `unknown key "network.listem_addr"`},
		{name: "docker typo", body: "schema_version = 1\ndocker.image = 'untrusted'\n", want: `unknown key "docker.image"`},
		{name: "ssh typo", body: "schema_version = 1\nssh.host = 'untrusted'\n", want: `unknown key "ssh.host"`},
		{name: "sandbox typo", body: "schema_version = 1\nsandbox.mode = 'strict'\n", want: `unknown key "sandbox.mode"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fastShell(t)
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte(tc.body), "dotted-typo.toml")
			require.NoError(t, err)
			assert.Contains(t, warnings.String(), tc.want,
				"an unknown dotted leaf under an alias section must still warn")
		})
	}
}

// TestWarnUnknownTomlKeysFixIsSurgical ensures the dotted-alias skip does not
// swallow genuine unknown keys at the top level or under a non-alias section. A
// config that pairs a valid dotted alias with an unrelated unknown top-level key
// must warn exactly once, and only about the unrelated key — never about the
// alias.
func TestWarnUnknownTomlKeysFixIsSurgical(t *testing.T) {
	fastShell(t)
	body := "schema_version = 1\nnetwork.require_token = true\nfuture_key = 7\n"
	warnings := captureLog(t, &aflog.WarningLog)
	_, err := parseConfigTOML([]byte(body), "mixed.toml")
	require.NoError(t, err)
	assert.Contains(t, warnings.String(), `unknown key "future_key"`)
	assert.NotContains(t, warnings.String(), `unknown key "network`)
	assert.Equal(t, 1, strings.Count(warnings.String(), "unknown key"),
		"only the genuine unknown top-level key warns; the approved dotted alias stays silent")
}

// TestMigrateThenLoadDottedAliasFileIsSilent is the end-to-end guard for the
// bug's blast radius: af config migrate, when the destination table is already
// opened by a root-dotted key, joins further migrated leaves in dotted form (see
// TestMigrateJoinsATableAlreadyOpenedByADottedKey). Before the fix, re-loading
// that af-authored file emitted a false "unknown key" warning per dotted alias
// even though every value was applied. This runs the real migrate entry point,
// then re-parses its output with the warning log captured.
func TestMigrateThenLoadDottedAliasFileIsSilent(t *testing.T) {
	path := migrateHome(t, "schema_version = 1\nlisten_addr = '0.0.0.0:8443'\nnetwork.require_token = true\n")

	result, err := MigrateGlobalConfig()
	require.NoError(t, err, "a config TOML af accepts must not fail to migrate")
	require.Len(t, result.Migrated, 1)
	assert.Equal(t, "listen_addr", result.Migrated[0].From)

	content := readFile(t, path)
	assert.NotContains(t, content, "[network]", "migrate joins the dotted table rather than re-opening it as a header")
	assert.Contains(t, content, "network.listen_addr = '0.0.0.0:8443'")

	warnings := captureLog(t, &aflog.WarningLog)
	cfg, err := parseConfigTOML([]byte(content), path)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8443", cfg.ListenAddr)
	assert.True(t, cfg.RequireToken, "the unrelated dotted leaf survives and is applied")
	assert.Equal(t, 0, strings.Count(warnings.String(), "unknown key"),
		"an af-generated migrated file must not emit an unknown-key warning for its own dotted alias keys on every load")
}
