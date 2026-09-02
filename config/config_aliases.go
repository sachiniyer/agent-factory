package config

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/pelletier/go-toml/v2"

	"github.com/sachiniyer/agent-factory/log"
)

// configKeyAlias describes one additive TOML spelling for a legacy flat global
// setting. Config deliberately keeps the flat field as its runtime and JSON
// representation: old JSON remains readable forever, and a rolled-back binary
// can still read the old TOML spelling. The canonical dotted name is a TOML-only
// presentation/write surface layered over that same field.
type configKeyAlias struct {
	canonical string
	legacy    string
	section   string
	leaf      string
}

var configKeyAliases = []configKeyAlias{
	{canonical: "ssh.host_key_verification", legacy: "ssh_host_key_verification", section: "ssh", leaf: "host_key_verification"},
	{canonical: "docker.mount_agent_credentials", legacy: "docker_mount_agent_credentials", section: "docker", leaf: "mount_agent_credentials"},
	{canonical: "sandbox.ssh", legacy: "sandbox_ssh", section: "sandbox", leaf: "ssh"},
	{canonical: "network.listen_addr", legacy: "listen_addr", section: "network", leaf: "listen_addr"},
	{canonical: "network.preview_listen_addr", legacy: "preview_listen_addr", section: "network", leaf: "preview_listen_addr"},
	{canonical: "network.require_token", legacy: "require_token", section: "network", leaf: "require_token"},
	{canonical: "network.require_loopback_token", legacy: "require_loopback_token", section: "network", leaf: "require_loopback_token"},
	{canonical: "network.cors_allowed_origins", legacy: "cors_allowed_origins", section: "network", leaf: "cors_allowed_origins"},
}

// globalSettingsTables is decoded in addition to Config on the TOML path. It
// validates only the new grouped leaves while Config continues to own all
// runtime values and the frozen JSON schema.
type globalSettingsTables struct {
	SSH struct {
		HostKeyVerification string `toml:"host_key_verification"`
	} `toml:"ssh"`
	Docker struct {
		MountAgentCredentials bool `toml:"mount_agent_credentials"`
	} `toml:"docker"`
	Sandbox struct {
		SSH string `toml:"ssh"`
	} `toml:"sandbox"`
	Network struct {
		ListenAddr           string   `toml:"listen_addr"`
		PreviewListenAddr    string   `toml:"preview_listen_addr"`
		RequireToken         bool     `toml:"require_token"`
		RequireLoopbackToken bool     `toml:"require_loopback_token"`
		CORSAllowedOrigins   []string `toml:"cors_allowed_origins"`
	} `toml:"network"`
}

func canonicalConfigKey(key string) string {
	for _, alias := range configKeyAliases {
		if key == alias.legacy {
			return alias.canonical
		}
	}
	return key
}

// CanonicalConfigKey maps a permanent flat CLI/storage alias to the dotted
// name shown by the manifest and list surfaces. Unknown keys pass through.
func CanonicalConfigKey(key string) string {
	return canonicalConfigKey(key)
}

// LegacyConfigKey maps either spelling of a migrated setting to the permanent
// flat spelling understood by older daemons and JSON readers. Unknown keys pass
// through unchanged.
func LegacyConfigKey(key string) string {
	canonical := canonicalConfigKey(key)
	if alias, ok := configAliasForCanonical(canonical); ok {
		return alias.legacy
	}
	return key
}

func configAliasForCanonical(key string) (configKeyAlias, bool) {
	for _, alias := range configKeyAliases {
		if key == alias.canonical {
			return alias, true
		}
	}
	return configKeyAlias{}, false
}

func aliasGroupedValue(shape map[string]any, alias configKeyAlias) (any, bool) {
	table, ok := shape[alias.section].(map[string]any)
	if !ok {
		return nil, false
	}
	value, present := table[alias.leaf]
	return value, present
}

// applyGroupedConfigAliases copies explicitly present grouped TOML leaves onto
// the legacy runtime fields. Presence comes from the shapeless decode, never a
// Go zero value, so false, empty strings, and (for later list aliases) empty
// lists remain authoritative.
func applyGroupedConfigAliases(cfg *Config, tables *globalSettingsTables, shape map[string]any) {
	if cfg == nil || tables == nil {
		return
	}
	for _, alias := range configKeyAliases {
		if _, present := aliasGroupedValue(shape, alias); !present {
			continue
		}
		table, ok := taggedFieldByKey(reflect.ValueOf(tables), alias.section)
		if !ok {
			continue
		}
		grouped, ok := taggedFieldByKey(table, alias.leaf)
		if !ok {
			continue
		}
		target, ok := taggedFieldByKey(reflect.ValueOf(cfg), alias.legacy)
		if ok && target.CanSet() && grouped.Type().AssignableTo(target.Type()) {
			target.Set(grouped)
		}
	}
}

func decodeGlobalSettingsTables(data []byte) (*globalSettingsTables, error) {
	var tables globalSettingsTables
	if err := toml.Unmarshal(data, &tables); err != nil {
		return nil, err
	}
	return &tables, nil
}

// marshalGlobalConfigTOML writes canonical grouped leaves while optionally
// retaining only the flat aliases present in legacyShape. Whole-document writes
// are limited to existing authoritative paths (first-run materialization,
// JSON conversion, and SaveConfig); ordinary edits remain surgical.
func marshalGlobalConfigTOML(cfg *Config, legacyShape map[string]any) ([]byte, error) {
	data, err := marshalConfigTOML(cfg)
	if err != nil {
		return nil, err
	}
	content := string(data)
	marshaled, _ := metadataForSource(data, "generated config", FormatTOML)
	for _, alias := range configKeyAliases {
		spec, ok := settableKeySpecs[alias.canonical]
		if !ok {
			return nil, fmt.Errorf("config alias %q has no settable encoding", alias.canonical)
		}
		value, ok := CurrentValue(cfg, alias.canonical)
		if !ok {
			return nil, fmt.Errorf("config alias %q has no runtime field", alias.canonical)
		}
		_, encoded, err := canonicalizeScalar(spec.kind, value)
		if err != nil {
			return nil, fmt.Errorf("encode config alias %q: %w", alias.canonical, err)
		}
		_, marshaledFlat := marshaled.shape[alias.legacy]
		_, sourceFlat := legacyShape[alias.legacy]
		_, sourceGrouped := aliasGroupedValue(legacyShape, alias)
		if marshaledFlat || sourceFlat || sourceGrouped {
			content = setTOMLScalar(content, alias.section, alias.leaf, encoded)
		}
		if _, preserve := legacyShape[alias.legacy]; preserve {
			content = setTOMLScalar(content, "", alias.legacy, encoded)
		} else {
			content, _ = deleteTOMLScalar(content, "", alias.legacy)
		}
	}
	return []byte(content), nil
}

var configAliasWarnings sync.Map

// warnLegacyConfigAliases names the deprecated flat spellings present in shape.
// Every remedy clause comes from the shared deprecation table (deprecations.go),
// so the warning can never point at a command that would not do what it says
// (#3624).
func warnLegacyConfigAliases(shape map[string]any, prettyPath string, format ConfigFormat) {
	for _, deprecation := range configDeprecations() {
		alias := deprecation.alias
		if alias == nil {
			// root_agents has its own warning, with its own presence test (an
			// empty map is not a configured legacy entry). warnLegacyRootAgents
			// reads the same table for its remedy.
			continue
		}
		if _, present := shape[alias.legacy]; !present {
			continue
		}
		memoKey := prettyPath + "\x00" + format.String() + "\x00" + alias.legacy
		if _, seen := configAliasWarnings.LoadOrStore(memoKey, struct{}{}); seen {
			continue
		}
		if format == FormatJSON {
			// The flat key is the PERMANENT JSON spelling, not a deprecated one,
			// so the remedy here is the format conversion rather than a rewrite.
			// `af config migrate` performs that conversion on its way in, which
			// is why it is still the verb to name.
			log.WarningLog.Printf("config %s: JSON config key %q remains supported; %q is TOML-only; retain the flat JSON spelling until the file is converted to %s; run `af config migrate` to convert it now", prettyPath, alias.legacy, alias.canonical, TomlConfigFileName)
			continue
		}
		_, grouped := aliasGroupedValue(shape, *alias)
		if grouped {
			log.WarningLog.Printf("config %s: deprecated config key %q; use %q; both are present, so the grouped value won; %s", prettyPath, alias.legacy, alias.canonical, deprecation.tomlRemedy(true))
			continue
		}
		log.WarningLog.Printf("config %s: deprecated config key %q; use %q; the flat alias remains supported; %s", prettyPath, alias.legacy, alias.canonical, deprecation.tomlRemedy(false))
	}
}

func resetConfigAliasWarnings() {
	configAliasWarnings.Clear()
}

func configAliasSection(section string) bool {
	for _, alias := range configKeyAliases {
		if alias.section == section {
			return true
		}
	}
	return false
}

func configAliasLeaf(section, leaf string) bool {
	for _, alias := range configKeyAliases {
		if alias.section == section && alias.leaf == leaf {
			return true
		}
	}
	return false
}

func globalOnlyGroupedAliasInShape(shape map[string]any) (string, bool) {
	for _, alias := range configKeyAliases {
		if _, present := aliasGroupedValue(shape, alias); present {
			return alias.canonical, true
		}
	}
	return "", false
}
