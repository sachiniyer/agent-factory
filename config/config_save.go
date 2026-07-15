package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// globalConfigTomlPath returns the absolute path of the canonical global
// config.toml (#1030). It is also the lock target every config.toml writer
// shares, so they serialize against each other.
func globalConfigTomlPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}
	return filepath.Join(configDir, TomlConfigFileName), nil
}

// withGlobalConfigLock runs fn holding the exclusive config.toml file lock —
// the same lock SetGlobalConfigValue (`af config set`) takes — so a whole
// load-modify-persist sequence over the global config is atomic against other
// processes (#1838).
//
// The lock has to span the read as well as the write. AtomicWriteFile alone
// makes each write all-or-nothing, but two racing read-modify-write sequences
// still resolve to "last writer wins": both snapshot the file, both re-marshal
// the entire Config, and the second rename silently drops the first writer's
// changes. Locking only the write serializes the writes without making the
// sequences atomic, so it does not fix that.
//
// LoadConfig runs BEFORE the lock is taken, deliberately: it is the call that
// converts a legacy config.json and materializes first-run defaults, and those
// paths take this very lock. WithFileLock is not reentrant — flock is tied to
// the open file description, so a second acquisition from the same process
// blocks forever instead of recursing — so fn must never call LoadConfig.
// Re-read the config inside fn with loadConfigLocked instead.
func withGlobalConfigLock(fn func() error) error {
	// Force any conversion/materialization to happen outside the lock, and
	// fail early on a config that does not load rather than under the lock.
	if _, err := LoadConfig(); err != nil {
		return err
	}
	tomlPath, err := globalConfigTomlPath()
	if err != nil {
		return err
	}
	return WithFileLock(tomlPath, fn)
}

// loadConfigLocked re-reads config.toml from inside the config file lock
// without taking it, mirroring the raw read SetGlobalConfigValue does under the
// same lock. Callers MUST already hold the lock (withGlobalConfigLock is the
// only intended way in); the re-read is what lets a load-modify-persist
// sequence see the writes that landed before it won the lock.
//
// withGlobalConfigLock's pre-lock LoadConfig has already converted/materialized
// and validated the file, so a config.toml that is missing or contentless here
// means it was removed in the window between the two — the same pathological
// case LoadConfig answers with defaults, answered the same way.
func loadConfigLocked() (*Config, error) {
	tomlPath, err := globalConfigTomlPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("failed to read config file %s: %w", prettyHomePath(tomlPath), err)
	}
	if isEffectivelyEmptyToml(data) {
		return DefaultConfig(), nil
	}
	return parseConfigTOML(data, prettyHomePath(tomlPath))
}

// saveConfigLocked saves the configuration to disk as config.toml WITHOUT
// taking the config file lock. Callers must already hold it — use SaveConfig
// for a standalone write, or withGlobalConfigLock when the write is the tail of
// a read-modify-write sequence. Re-locking from the same process deadlocks
// rather than recursing (see withGlobalConfigLock).
//
// It writes only the TOML file — the canonical global config since #1030:
// writing config.json would resurrect the "both files exist" shadow state that
// conversion exists to retire.
func saveConfigLocked(config *Config) error {
	configDir, err := GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	config.SchemaVersion = GlobalConfigSchemaVersion
	data, err := toml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return AtomicWriteFile(tomlPath, data, 0644)
}

// SaveConfig persists the whole global config under the config file lock, so a
// standalone write serializes against `af config set` and the root-agent
// registration writes instead of racing them (#1838).
//
// This is a blind whole-file write: it clobbers whatever another writer changed
// since the caller's snapshot. Only use it when the config being written is
// authoritative (first-run seeding, tests). To change one part of an existing
// config, do the read and the write inside a single withGlobalConfigLock body
// so no concurrent change is lost — RegisterRootAgent is the model.
//
// Must NOT be called from inside a withGlobalConfigLock body: the lock is not
// reentrant, so the nested acquisition would deadlock. Use saveConfigLocked
// there.
func SaveConfig(config *Config) error {
	tomlPath, err := globalConfigTomlPath()
	if err != nil {
		return err
	}
	return WithFileLock(tomlPath, func() error {
		return saveConfigLocked(config)
	})
}
