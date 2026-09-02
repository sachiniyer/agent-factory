package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// UnsetResult reports a successful config unset. Removed is false when there
// was no override or alias spelling to clear, which is a clean no-op.
type UnsetResult struct {
	Key             string `json:"key"`
	Path            string `json:"path"`
	Removed         bool   `json:"removed"`
	RequiresRestart bool   `json:"requires_restart"`
}

// UnsetProjectConfigValue removes key's personal override for a project so the
// value falls back to the lower layers again (#2216 Phase 5).
func UnsetProjectConfigValue(selector, key string) (*UnsetResult, error) {
	if key == "auto_yes" {
		return nil, RemovedAutoYesError()
	}
	project, err := ResolveProjectSelector(selector)
	if err != nil {
		return nil, err
	}
	section, leaf, spec, err := resolveProjectSettable(key)
	if err != nil {
		return nil, err
	}
	path, err := ProjectConfigTomlPath(project.ID)
	if err != nil {
		return nil, err
	}
	prettyPath := prettyHomePath(path)

	var result *UnsetResult
	writeErr := WithFileLock(path, func() error {
		var err error
		result, err = applyProjectUnset(path, prettyPath, section, leaf, key, spec.structured && section == "")
		return err
	})
	if writeErr != nil {
		return nil, writeErr
	}
	return result, nil
}

// applyProjectUnset removes the target key line from a project's config.toml
// under the caller-held lock. A missing file or absent key is a clean no-op.
func applyProjectUnset(path, prettyPath, section, leaf, key string, structured bool) (*UnsetResult, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &UnsetResult{Key: key, Path: path, Removed: false}, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	current = stripUTF8BOM(current)
	updated := string(current)
	removed := false
	if structured {
		updated, err = removeTOMLTopLevelValue(updated, key)
		if err != nil {
			return nil, fmt.Errorf("failed to remove %s from %s: %w", key, prettyPath, err)
		}
		removed = updated != string(current)
	} else {
		updated, removed = deleteTOMLScalar(updated, section, leaf)
	}
	if !removed {
		return &UnsetResult{Key: key, Path: path, Removed: false}, nil
	}
	if projectConfigHasNoTopLevelKeys(updated) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to remove emptied %s: %w", prettyPath, err)
		}
		return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: true}, nil
	}
	// Read after the no-op returns above, so an absent key on a file this loader
	// would reject stays the clean no-op it has always been.
	before, err := parseProjectConfig(current, path)
	if err != nil {
		return nil, fmt.Errorf("refusing to write: the current personal project config does not load: %w", err)
	}
	resulting, err := parseProjectConfig([]byte(updated), path)
	if err != nil {
		return nil, fmt.Errorf("internal error: edited personal project config would not load (no changes written): %w", err)
	}
	// The delete must have removed the target's line and nothing else. A parse
	// gate cannot tell those apart — a line lifted out of somebody's multiline
	// value leaves valid TOML behind (#3662) — so the values and the presence
	// map are compared instead, and anything else that moved is named.
	if drift := projectRewriteDrift(before, resulting, key); drift != "" {
		return nil, fmt.Errorf("internal error: unsetting %s in %s would change %s (no changes written)", key, prettyPath, drift)
	}
	if err := AtomicWriteFile(path, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: true}, nil
}

func projectConfigHasNoTopLevelKeys(content string) bool {
	if strings.TrimSpace(content) == "" {
		return true
	}
	var shape map[string]any
	if err := toml.Unmarshal([]byte(content), &shape); err != nil {
		return false
	}
	return len(shape) == 0
}

// UnsetGlobalConfigValue removes both storage spellings of one migrated alias
// under the global config lock. Removing only the grouped winner would silently
// resurrect a conflicting flat value, so an alias is one effective setting for
// unset just as it is for get and set.
func UnsetGlobalConfigValue(key string) (*UnsetResult, error) {
	if key == "auto_yes" {
		return nil, RemovedAutoYesError()
	}
	canonicalKey := canonicalConfigKey(key)
	alias, ok := configAliasForCanonical(canonicalKey)
	if !ok {
		return nil, fmt.Errorf("%q is not a globally unsettable migrated config key", key)
	}
	if _, err := LoadConfig(); err != nil {
		return nil, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(configDir, TomlConfigFileName)
	prettyPath := prettyHomePath(path)

	var result *UnsetResult
	writeErr := WithFileLock(path, func() error {
		var err error
		result, err = applyGlobalUnset(path, prettyPath, canonicalKey, alias)
		return err
	})
	if writeErr != nil {
		return nil, writeErr
	}
	return result, nil
}

// applyGlobalUnset removes both storage spellings of one alias from the global
// config.toml under the caller-held lock. It is separate from its caller so the
// value-drift guard below can be driven directly, with a canonicalKey and an
// alias that name different settings — which is exactly the shape of the bug the
// guard exists to catch: an edit that does not do what the command says it does.
func applyGlobalUnset(path, prettyPath, canonicalKey string, alias configKeyAlias) (*UnsetResult, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	current = stripUTF8BOM(current)
	updated, groupedRemoved := deleteTOMLScalar(string(current), alias.section, alias.leaf)
	updated, legacyRemoved := deleteTOMLScalar(updated, "", alias.legacy)
	if !groupedRemoved && !legacyRemoved {
		return &UnsetResult{Key: canonicalKey, Path: path, Removed: false}, nil
	}
	// Read after the no-op return above, for the same reason as its personal
	// twin: nothing is being written, so nothing needs vouching for.
	before, err := parseConfigTOML(current, prettyPath)
	if err != nil {
		return nil, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	updated = setTOMLScalar(updated, "", SchemaVersionField, strconv.Itoa(GlobalConfigSchemaVersion))
	resulting, err := parseConfigTOML([]byte(updated), prettyPath)
	if err != nil {
		return nil, fmt.Errorf("internal error: edited config would not load (no changes written): %w", err)
	}
	// Removing a line that only LOOKED like the key leaves valid TOML behind
	// (#3662), so the parse above cannot vouch for the delete. This can: the
	// unset key and the machine-managed schema marker are the only values the
	// rewrite may move.
	if drift := configRewriteDrift(before, resulting, canonicalKey, SchemaVersionField); drift != "" {
		return nil, fmt.Errorf("internal error: unsetting %s in %s would change %s (no changes written)", canonicalKey, prettyPath, drift)
	}
	if err := AtomicWriteFile(path, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return &UnsetResult{Key: canonicalKey, Path: path, Removed: true, RequiresRestart: true}, nil
}
