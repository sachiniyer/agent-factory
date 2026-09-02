package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendSettingsAliasesUseGroupedPresence(t *testing.T) {
	tests := []struct {
		name        string
		legacyKey   string
		canonical   string
		oldOnly     string
		groupOnly   string
		bothEqual   string
		conflict    string
		assertValue func(*testing.T, *Config, string)
		oldValue    string
		groupValue  string
		equalValue  string
		newValue    string
	}{
		{
			name: "ssh host-key verification", legacyKey: "ssh_host_key_verification",
			canonical:   "ssh.host_key_verification",
			oldOnly:     "ssh_host_key_verification = \"insecure\"\n",
			groupOnly:   "[ssh]\nhost_key_verification = \"accept-new\"\n",
			bothEqual:   "ssh_host_key_verification = \"accept-new\"\n[ssh]\nhost_key_verification = \"accept-new\"\n",
			conflict:    "ssh_host_key_verification = \"insecure\"\n[ssh]\nhost_key_verification = \"strict\"\n",
			assertValue: func(t *testing.T, cfg *Config, want string) { assert.Equal(t, want, cfg.SSHHostKeyVerification) },
			oldValue:    "insecure", groupValue: "accept-new", equalValue: "accept-new", newValue: "strict",
		},
		{
			name: "docker credential mount", legacyKey: "docker_mount_agent_credentials",
			canonical: "docker.mount_agent_credentials",
			oldOnly:   "docker_mount_agent_credentials = true\n",
			groupOnly: "[docker]\nmount_agent_credentials = true\n",
			bothEqual: "docker_mount_agent_credentials = true\n[docker]\nmount_agent_credentials = true\n",
			conflict:  "docker_mount_agent_credentials = true\n[docker]\nmount_agent_credentials = false\n",
			assertValue: func(t *testing.T, cfg *Config, want string) {
				assert.Equal(t, want == "true", cfg.DockerMountAgentCredentials)
			},
			oldValue: "true", groupValue: "true", equalValue: "true", newValue: "false",
		},
		{
			name: "sandbox ssh command", legacyKey: "sandbox_ssh",
			canonical:   "sandbox.ssh",
			oldOnly:     "sandbox_ssh = \"ssh old\"\n",
			groupOnly:   "[sandbox]\nssh = \"ssh new\"\n",
			bothEqual:   "sandbox_ssh = \"ssh same\"\n[sandbox]\nssh = \"ssh same\"\n",
			conflict:    "sandbox_ssh = \"ssh old\"\n[sandbox]\nssh = \"\"\n",
			assertValue: func(t *testing.T, cfg *Config, want string) { assert.Equal(t, want, cfg.SandboxSSH) },
			oldValue:    "ssh old", groupValue: "ssh new", equalValue: "ssh same", newValue: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, parseCase := range []struct {
				name        string
				body        string
				want        string
				wantWarning bool
				both        bool
			}{
				{name: "old only", body: tc.oldOnly, want: tc.oldValue, wantWarning: true},
				{name: "grouped only", body: tc.groupOnly, want: tc.groupValue, wantWarning: false},
				{name: "both equal", body: tc.bothEqual, want: tc.equalValue, wantWarning: true, both: true},
				{name: "grouped conflict wins including zero", body: tc.conflict, want: tc.newValue, wantWarning: true, both: true},
			} {
				t.Run(parseCase.name, func(t *testing.T) {
					warnings := captureLog(t, &aflog.WarningLog)
					cfg, err := parseConfigTOML([]byte(parseCase.body), "backend-settings.toml")
					require.NoError(t, err)
					tc.assertValue(t, cfg, parseCase.want)
					if parseCase.wantWarning {
						want := "config backend-settings.toml: deprecated config key \"" + tc.legacyKey + "\"; use \"" + tc.canonical + "\"; "
						if parseCase.both {
							want += "both are present, so the grouped value won; run `af config migrate` to drop the flat spelling once the two agree\n"
						} else {
							want += "the flat alias remains supported; run `af config migrate` to rewrite it in place\n"
						}
						assert.Equal(t, want, warnings.String())
					} else {
						assert.Empty(t, warnings.String())
					}
				})
			}
		})
	}
}

func TestBackendSettingsFlatJSONAliasesRemainSupported(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	cfg, err := parseConfig([]byte(`{
		"ssh_host_key_verification":"accept-new",
		"docker_mount_agent_credentials":true,
		"sandbox_ssh":"ssh sandbox.example"
	}`), "config.json")
	require.NoError(t, err)
	assert.Equal(t, SSHHostKeyAcceptNew, cfg.SSHHostKeyVerification)
	assert.True(t, cfg.DockerMountAgentCredentials)
	assert.Equal(t, "ssh sandbox.example", cfg.SandboxSSH)
	for _, replacement := range []string{
		"ssh.host_key_verification",
		"docker.mount_agent_credentials",
		"sandbox.ssh",
	} {
		assert.Contains(t, warnings.String(), replacement)
	}
	assert.NotContains(t, warnings.String(), "deprecated config key")
	assert.Contains(t, warnings.String(), "TOML-only")
	assert.Contains(t, warnings.String(), "retain the flat JSON spelling")
}

func TestBackendOperatorSettingsAreRejectedInRepo(t *testing.T) {
	tests := []struct {
		name string
		body string
		key  string
	}{
		{name: "ssh", body: "[ssh]\nhost_key_verification = \"insecure\"\n", key: "ssh.host_key_verification"},
		{name: "docker", body: "[docker]\nmount_agent_credentials = true\n", key: "docker.mount_agent_credentials"},
		{name: "sandbox", body: "[sandbox]\nssh = \"ssh attacker\"\n", key: "sandbox.ssh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			repoRoot := t.TempDir()
			dir := filepath.Join(repoRoot, InRepoConfigDirName)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, TomlConfigFileName), []byte(tc.body), 0o644))

			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)
			assert.Contains(t, err.Error(), "global setting")
			assert.Contains(t, err.Error(), filepath.Join(home, TomlConfigFileName))
		})
	}
}

func TestBackendRepoSettingsStayUnknownInGlobalTables(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	cfg, err := parseConfigTOML([]byte(strings.Join([]string{
		"[docker]",
		"image = \"untrusted\"",
		"[ssh]",
		"host = \"untrusted\"",
	}, "\n")), "config.toml")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Contains(t, warnings.String(), `unknown key "docker.image"`)
	assert.Contains(t, warnings.String(), `unknown key "ssh.host"`)
}

func TestBackendAliasSetPreservesLegacyForDowngradeAndUnknownKeys(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		setKey    string
		setValue  string
		canonical string
		legacy    string
		assert    func(*testing.T, *Config)
	}{
		{
			name: "ssh", setKey: "ssh_host_key_verification", setValue: "strict",
			canonical: "host_key_verification = 'strict'", legacy: "ssh_host_key_verification = 'strict'",
			input:  "# keep\nssh_host_key_verification = \"insecure\"\nfuture_key = 7\n[ssh]\nhost_key_verification = \"accept-new\"\n[future]\nvalue = \"keep\"\n",
			assert: func(t *testing.T, cfg *Config) { assert.Equal(t, "strict", cfg.SSHHostKeyVerification) },
		},
		{
			name: "docker", setKey: "docker_mount_agent_credentials", setValue: "false",
			canonical: "mount_agent_credentials = false", legacy: "docker_mount_agent_credentials = false",
			input:  "# keep\ndocker_mount_agent_credentials = true\nfuture_key = 7\n[docker]\nmount_agent_credentials = true\n[future]\nvalue = \"keep\"\n",
			assert: func(t *testing.T, cfg *Config) { assert.False(t, cfg.DockerMountAgentCredentials) },
		},
		{
			name: "sandbox", setKey: "sandbox_ssh", setValue: "",
			canonical: "ssh = ''", legacy: "sandbox_ssh = ''",
			input:  "# keep\nsandbox_ssh = \"ssh old\"\nfuture_key = 7\n[sandbox]\nssh = \"ssh newer\"\n[future]\nvalue = \"keep\"\n",
			assert: func(t *testing.T, cfg *Config) { assert.Empty(t, cfg.SandboxSSH) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.input)
			result, err := SetGlobalConfigValue(tc.setKey, tc.setValue)
			require.NoError(t, err)
			assert.Equal(t, canonicalConfigKey(tc.setKey), result.Key)

			written, err := os.ReadFile(path)
			require.NoError(t, err)
			body := string(written)
			assert.Contains(t, body, "# keep")
			assert.Contains(t, body, "future_key = 7")
			assert.Contains(t, body, "[future]\nvalue = \"keep\"")
			assert.Contains(t, body, tc.canonical)
			assert.Contains(t, body, tc.legacy)

			upgraded, err := parseConfigTOML(written, path)
			require.NoError(t, err)
			tc.assert(t, upgraded)

			// A rolled-back reader knows only the flat tags. Keeping an existing
			// flat alias synchronized prevents a new-version edit from changing
			// meaning merely because the operator rolls back the binary.
			var downgraded Config
			require.NoError(t, toml.Unmarshal(written, &downgraded))
			tc.assert(t, &downgraded)
		})
	}
}

func TestBackendAliasSetOnGroupedOnlyFileDoesNotInventFlatKey(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\n[ssh]\nhost_key_verification = \"strict\"\n")
	result, err := SetGlobalConfigValue("ssh_host_key_verification", "accept-new")
	require.NoError(t, err)
	assert.Equal(t, "ssh.host_key_verification", result.Key)
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(written), "host_key_verification = 'accept-new'")
	assert.NotContains(t, string(written), "ssh_host_key_verification")
}

func TestBackendAliasSetUpdatesQuotedGroupedLeaf(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\n['ssh']\n'host_key_verification' = \"insecure\" # keep\n")
	result, err := SetGlobalConfigValue("ssh.host_key_verification", "strict")
	require.NoError(t, err)
	assert.Equal(t, "ssh.host_key_verification", result.Key)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(written), "'host_key_verification' = 'strict' # keep")
	assert.Equal(t, 1, strings.Count(string(written), "host_key_verification"))
}

func TestBackendAliasSetPreservesInlineTableSiblings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "replace", input: `ssh = { host_key_verification = "insecure", future = "keep" }`, want: `ssh = { host_key_verification = 'strict', future = "keep" }`},
		{name: "insert", input: `ssh = { future = "keep" }`, want: `ssh = { future = "keep", host_key_verification = 'strict' }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, "schema_version = 1\n"+tc.input+"\n")
			_, err := SetGlobalConfigValue("ssh.host_key_verification", "strict")
			require.NoError(t, err)
			written, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(written), tc.want)
			assert.Equal(t, 1, strings.Count(string(written), `future = "keep"`))
		})
	}
}

func TestBackendAliasUnsetRemovesBothSpellingsWithoutResurrection(t *testing.T) {
	path := writeTempConfig(t, "# keep\nssh_host_key_verification = \"insecure\"\nfuture_key = 7\n[ssh]\nhost_key_verification = \"accept-new\"\n")
	result, err := UnsetGlobalConfigValue("ssh_host_key_verification")
	require.NoError(t, err)
	assert.Equal(t, "ssh.host_key_verification", result.Key)
	assert.True(t, result.Removed)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(written)
	assert.NotContains(t, body, "ssh_host_key_verification")
	assert.NotContains(t, body, "host_key_verification =")
	assert.Contains(t, body, "# keep")
	assert.Contains(t, body, "future_key = 7")
	cfg, err := parseConfigTOML(written, path)
	require.NoError(t, err)
	assert.Equal(t, SSHHostKeyStrict, cfg.SSHHostKeyVerification)
}

func TestBackendAliasUnsetRemovesQuotedSpellingsWithoutResurrection(t *testing.T) {
	path := writeTempConfig(t, "# keep\n\"ssh_host_key_verification\" = \"insecure\"\n['ssh']\n'host_key_verification' = \"accept-new\"\n")
	result, err := UnsetGlobalConfigValue("ssh.host_key_verification")
	require.NoError(t, err)
	assert.True(t, result.Removed)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "# keep\nschema_version = 1\n['ssh']\n", string(written))
	cfg, err := parseConfigTOML(written, path)
	require.NoError(t, err)
	assert.Equal(t, SSHHostKeyStrict, cfg.SSHHostKeyVerification)
}

func TestBackendAliasUnsetPreservesInlineTableSiblings(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\nssh = { host_key_verification = \"accept-new\", future = \"keep\" }\n")
	result, err := UnsetGlobalConfigValue("ssh.host_key_verification")
	require.NoError(t, err)
	assert.True(t, result.Removed)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(written), `ssh = { future = "keep" }`)
	assert.NotContains(t, string(written), "host_key_verification")
	cfg, err := parseConfigTOML(written, path)
	require.NoError(t, err)
	assert.Equal(t, SSHHostKeyStrict, cfg.SSHHostKeyVerification)
}

func TestBackendAliasManifestAndEffectsUseCanonicalNames(t *testing.T) {
	wantCanonical := []string{
		"ssh.host_key_verification",
		"docker.mount_agent_credentials",
		"sandbox.ssh",
	}
	legacy := []string{
		"ssh_host_key_verification",
		"docker_mount_agent_credentials",
		"sandbox_ssh",
	}
	manifestKeys := make([]string, 0, len(Manifest()))
	for _, entry := range Manifest() {
		manifestKeys = append(manifestKeys, entry.Key)
	}
	settable := SettableKeys()
	for i, key := range wantCanonical {
		assert.True(t, slices.Contains(manifestKeys, key), "manifest missing %s", key)
		assert.False(t, slices.Contains(manifestKeys, legacy[i]), "legacy alias must not be a second manifest row")
		assert.True(t, slices.Contains(settable, key), "settable list missing %s", key)
		assert.False(t, slices.Contains(settable, legacy[i]), "settable list must advertise canonical names")
		assert.Equal(t, EffectAppliedLive, KeyEffectClass(key))
		assert.Equal(t, EffectAppliedLive, KeyEffectClass(legacy[i]))
	}

	briefing := RenderBriefing(DefaultConfig(), "config.toml")
	for i, key := range wantCanonical {
		assert.Contains(t, briefing, "`"+legacy[i]+"` remains accepted by TOML, JSON, and the CLI")
		assert.Contains(t, briefing, "`"+key+"` wins")
	}
}

func TestBackendAliasDefaultMaterializationUsesGroupedTOMLOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), TomlConfigFileName)
	created, err := writeConfigIfMissing(path, DefaultConfig())
	require.NoError(t, err)
	assert.True(t, created)
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(written)
	assert.Contains(t, body, "[ssh]\n")
	assert.Contains(t, body, "host_key_verification = 'strict'")
	assert.NotContains(t, body, "[docker]\n", "an unset false omitempty field must stay absent")
	assert.NotContains(t, body, "[sandbox]\n", "an unset empty omitempty field must stay absent")
	for _, legacy := range []string{"ssh_host_key_verification", "docker_mount_agent_credentials", "sandbox_ssh"} {
		assert.NotContains(t, body, legacy)
	}

	warnings := captureLog(t, &aflog.WarningLog)
	_, err = parseConfigTOML(written, path)
	require.NoError(t, err)
	assert.NotContains(t, warnings.String(), "deprecated config key")
}

func TestBackendAliasJSONUpgradeKeepsFlatValuesForDowngrade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	legacyJSON := `{
		"schema_version":1,
		"ssh_host_key_verification":"accept-new",
		"docker_mount_agent_credentials":true,
		"sandbox_ssh":"ssh sandbox.example"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(legacyJSON), 0o644))

	upgraded, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "accept-new", upgraded.SSHHostKeyVerification)
	assert.True(t, upgraded.DockerMountAgentCredentials)
	assert.Equal(t, "ssh sandbox.example", upgraded.SandboxSSH)

	written, err := os.ReadFile(filepath.Join(home, TomlConfigFileName))
	require.NoError(t, err)
	body := string(written)
	assert.Contains(t, body, "[ssh]\n")
	assert.Contains(t, body, "[docker]\n")
	assert.Contains(t, body, "[sandbox]\n")
	for _, fragment := range []string{
		"ssh_host_key_verification = 'accept-new'",
		"docker_mount_agent_credentials = true",
		"sandbox_ssh = 'ssh sandbox.example'",
		"host_key_verification = 'accept-new'",
		"mount_agent_credentials = true",
		"ssh = 'ssh sandbox.example'",
	} {
		assert.Contains(t, body, fragment)
	}

	var downgraded Config
	require.NoError(t, toml.Unmarshal(written, &downgraded))
	assert.Equal(t, upgraded.SSHHostKeyVerification, downgraded.SSHHostKeyVerification)
	assert.Equal(t, upgraded.DockerMountAgentCredentials, downgraded.DockerMountAgentCredentials)
	assert.Equal(t, upgraded.SandboxSSH, downgraded.SandboxSSH)
}
