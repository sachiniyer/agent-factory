package config

import (
	"fmt"
	"path/filepath"
)

const TUIStateFileName = "tui-state.json"

// TUIStatePath returns the global, TUI-owned view-state file path. The TUI
// state is client-side convenience state, separate from daemon-owned
// instances.json.
func TUIStatePath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}
	return filepath.Join(configDir, TUIStateFileName), nil
}

// NewTUIStateSchemaMigrationPlan returns the schema framework plan that #1240's
// greenfield tui-state.json loader should use. The empty migrator registry is
// deliberate: missing schema_version is legacy v0, but this file has no legacy
// on-disk shape, so bare {"version": 1} documents are rejected instead of
// silently accepted.
func NewTUIStateSchemaMigrationPlan(path string, validate SchemaValidator) SchemaMigrationPlan {
	return SchemaMigrationPlan{
		StoreName:      TUIStateFileName,
		Path:           path,
		CurrentVersion: TUIStateSchemaVersion,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      NewSchemaMigrationRegistry(),
		Validate:       validate,
		Perm:           0644,
	}
}
