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
		return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: ProjectConfigRequiresRestart(key)}, nil
	}
	if _, err := parseProjectConfig([]byte(updated), path); err != nil {
		return nil, fmt.Errorf("internal error: edited personal project config would not load (no changes written): %w", err)
	}
	if err := AtomicWriteFile(path, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: ProjectConfigRequiresRestart(key)}, nil
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
		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", prettyPath, err)
		}
		updated, groupedRemoved := deleteTOMLScalar(string(current), alias.section, alias.leaf)
		updated, legacyRemoved := deleteTOMLScalar(updated, "", alias.legacy)
		if !groupedRemoved && !legacyRemoved {
			result = &UnsetResult{Key: canonicalKey, Path: path, Removed: false}
			return nil
		}
		updated = setTOMLScalar(updated, "", SchemaVersionField, strconv.Itoa(GlobalConfigSchemaVersion))
		if _, err := parseConfigTOML([]byte(updated), prettyPath); err != nil {
			return fmt.Errorf("internal error: edited config would not load (no changes written): %w", err)
		}
		if err := AtomicWriteFile(path, []byte(updated), 0o644); err != nil {
			return err
		}
		result = &UnsetResult{Key: canonicalKey, Path: path, Removed: true, RequiresRestart: true}
		return nil
	})
	if writeErr != nil {
		return nil, writeErr
	}
	return result, nil
}
