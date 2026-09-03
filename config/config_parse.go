package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
)

// parseConfig validates and unmarshals raw config.json bytes on top of the
// defaults. This is the FROZEN legacy JSON reader (#1030): it stays at the
// schema as of the TOML swap so an arbitrarily old install still converts,
// but every key added after the swap (starting with [keys]) lives only in the
// TOML decoder. Any key this reader does not recognize is warned about rather
// than silently dropped, so "I added the new option to my old config.json"
// fails loud. Reachable from convertJSONToTOML and read-only diagnostics now
// that first-run and lost-race materialization write TOML.
func parseConfig(data []byte, prettyConfigPath string) (*Config, error) {
	return parseConfigJSON(data, prettyConfigPath, true)
}

// parseConfigForConversion is the frozen JSON reader on the one path that is
// about to replace config.json with config.toml. The conversion emits one
// write-aware root_agents notice after its atomic TOML write; suppressing the
// generic read-only notice here avoids two contradictory warnings.
func parseConfigForConversion(data []byte, prettyConfigPath string) (*Config, error) {
	return parseConfigJSON(data, prettyConfigPath, false)
}

func parseConfigJSON(data []byte, prettyConfigPath string, warnRootAgents bool) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config file %s is empty; delete it to regenerate defaults, or add valid JSON", prettyConfigPath)
	}
	decodedData, err := normalizeJSONDurationValues(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", prettyConfigPath, err)
	}
	config := DefaultConfig()
	config.source.builtIn = snapshotConfig(config)
	if err := json.Unmarshal(decodedData, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", prettyConfigPath, err)
	}
	if err := validateLoadedConfigSchemaVersion(config.SchemaVersion, prettyConfigPath); err != nil {
		return nil, err
	}
	if metadata, err := metadataForSource(data, prettyConfigPath, FormatJSON); err == nil {
		warnRemovedAutoYes(metadata.shape, "config file "+prettyConfigPath)
		if warnRootAgents {
			warnLegacyRootAgents(metadata.shape, prettyConfigPath)
		}
		warnLegacyConfigAliases(metadata.shape, prettyConfigPath, FormatJSON)
	}

	// Warn about keys the frozen reader ignores so they are not silently lost
	// on conversion. The [keys] keymap gets a specific message (it is a real,
	// TOML-only setting, #1026); anything else is an unknown/newer key.
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err == nil {
		known := knownJSONConfigKeys()
		for key := range topLevel {
			switch {
			case key == "auto_yes":
				// Compatibility warning emitted above. Do not follow it with the
				// generic unknown-key warning for the same removed setting.
			case key == "keys":
				log.WarningLog.Printf("config %s: \"keys\" is ignored in config.json — the keymap is TOML-only; move it to a [keys] table in %s", prettyConfigPath, TomlConfigFileName)
			case !known[key]:
				log.WarningLog.Printf("config %s: unknown key %q is not recognized by this version of af and will be dropped on conversion to %s", prettyConfigPath, key, TomlConfigFileName)
			}
		}
	}

	return validateConfig(config, prettyConfigPath)
}

// parseConfigTOML validates and unmarshals raw config.toml bytes on top of
// the defaults — the TOML twin of parseConfig, sharing validateConfig so the
// two formats can never drift on semantics (#1030).
//
// A config.toml with no settings — zero bytes, only whitespace, only a BOM, or
// comments only — is technically valid TOML (an empty document), but nothing
// materializes this file, so such a stub is always hand-made —
// and because its mere existence shadows config.json, silently treating it
// as "all defaults" would disguise the shadowing as a settings loss. Per the
// #734/#758 posture it is a loud error instead (the TOML analogue of the
// #864 empty-stub handling; unlike json there is no materializer whose
// partial write we could be cleaning up, so the file is never auto-deleted).
//
// Unknown top-level keys warn rather than error: a config.toml written by a
// newer af must keep loading on an older binary (rollback within the TOML
// era), but a silently ignored key is how typos eat settings, so each one is
// named in the log.
func parseConfigTOML(data []byte, prettyConfigPath string) (*Config, error) {
	data = stripUTF8BOM(data)
	if isEffectivelyEmptyToml(data) {
		return nil, fmt.Errorf("config file %s is empty; add valid TOML, or delete it to fall back to config.json or defaults", prettyConfigPath)
	}
	decodedData, err := normalizeTOMLDurationValues(data)
	if err != nil {
		return nil, tomlParseError("config file "+prettyConfigPath, err)
	}
	config := DefaultConfig()
	config.source.builtIn = snapshotConfig(config)
	// A table is a custom palette, not the named default it overlays. Clear the
	// preset metadata before decoding so a later save preserves the user's table
	// rather than collapsing it to theme = "nord".
	var shape map[string]any
	if err := toml.Unmarshal(decodedData, &shape); err == nil {
		if _, isTable := shape["theme"].(map[string]any); isTable {
			config.Theme.preset = ""
			config.Theme.explicitPreset = false
		}
	}
	if err := toml.Unmarshal(decodedData, config); err != nil {
		return nil, tomlParseError("config file "+prettyConfigPath, err)
	}
	tables, err := decodeGlobalSettingsTables(decodedData)
	if err != nil {
		return nil, tomlParseError("config file "+prettyConfigPath, err)
	}
	if err := validateLoadedConfigSchemaVersion(config.SchemaVersion, prettyConfigPath); err != nil {
		return nil, err
	}
	if metadata, err := metadataForSource(data, prettyConfigPath, FormatTOML); err == nil {
		applyGroupedConfigAliases(config, tables, metadata.shape)
		warnRemovedAutoYes(metadata.shape, "config file "+prettyConfigPath)
		warnLegacyRootAgents(metadata.shape, prettyConfigPath)
		warnLegacyConfigAliases(metadata.shape, prettyConfigPath, FormatTOML)
	}
	warnUnknownTomlKeys(decodedData, prettyConfigPath)

	return validateConfig(config, prettyConfigPath)
}

// stripUTF8BOM removes a leading UTF-8 BOM. TOML 1.0 says a BOM should be
// ignored when present, but go-toml/v2 does not strip it, so every entry
// point that feeds raw file bytes to toml.Unmarshal must call this first.
// bytes.TrimPrefix is a no-op when the prefix is absent, so applying it at
// more than one layer is safe.
func stripUTF8BOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
}

// isEffectivelyEmptyToml reports whether data decodes to an empty TOML
// document. That includes zero bytes, whitespace, a UTF-8 BOM, and comments
// without any keys or tables. Every such file is valid TOML, so without this
// check it would silently become an all-defaults canonical config while
// shadowing a real config.json.
func isEffectivelyEmptyToml(data []byte) bool {
	trimmed := stripUTF8BOM(data)
	if len(bytes.TrimSpace(trimmed)) == 0 {
		return true
	}
	var document map[string]any
	if err := toml.Unmarshal(trimmed, &document); err != nil {
		return false
	}
	return len(document) == 0
}

// tomlParseError renders a TOML decode failure for the file described by
// what (e.g. "config file ~/.agent-factory/config.toml"), surfacing
// pelletier's line/column and caret-annotated source snippet when available —
// the error quality a hand-edited format owes its editors.
func tomlParseError(what string, err error) error {
	var decodeErr *toml.DecodeError
	if errors.As(err, &decodeErr) {
		row, col := decodeErr.Position()
		return fmt.Errorf("failed to parse %s (line %d, column %d):\n%s", what, row, col, decodeErr.String())
	}
	return fmt.Errorf("failed to parse %s: %w", what, err)
}

// warnUnknownTomlKeys logs one warning per key in data that the Config
// schema does not know. Best-effort by design: it re-decodes in strict mode
// purely to harvest the unknown-key list, and any error other than the
// strict-mode report is ignored here because parseConfigTOML has already
// decoded the same bytes successfully.
func warnUnknownTomlKeys(data []byte, prettyConfigPath string) {
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var probe Config
	err := decoder.Decode(&probe)
	var strictErr *toml.StrictMissingError
	if !errors.As(err, &strictErr) {
		return
	}
	for _, keyErr := range strictErr.Errors {
		key := keyErr.Key()
		if removedAutoYesKeyPath(key) {
			// Compatibility warning emitted by parseConfigTOML. Avoid a second,
			// generic warning that loses the migration recipe.
			continue
		}
		if len(key) == 1 && configAliasSection(key[0]) {
			// Config intentionally keeps the grouped tables out of its runtime and
			// JSON shape. Their approved leaves are checked below; do not reduce an
			// unknown leaf to a misleading warning about the whole table.
			continue
		}
		log.WarningLog.Printf("config %s: unknown key %q is ignored by this version of af", prettyConfigPath, strings.Join(key, "."))
	}

	var shape map[string]any
	if err := toml.Unmarshal(data, &shape); err != nil {
		return
	}
	for section, raw := range shape {
		if !configAliasSection(section) {
			continue
		}
		table, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for leaf := range table {
			if configAliasLeaf(section, leaf) {
				continue
			}
			log.WarningLog.Printf("config %s: unknown key %q is ignored by this version of af", prettyConfigPath, section+"."+leaf)
		}
	}
}

// validateConfig applies the format-independent semantic checks shared by
// parseConfig and parseConfigTOML: enum validation hard-errors, range checks
// warn and fall back to defaults.
func validateConfig(config *Config, prettyConfigPath string) (*Config, error) {
	if config.SchemaVersion == LegacySchemaVersion {
		config.SchemaVersion = GlobalConfigSchemaVersion
	}
	if err := ValidateProgramEnum(
		fmt.Sprintf("Config issue in %s: default_program", prettyConfigPath),
		"default_program",
		config.DefaultProgram,
		"",
	); err != nil {
		return nil, err
	}
	for key, value := range config.ProgramOverrides {
		if err := ValidateProgramEnum(
			fmt.Sprintf("Config issue in %s: program_overrides key", prettyConfigPath),
			"program_overrides key",
			key,
			value,
		); err != nil {
			return nil, err
		}
	}
	// Beside the program-override KEY check above: warn about the SHAPE of a
	// value this file hands to /bin/sh -c (#3566). Each of the three config
	// sources warns for the keys it admits — this one is the only source of
	// sandbox.ssh, and the operator's own layer for on_archive_command.
	shellValues := shellValueSet{}
	shellValues.addMap("program_overrides", config.ProgramOverrides)
	shellValues.add("on_archive_command", config.OnArchiveCommand)
	shellValues.add("sandbox.ssh", config.SandboxSSH)
	shellValues.warnExecSeparator(prettyConfigPath)

	normalizedSessionEnv, err := sessionenv.NormalizeExtraNames(config.SessionEnvPassthrough)
	if err != nil {
		return nil, fmt.Errorf("Config issue in %s: session_env_passthrough: %w", prettyConfigPath, err)
	}
	config.SessionEnvPassthrough = normalizedSessionEnv

	sanitizeLimitPatterns(config)
	sanitizeThemeColors(config, prettyConfigPath)
	config.LimitRetryInterval = sanitizeLimitRetryInterval(config.LimitRetryInterval, prettyConfigPath)
	config.WorktreeRoot = normalizeWorktreeRoot(config.WorktreeRoot, prettyConfigPath)
	config.DetachKeys = sanitizeDetachKeys(config.DetachKeys, prettyConfigPath)

	if config.DaemonPollInterval <= 0 {
		log.WarningLog.Printf("daemon_poll_interval=%d is non-positive; using default %dms", config.DaemonPollInterval, defaultDaemonPollInterval)
		config.DaemonPollInterval = defaultDaemonPollInterval
	}

	if config.LogMaxSizeMB <= 0 {
		log.WarningLog.Printf("log_max_size_mb=%d is non-positive; using default %d MB", config.LogMaxSizeMB, log.DefaultMaxSizeMB)
		config.LogMaxSizeMB = log.DefaultMaxSizeMB
	}
	if config.LogMaxBackups < 0 {
		log.WarningLog.Printf("log_max_backups=%d is negative; using default %d", config.LogMaxBackups, log.DefaultMaxBackups)
		config.LogMaxBackups = log.DefaultMaxBackups
	}

	if config.UpdateChannel != UpdateChannelStable && config.UpdateChannel != UpdateChannelPreview {
		log.WarningLog.Printf("update_channel=%q is not one of [%s, %s]; using default %q",
			config.UpdateChannel, UpdateChannelStable, UpdateChannelPreview, UpdateChannelStable)
		config.UpdateChannel = UpdateChannelStable
	}

	// detach_keys-style warn-and-default: an empty value means "unset → default"
	// (no warning); a non-empty invalid value warns and falls back to strict. The
	// fallback (strict) is the safe posture, so a bad hand-edit never silently
	// weakens host-key verification (#2556).
	if config.SSHHostKeyVerification == "" {
		config.SSHHostKeyVerification = SSHHostKeyStrict
	} else if !IsValidSSHHostKeyVerification(config.SSHHostKeyVerification) {
		log.WarningLog.Printf("ssh.host_key_verification=%q is not one of [%s, %s, %s]; using default %q",
			config.SSHHostKeyVerification, SSHHostKeyStrict, SSHHostKeyAcceptNew, SSHHostKeyInsecure, SSHHostKeyStrict)
		config.SSHHostKeyVerification = SSHHostKeyStrict
	}

	// The [keys] keymap hard-errors on any problem (unknown action, bad key
	// string, reserved key, conflict) rather than warn-and-default: a keymap
	// that silently falls back to defaults is indistinguishable from a dead
	// keyboard binding at runtime, which is far harder to debug than a load
	// error naming the file and action (#1026).
	//
	// This runs in every LoadConfig, so the error surfaces on the TUI and on
	// `af keys` (which load the config). CLI subcommands that never load the
	// config — e.g. `af sessions list`, which talks only to the daemon — do
	// not validate the keymap, and that is intentional: the keymap is a
	// TUI-only preference, and blocking an unrelated CLI command because of a
	// keybinding typo would keep a user from running the very commands they'd
	// use to debug it. The binding is validated wherever it is actually
	// consumed.
	overrides, err := normalizeKeyOverrides(config.Keys, prettyConfigPath)
	if err != nil {
		return nil, err
	}
	if err := keys.ValidateOverrides(overrides); err != nil {
		return nil, fmt.Errorf("Config issue in %s: %w", prettyConfigPath, err)
	}
	config.keyOverrides = overrides

	return config, nil
}

// normalizeKeyOverrides converts the shapeless [keys] table into action →
// key-list form: each value must be a single string or an array of strings.
// Returns nil for an absent table so “no rebinds” stays a nil map.
func normalizeKeyOverrides(raw map[string]any, prettyConfigPath string) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	overrides := make(map[string][]string, len(raw))
	for action, value := range raw {
		switch v := value.(type) {
		case string:
			overrides[action] = []string{v}
		case []any:
			list := make([]string, 0, len(v))
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("Config issue in %s: keys.%s: expected a key string or a list of key strings, got a list containing %T", prettyConfigPath, action, item)
				}
				list = append(list, s)
			}
			overrides[action] = list
		default:
			return nil, fmt.Errorf("Config issue in %s: keys.%s: expected a key string or a list of key strings, got %T", prettyConfigPath, action, value)
		}
	}
	return overrides, nil
}
