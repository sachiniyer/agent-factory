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

func TestNetworkSettingsAliasesUseGroupedPresence(t *testing.T) {
	tests := []struct {
		name, legacyKey, canonical                     string
		oldOnly, groupOnly, bothEqual, conflict        string
		defaultValue, oldValue, groupValue, equalValue string
		conflictValue                                  string
		read                                           func(*Config) string
	}{
		{
			name: "listen address", legacyKey: "listen_addr", canonical: "network.listen_addr",
			oldOnly: `listen_addr = "0.0.0.0:8000"`, groupOnly: "[network]\nlisten_addr = \"127.0.0.1:9000\"",
			bothEqual:    "listen_addr = \"127.0.0.1:9000\"\n[network]\nlisten_addr = \"127.0.0.1:9000\"",
			conflict:     "listen_addr = \"0.0.0.0:8000\"\n[network]\nlisten_addr = \"\"",
			defaultValue: defaultListenAddr, oldValue: "0.0.0.0:8000", groupValue: "127.0.0.1:9000", equalValue: "127.0.0.1:9000",
			conflictValue: "",
			read:          func(cfg *Config) string { return cfg.ListenAddr },
		},
		{
			name: "preview listen address", legacyKey: "preview_listen_addr", canonical: "network.preview_listen_addr",
			oldOnly: `preview_listen_addr = "0.0.0.0:8001"`, groupOnly: "[network]\npreview_listen_addr = \"127.0.0.1:9001\"",
			bothEqual: "preview_listen_addr = \"127.0.0.1:9001\"\n[network]\npreview_listen_addr = \"127.0.0.1:9001\"",
			conflict:  "preview_listen_addr = \"0.0.0.0:8001\"\n[network]\npreview_listen_addr = \"\"",
			oldValue:  "0.0.0.0:8001", groupValue: "127.0.0.1:9001", equalValue: "127.0.0.1:9001",
			read: func(cfg *Config) string { return cfg.PreviewListenAddr },
		},
		{
			name: "token requirement", legacyKey: "require_token", canonical: "network.require_token",
			oldOnly: `require_token = true`, groupOnly: "[network]\nrequire_token = true",
			bothEqual: "require_token = true\n[network]\nrequire_token = true",
			conflict:  "require_token = true\n[network]\nrequire_token = false",
			oldValue:  "true", groupValue: "true", equalValue: "true",
			read: func(cfg *Config) string { return boolString(cfg.RequireToken) },
		},
		{
			name: "loopback token requirement", legacyKey: "require_loopback_token", canonical: "network.require_loopback_token",
			oldOnly: `require_loopback_token = true`, groupOnly: "[network]\nrequire_loopback_token = true",
			bothEqual: "require_loopback_token = true\n[network]\nrequire_loopback_token = true",
			conflict:  "require_loopback_token = true\n[network]\nrequire_loopback_token = false",
			oldValue:  "true", groupValue: "true", equalValue: "true",
			read: func(cfg *Config) string { return boolString(cfg.RequireLoopbackToken) },
		},
		{
			name: "CORS origins", legacyKey: "cors_allowed_origins", canonical: "network.cors_allowed_origins",
			oldOnly: `cors_allowed_origins = ["https://old.example.com"]`, groupOnly: "[network]\ncors_allowed_origins = [\"https://new.example.com\"]",
			bothEqual: "cors_allowed_origins = [\"https://same.example.com\"]\n[network]\ncors_allowed_origins = [\"https://same.example.com\"]",
			conflict:  "cors_allowed_origins = [\"https://old.example.com\"]\n[network]\ncors_allowed_origins = []",
			oldValue:  "https://old.example.com", groupValue: "https://new.example.com", equalValue: "https://same.example.com",
			read: func(cfg *Config) string { return strings.Join(cfg.CORSAllowedOrigins, ",") },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, parseCase := range []struct {
				name, body, want string
				legacy, both     bool
			}{
				{name: "neither", body: "schema_version = 1", want: tc.defaultValue},
				{name: "old only", body: tc.oldOnly, want: tc.oldValue, legacy: true},
				{name: "grouped only", body: tc.groupOnly, want: tc.groupValue},
				{name: "both equal", body: tc.bothEqual, want: tc.equalValue, legacy: true, both: true},
				{name: "grouped conflict wins including zero", body: tc.conflict, want: tc.conflictValue, legacy: true, both: true},
			} {
				t.Run(parseCase.name, func(t *testing.T) {
					warnings := captureLog(t, &aflog.WarningLog)
					cfg, err := parseConfigTOML([]byte(parseCase.body+"\n"), "network-settings.toml")
					require.NoError(t, err)
					assert.Equal(t, parseCase.want, tc.read(cfg))
					if !parseCase.legacy {
						assert.Empty(t, warnings.String())
						return
					}
					want := "config network-settings.toml: deprecated config key \"" + tc.legacyKey + "\"; use \"" + tc.canonical + "\"; "
					if parseCase.both {
						want += "both are present, so the grouped value won; run `af config migrate` to drop the flat spelling once the two agree\n"
					} else {
						want += "the flat alias remains supported; run `af config migrate` to rewrite it in place\n"
					}
					assert.Equal(t, want, warnings.String())
				})
			}
		})
	}
}

func TestNetworkSettingsFlatJSONAliasesRemainSupported(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	cfg, err := parseConfig([]byte(`{
		"listen_addr":"0.0.0.0:8443",
		"preview_listen_addr":"127.0.0.1:8444",
		"require_token":true,
		"require_loopback_token":true,
		"cors_allowed_origins":["https://af.example.com"]
	}`), "config.json")
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8443", cfg.ListenAddr)
	assert.Equal(t, "127.0.0.1:8444", cfg.PreviewListenAddr)
	assert.True(t, cfg.RequireToken)
	assert.True(t, cfg.RequireLoopbackToken)
	assert.Equal(t, []string{"https://af.example.com"}, cfg.CORSAllowedOrigins)
	for _, canonical := range []string{
		"network.listen_addr", "network.preview_listen_addr", "network.require_token",
		"network.require_loopback_token", "network.cors_allowed_origins",
	} {
		assert.Contains(t, warnings.String(), canonical)
	}
	assert.NotContains(t, warnings.String(), "deprecated config key")
	assert.Contains(t, warnings.String(), "retain the flat JSON spelling")
}

func TestNetworkAliasManifestAndEffectsUseCanonicalNames(t *testing.T) {
	canonical := []string{
		"network.listen_addr", "network.preview_listen_addr", "network.require_token",
		"network.require_loopback_token", "network.cors_allowed_origins",
	}
	legacy := []string{"listen_addr", "preview_listen_addr", "require_token", "require_loopback_token", "cors_allowed_origins"}
	manifestKeys := make([]string, 0, len(Manifest()))
	for _, entry := range Manifest() {
		manifestKeys = append(manifestKeys, entry.Key)
	}
	for i, key := range canonical {
		assert.True(t, slices.Contains(manifestKeys, key), "manifest missing %s", key)
		assert.False(t, slices.Contains(manifestKeys, legacy[i]), "legacy alias must not be a second row")
		assert.True(t, slices.Contains(SettableKeys(), key), "settable list missing %s", key)
		assert.Equal(t, EffectAppliedLive, KeyEffectClass(key))
		assert.Equal(t, EffectAppliedLive, KeyEffectClass(legacy[i]))
	}

	briefing := RenderBriefing(DefaultConfig(), "config.toml")
	for i, key := range canonical {
		assert.Contains(t, briefing, "`"+legacy[i]+"` remains accepted by TOML, JSON, and the CLI")
		assert.Contains(t, briefing, "`"+key+"` wins")
	}
}

func TestNetworkAliasSetPreservesLegacyForDowngradeAndUnknownKeys(t *testing.T) {
	tests := []struct {
		name, key, value, legacyLine string
		assert                       func(*testing.T, *Config)
	}{
		{name: "listen", key: "listen_addr", value: "127.0.0.1:9000", legacyLine: "listen_addr = '127.0.0.1:9000'", assert: func(t *testing.T, cfg *Config) { assert.Equal(t, "127.0.0.1:9000", cfg.ListenAddr) }},
		{name: "preview", key: "preview_listen_addr", value: "127.0.0.1:9001", legacyLine: "preview_listen_addr = '127.0.0.1:9001'", assert: func(t *testing.T, cfg *Config) { assert.Equal(t, "127.0.0.1:9001", cfg.PreviewListenAddr) }},
		{name: "token", key: "require_token", value: "false", legacyLine: "require_token = false", assert: func(t *testing.T, cfg *Config) { assert.False(t, cfg.RequireToken) }},
		{name: "loopback token", key: "require_loopback_token", value: "false", legacyLine: "require_loopback_token = false", assert: func(t *testing.T, cfg *Config) { assert.False(t, cfg.RequireLoopbackToken) }},
		{name: "CORS", key: "cors_allowed_origins", value: "https://new.example.com", legacyLine: "cors_allowed_origins = ['https://new.example.com']", assert: func(t *testing.T, cfg *Config) {
			assert.Equal(t, []string{"https://new.example.com"}, cfg.CORSAllowedOrigins)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, setKey := range []string{tc.key, canonicalConfigKey(tc.key)} {
				t.Run(setKey, func(t *testing.T) {
					input := "# keep\n" + tc.key + " = " + networkAliasOldEncoded(tc.key) + "\nfuture_key = 7\n[network]\n" + tc.key + " = " + networkAliasOldEncoded(tc.key) + "\nfuture_network_key = \"keep\"\n[future]\nvalue = \"keep\"\n"
					path := writeTempConfig(t, input)
					result, err := SetGlobalConfigValue(setKey, tc.value)
					require.NoError(t, err)
					assert.Equal(t, canonicalConfigKey(tc.key), result.Key)

					written, err := os.ReadFile(path)
					require.NoError(t, err)
					body := string(written)
					assert.Contains(t, body, "# keep")
					assert.Contains(t, body, "future_key = 7")
					assert.Contains(t, body, "future_network_key = \"keep\"")
					assert.Contains(t, body, "[future]\nvalue = \"keep\"")
					assert.Equal(t, 2, strings.Count(body, tc.legacyLine), "flat and grouped values stay synchronized")
					assert.Contains(t, body, "[network]")

					upgraded, err := parseConfigTOML(written, path)
					require.NoError(t, err)
					tc.assert(t, upgraded)
					var downgraded Config
					require.NoError(t, toml.Unmarshal(written, &downgraded))
					tc.assert(t, &downgraded)
				})
			}
		})
	}
}

func networkAliasOldEncoded(key string) string {
	switch key {
	case "listen_addr", "preview_listen_addr":
		return `"0.0.0.0:8000"`
	case "require_token", "require_loopback_token":
		return "true"
	default:
		return `["https://old.example.com"]`
	}
}

func TestNetworkAliasSetOnGroupedOnlyFileDoesNotInventFlatKey(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\n[network]\nlisten_addr = \"127.0.0.1:8443\"\n")
	result, err := SetGlobalConfigValue("listen_addr", "127.0.0.1:9000")
	require.NoError(t, err)
	assert.Equal(t, "network.listen_addr", result.Key)
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(written), "listen_addr = '127.0.0.1:9000'")
	assert.Equal(t, 1, strings.Count(string(written), "listen_addr"))
}

func TestNetworkAliasUnsetRemovesBothSpellingsWithoutResurrection(t *testing.T) {
	tests := []struct {
		legacy, flat, grouped string
		assert                func(*testing.T, *Config)
	}{
		{legacy: "listen_addr", flat: `"0.0.0.0:8000"`, grouped: `""`, assert: func(t *testing.T, cfg *Config) { assert.Equal(t, defaultListenAddr, cfg.ListenAddr) }},
		{legacy: "preview_listen_addr", flat: `"0.0.0.0:8001"`, grouped: `""`, assert: func(t *testing.T, cfg *Config) { assert.Empty(t, cfg.PreviewListenAddr) }},
		{legacy: "require_token", flat: "true", grouped: "false", assert: func(t *testing.T, cfg *Config) { assert.False(t, cfg.RequireToken) }},
		{legacy: "require_loopback_token", flat: "true", grouped: "false", assert: func(t *testing.T, cfg *Config) { assert.False(t, cfg.RequireLoopbackToken) }},
		{legacy: "cors_allowed_origins", flat: `["https://old.example.com"]`, grouped: `[]`, assert: func(t *testing.T, cfg *Config) { assert.Empty(t, cfg.CORSAllowedOrigins) }},
	}
	for _, tc := range tests {
		for _, unsetKey := range []string{tc.legacy, canonicalConfigKey(tc.legacy)} {
			t.Run(unsetKey, func(t *testing.T) {
				path := writeTempConfig(t, "# keep\n"+tc.legacy+" = "+tc.flat+"\nfuture_key = 7\n[network]\n"+tc.legacy+" = "+tc.grouped+"\nfuture_network_key = \"keep\"\n")
				result, err := UnsetGlobalConfigValue(unsetKey)
				require.NoError(t, err)
				assert.Equal(t, canonicalConfigKey(tc.legacy), result.Key)
				assert.True(t, result.Removed)
				written, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.NotContains(t, string(written), tc.legacy)
				assert.Contains(t, string(written), "# keep")
				assert.Contains(t, string(written), "future_key = 7")
				assert.Contains(t, string(written), "future_network_key = \"keep\"")
				cfg, err := parseConfigTOML(written, path)
				require.NoError(t, err)
				tc.assert(t, cfg)
			})
		}
	}
}

func TestNetworkAliasCurrentValueAcceptsBothSpellings(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:9000"
	cfg.PreviewListenAddr = "127.0.0.1:9001"
	cfg.RequireToken = true
	cfg.RequireLoopbackToken = true
	cfg.CORSAllowedOrigins = []string{"https://af.example.com"}
	for _, tc := range []struct{ legacy, canonical, want string }{
		{legacy: "listen_addr", canonical: "network.listen_addr", want: "127.0.0.1:9000"},
		{legacy: "preview_listen_addr", canonical: "network.preview_listen_addr", want: "127.0.0.1:9001"},
		{legacy: "require_token", canonical: "network.require_token", want: "true"},
		{legacy: "require_loopback_token", canonical: "network.require_loopback_token", want: "true"},
		{legacy: "cors_allowed_origins", canonical: "network.cors_allowed_origins", want: "https://af.example.com"},
	} {
		legacy, legacyOK := CurrentValue(cfg, tc.legacy)
		canonical, canonicalOK := CurrentValue(cfg, tc.canonical)
		assert.True(t, legacyOK)
		assert.True(t, canonicalOK)
		assert.Equal(t, tc.want, legacy)
		assert.Equal(t, legacy, canonical)
	}
}

func TestNetworkAliasSetAndUnsetMultilineListPreserveSiblings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   bool
		value string
	}{
		{name: "set", set: true, value: "https://new.example.com"},
		{name: "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, "schema_version = 1\n[network]\ncors_allowed_origins = [ # keep opening\n  # keep standalone\n  \"https://old.example.com\", # keep element\n] # keep closing\nfuture = \"keep\"\n")
			if tc.set {
				_, err := SetGlobalConfigValue("network.cors_allowed_origins", tc.value)
				require.NoError(t, err)
			} else {
				_, err := UnsetGlobalConfigValue("network.cors_allowed_origins")
				require.NoError(t, err)
			}
			written, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(written), `future = "keep"`)
			for _, comment := range []string{"# keep opening", "# keep standalone", "# keep element", "# keep closing"} {
				assert.Contains(t, string(written), comment)
			}
			_, err = parseConfigTOML(written, path)
			require.NoError(t, err)
		})
	}
}

func TestNetworkAliasDefaultMaterializationUsesGroupedTOMLOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), TomlConfigFileName)
	created, err := writeConfigIfMissing(path, DefaultConfig())
	require.NoError(t, err)
	assert.True(t, created)
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(written)
	assert.Contains(t, body, "[network]\n")
	assert.Contains(t, body, "listen_addr = '127.0.0.1:8443'")
	for _, legacy := range []string{"listen_addr =", "preview_listen_addr =", "require_token =", "require_loopback_token =", "cors_allowed_origins ="} {
		beforeNetwork, _, _ := strings.Cut(body, "[network]")
		assert.NotContains(t, beforeNetwork, legacy)
	}
}

func TestNetworkAliasJSONUpgradeKeepsFlatValuesForDowngrade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	legacyJSON := `{
		"schema_version":1,
		"listen_addr":"0.0.0.0:8443",
		"preview_listen_addr":"127.0.0.1:8444",
		"require_token":true,
		"require_loopback_token":true,
		"cors_allowed_origins":["https://af.example.com"]
	}`
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(legacyJSON), 0o644))

	upgraded, err := LoadConfig()
	require.NoError(t, err)
	written, err := os.ReadFile(filepath.Join(home, TomlConfigFileName))
	require.NoError(t, err)
	body := string(written)
	assert.Contains(t, body, "[network]\n")
	for _, fragment := range []string{
		"listen_addr = '0.0.0.0:8443'", "preview_listen_addr = '127.0.0.1:8444'",
		"require_token = true", "require_loopback_token = true",
		"cors_allowed_origins = ['https://af.example.com']",
	} {
		assert.Equal(t, 2, strings.Count(body, fragment), "flat and grouped copies of %q", fragment)
	}

	var downgraded Config
	require.NoError(t, toml.Unmarshal(written, &downgraded))
	assert.Equal(t, upgraded.ListenAddr, downgraded.ListenAddr)
	assert.Equal(t, upgraded.PreviewListenAddr, downgraded.PreviewListenAddr)
	assert.Equal(t, upgraded.RequireToken, downgraded.RequireToken)
	assert.Equal(t, upgraded.RequireLoopbackToken, downgraded.RequireLoopbackToken)
	assert.Equal(t, upgraded.CORSAllowedOrigins, downgraded.CORSAllowedOrigins)
}

func TestNetworkAliasDocumentationUsesCanonicalTableAndNamesAliases(t *testing.T) {
	root := repoRootForDocs(t)
	configuration, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	require.NoError(t, err)
	remote, err := os.ReadFile(filepath.Join(root, "docs", "remote-http-auth.md"))
	require.NoError(t, err)
	web, err := os.ReadFile(filepath.Join(root, "docs", "web.md"))
	require.NoError(t, err)
	networkGuides := string(remote) + string(web)

	assert.Contains(t, string(configuration), "[network]\nlisten_addr =")
	for _, tc := range []struct{ legacy, canonical string }{
		{legacy: "listen_addr", canonical: "network.listen_addr"},
		{legacy: "preview_listen_addr", canonical: "network.preview_listen_addr"},
		{legacy: "require_token", canonical: "network.require_token"},
		{legacy: "require_loopback_token", canonical: "network.require_loopback_token"},
		{legacy: "cors_allowed_origins", canonical: "network.cors_allowed_origins"},
	} {
		assert.Contains(t, string(configuration), "`"+tc.legacy+"`", "permanent alias must be documented")
		assert.Contains(t, string(configuration), "| `"+tc.canonical+"` |", "canonical reference row must be documented")
		assert.Contains(t, networkGuides, "`"+tc.canonical+"`", "network guides must use the canonical key")
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return ""
}
