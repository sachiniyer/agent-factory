package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/aymanbagabas/go-udiff"
)

// MigrateGlobalConfig rewrites the deprecated spellings in the global config
// to their current ones, in place (#3624). It is the verb every deprecated-key
// warning now names.
//
// The contract, in the order it matters:
//
//   - It changes SPELLING, never meaning. Every migrated value is carried over
//     byte-for-byte where the source wrote it on one line, and the rewritten
//     file is re-parsed and compared against the original config before
//     anything is written: a migration that would change a single effective
//     value is an internal error, not a write.
//   - It is idempotent. A second run finds no deprecated flat spelling and
//     reports that there was nothing to migrate.
//   - It refuses rather than choose. A key written in BOTH spellings with
//     DIFFERENT values has no migration that does not pick a value for the
//     reader, so the whole run refuses, names the key, and writes nothing.
//   - It leaves what it cannot rewrite, and says so. root_agents' successor is
//     a registered project's personal [root_agent], so migrating it would mean
//     inventing project registrations — durable state outside this file. It is
//     reported with the manual step and left exactly where it is, and the keys
//     that CAN move still move: one un-rewritable key must not block the
//     migration that is available.
//   - The readers of the old spellings stay, so an older config keeps loading and
//     the running configuration is untouched. The reverse is narrower and worth
//     stating plainly: the GROUPED spellings have only been read since #3354
//     (2026-08-14), so an af older than that falls back to the built-in default
//     for a migrated key instead of reading it. For every key in the table that
//     default is the conservative one — a loopback listener, strict host-key
//     verification, no credential mount — so a downgrade past that boundary
//     loses the setting in the safe direction, and the .bak restores it.
//
// A legacy config.json is converted to config.toml on the way in (the ordinary
// LoadConfig conversion), which is what makes the flat JSON spellings — the
// permanent, correct spelling in that format — reachable by this at all.
func MigrateGlobalConfig() (*MigrationResult, error) {
	// Precondition, exactly as `af config set` uses it: materialize or convert
	// so config.toml exists, and prove the current file loads, so a later parse
	// failure is unambiguously this migration's fault.
	if _, err := LoadConfig(); err != nil {
		return nil, fmt.Errorf("refusing to migrate: the current config does not load: %w", err)
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)

	var result *MigrationResult
	if err := WithFileLock(tomlPath, func() error {
		var err error
		result, err = migrateConfigFile(tomlPath)
		return err
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// MigrationResult is one `af config migrate` run.
type MigrationResult struct {
	// Path is the file that was examined, whether or not it changed.
	Path string `json:"path"`
	// Backup is the copy of the pre-migration file, empty when nothing was
	// written.
	Backup string `json:"backup,omitempty"`
	// Migrated lists the keys that moved to their current spelling.
	Migrated []MigratedKey `json:"migrated"`
	// Left lists the deprecated keys that have no in-file migration and so
	// stayed exactly where they were, each with the step that ends it.
	Left []UnmigratedKey `json:"left,omitempty"`
	// Diff is the unified diff of the rewrite, empty when nothing changed.
	Diff string `json:"diff,omitempty"`
	// Cautions are consequences of THIS migration the reader has to know about,
	// not general advice — today, the one downgrade that costs a security
	// setting rather than a convenience one.
	Cautions []string `json:"cautions,omitempty"`
}

// Changed reports whether the run rewrote the file.
func (r *MigrationResult) Changed() bool { return r != nil && len(r.Migrated) > 0 }

// MigratedKey is one deprecated key rewritten to its current spelling.
type MigratedKey struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Value is the value as it now reads in the file. It is reported so the
	// caller can show that the migration moved a value rather than changed one.
	Value string `json:"value"`
	// Redundant marks a key that was already written in BOTH spellings with the
	// same value: nothing moved, the stale flat line was simply dropped.
	Redundant bool `json:"redundant,omitempty"`
}

// UnmigratedKey is one deprecated key `af config migrate` deliberately left in
// place, with the manual step that ends its warning.
type UnmigratedKey struct {
	Key  string `json:"key"`
	Step string `json:"step"`
	// Detail names the specific instances the step applies to (the root_agents
	// map's paths), so the reader does not have to open the file to find them.
	Detail []string `json:"detail,omitempty"`
}

// migrateConfigFile is MigrateGlobalConfig's body, minus the lock and the
// load precondition, so tests can drive it against a file directly.
func migrateConfigFile(tomlPath string) (*MigrationResult, error) {
	prettyPath := prettyHomePath(tomlPath)
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	before := string(stripUTF8BOM(raw))
	metadata, err := metadataForSource(raw, tomlPath, FormatTOML)
	if err != nil {
		return nil, tomlParseError("config file "+prettyPath, err)
	}
	beforeCfg, err := parseConfigTOML([]byte(before), prettyPath)
	if err != nil {
		return nil, fmt.Errorf("refusing to migrate: the current config does not load: %w", err)
	}

	result := &MigrationResult{Path: tomlPath, Migrated: []MigratedKey{}}
	content := before
	for _, deprecation := range configDeprecations() {
		if !deprecation.migratable() {
			if left, ok := unmigratedKey(deprecation, metadata.shape); ok {
				result.Left = append(result.Left, left)
			}
			continue
		}
		alias := *deprecation.alias
		flat, present := metadata.shape[alias.legacy]
		if !present {
			continue
		}
		grouped, groupedPresent := aliasGroupedValue(metadata.shape, alias)
		if groupedPresent {
			if !reflect.DeepEqual(flat, grouped) {
				return nil, ambiguousSpellingError(prettyPath, alias, beforeCfg)
			}
			// Same value in both spellings: the flat line carries no information
			// the grouped one does not, so dropping it changes nothing at all.
			updated, removed := deleteTOMLScalar(content, "", alias.legacy)
			if !removed {
				// The decoded shape says the key is there, so a delete that finds
				// no line means the two disagree about the file. Reporting success
				// here would tell the reader the warning is over while the key —
				// and the warning — are still in the file.
				return nil, unremovableKeyError(alias.legacy, prettyPath)
			}
			content = updated
			value, _ := CurrentValue(beforeCfg, alias.canonical)
			result.Migrated = append(result.Migrated, MigratedKey{
				From: alias.legacy, To: alias.canonical, Value: value, Redundant: true,
			})
			continue
		}
		encoded, ok := tomlRootScalarRawValue(content, alias.legacy)
		if !ok {
			// A value spread over several lines (an array) is re-encoded from the
			// decoded config rather than relocated as raw bytes.
			if encoded, err = encodeAliasValue(beforeCfg, alias); err != nil {
				return nil, err
			}
		}
		if tomlRootDottedTable(content, alias.section) {
			// The destination table is already open as a dotted key, and TOML will
			// not let a [header] re-open it. Join it in the same form.
			content = setTOMLScalar(content, "", alias.section+"."+alias.leaf, encoded)
		} else {
			content = setTOMLScalar(content, alias.section, alias.leaf, encoded)
		}
		updated, removed := deleteTOMLScalar(content, "", alias.legacy)
		if !removed {
			return nil, unremovableKeyError(alias.legacy, prettyPath)
		}
		content = updated
		result.Migrated = append(result.Migrated, MigratedKey{From: alias.legacy, To: alias.canonical, Value: encoded})
	}
	if len(result.Migrated) == 0 {
		return result, nil
	}

	// The rewritten file must load, and must load to the SAME configuration.
	// This is the whole safety claim of a spelling migration, so it is asserted
	// against the parsed result rather than argued from the edit.
	afterCfg, err := parseConfigTOML([]byte(content), prettyPath)
	if err != nil {
		return nil, fmt.Errorf("internal error: the migrated config would not load (no changes written): %w", err)
	}
	if diff := configValueDrift(beforeCfg, afterCfg); diff != "" {
		return nil, fmt.Errorf("internal error: migrating %s would change %s (no changes written)", prettyPath, diff)
	}

	backup, err := availableBackupPath(tomlPath + ".bak")
	if err != nil {
		return nil, err
	}
	// Both writes carry the ORIGINAL file's mode rather than a fresh default.
	// An operator who deliberately made config.toml owner-only must not find it
	// world-readable because they took a migration — widening a live file's
	// permissions is not something a spelling rewrite gets to do — and the
	// backup exists to be copied back over it, so a restore must not change
	// them either (#3624 review).
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(tomlPath); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := AtomicWriteFile(backup, raw, mode); err != nil {
		return nil, fmt.Errorf("failed to write the backup %s (no changes written): %w", prettyHomePath(backup), err)
	}
	if err := AtomicWriteFile(tomlPath, []byte(content), mode); err != nil {
		return nil, err
	}
	result.Backup = backup
	// go-udiff is x/tools' diff implementation, already in this module's graph.
	// The reader is being shown a rewrite of their own file, so the rendering
	// should be the ordinary unified diff they already know how to read — and one
	// this repo does not have to own a differ to produce.
	result.Diff = udiff.Unified(prettyPath, prettyPath, before, content)
	result.Cautions = downgradeCautions(result.Migrated, prettyHomePath(backup))
	return result, nil
}

// tokenKeysLostOnDowngrade are the migrated keys whose pre-#3354 fallback is
// UNSAFE rather than conservative. Both default to false, and ListenAddr still
// defaults to a live 127.0.0.1:8443 listener, so losing them does not turn the
// control plane off — it turns its authentication off, for every local user, on
// exactly the shared hosts where someone bothered to set them.
var tokenKeysLostOnDowngrade = map[string]bool{
	"network.require_token":          true,
	"network.require_loopback_token": true,
}

// downgradeCautions reports what this migration costs a reader who later runs an
// af predating the grouped spellings. It fires only when a token requirement was
// actually turned ON and then moved: a migrated `false` is the default anyway, so
// warning about it would be noise that teaches people to skip the notice.
func downgradeCautions(migrated []MigratedKey, backup string) []string {
	var affected []string
	for _, key := range migrated {
		if tokenKeysLostOnDowngrade[key.To] && key.Value == "true" {
			affected = append(affected, key.To)
		}
	}
	if len(affected) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s moved to the grouped spelling, which af only began reading on 2026-08-14 (#3354) · "+
			"an older af reads neither spelling, falls back to false, and keeps serving the control plane on the "+
			"default 127.0.0.1:8443 listener with no token — restore %s before downgrading past that release",
		strings.Join(affected, " and "), backup)}
}

// unmigratedKey builds the report for a deprecation with no in-file migration,
// or reports false when the file does not carry it. The presence test comes off
// the table entry, so it is by construction the same test the warning uses —
// migrate cannot stay silent about a key the loader just warned on.
func unmigratedKey(deprecation configDeprecation, shape map[string]any) (UnmigratedKey, bool) {
	if deprecation.present == nil {
		return UnmigratedKey{}, false
	}
	detail, ok := deprecation.present(shape)
	if !ok {
		return UnmigratedKey{}, false
	}
	return UnmigratedKey{Key: deprecation.key, Step: deprecation.migrationRemedy(), Detail: detail}, true
}

// ambiguousSpellingError refuses a run whose file writes one setting twice with
// two different values. Choosing between them is the reader's call — af has a
// documented winner at LOAD time (the grouped value), but silently making that
// tie-break permanent is exactly the sort of thing a migration must not do to a
// file it also backs up and rewrites.
func ambiguousSpellingError(prettyPath string, alias configKeyAlias, cfg *Config) error {
	effective, _ := CurrentValue(cfg, alias.canonical)
	return fmt.Errorf("refusing to migrate %s: %q and %q are both set, to different values — "+
		"af currently uses the grouped value (%s), and no migration should make that tie-break permanent for you; "+
		"delete whichever line is wrong, then run `af config migrate` again. Nothing was rewritten",
		prettyPath, alias.legacy, alias.canonical, echoMigrationValue(effective))
}

// unremovableKeyError reports a key the decoder found but the surgical edit
// could not locate. Both callers use it: an unusual spelling must never be
// reported as migrated, and must never be silently skipped either.
func unremovableKeyError(key, prettyPath string) error {
	return fmt.Errorf("internal error: %q is present in %s but could not be removed from the root block, "+
		"so nothing was rewritten; move it under its current spelling by hand, or open an issue with the file's shape", key, prettyPath)
}

func echoMigrationValue(v string) string {
	if v == "" {
		return `""`
	}
	return v
}

// encodeAliasValue renders an alias's current value in TOML, for the values
// tomlRootScalarRawValue declines to relocate as raw bytes.
func encodeAliasValue(cfg *Config, alias configKeyAlias) (string, error) {
	spec, ok := settableKeySpecs[alias.canonical]
	if !ok {
		return "", fmt.Errorf("config alias %q has no settable encoding", alias.canonical)
	}
	value, ok := CurrentValue(cfg, alias.canonical)
	if !ok {
		return "", fmt.Errorf("config alias %q has no runtime field", alias.canonical)
	}
	_, encoded, err := canonicalizeScalar(spec.kind, value)
	if err != nil {
		return "", fmt.Errorf("encode config alias %q: %w", alias.canonical, err)
	}
	return encoded, nil
}

// configValueDrift names the first exported Config field whose value differs
// between two loads, or "" when the two configurations are identical. It is the
// migration's proof that it moved a spelling and nothing else.
func configValueDrift(before, after *Config) string {
	if before == nil || after == nil {
		return "the loaded configuration"
	}
	beforeValue := reflect.ValueOf(*snapshotConfig(before))
	afterValue := reflect.ValueOf(*snapshotConfig(after))
	for i := 0; i < beforeValue.NumField(); i++ {
		field := beforeValue.Type().Field(i)
		if field.PkgPath != "" {
			continue
		}
		if reflect.DeepEqual(beforeValue.Field(i).Interface(), afterValue.Field(i).Interface()) {
			continue
		}
		name := field.Name
		if tag, ok := field.Tag.Lookup("toml"); ok {
			name = strings.Split(tag, ",")[0]
		}
		return fmt.Sprintf("%s from %v to %v", name, beforeValue.Field(i).Interface(), afterValue.Field(i).Interface())
	}
	return ""
}
