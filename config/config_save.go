package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// saveConfig saves the configuration to disk as config.toml — the canonical
// global config since #1030. It writes only the TOML file: writing config.json
// would resurrect the "both files exist" shadow state that conversion exists
// to retire.
func saveConfig(config *Config) error {
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

// SaveConfig exports the saveConfig function for use by other packages
func SaveConfig(config *Config) error {
	return saveConfig(config)
}
