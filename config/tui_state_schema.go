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
//
// It sets no LinkPolicy, and unlike the other two plans that is not a decision
// left implicit: an empty registry means Migrated is never true, so this plan
// has no write-back for a policy to govern. tui-state.json is migrated in
// memory (config.decodeTUIStateFile) and never goes through
// LoadAndMigrateSchemaFile. Register a migrator here and the field becomes
// live — af-managed state wants SchemaWriteReplaceLink, which is the zero
// value it would already have (#3718).
func NewTUIStateSchemaMigrationPlan(path string, validate SchemaValidator) SchemaMigrationPlan {
	return SchemaMigrationPlan{
		StoreName:      TUIStateFileName,
		Path:           path,
		CurrentVersion: TUIStateSchemaVersion,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      NewSchemaMigrationRegistry(),
		Validate:       validate,
		Perm:           0644,
		// The probe is written against DetectJSONSchemaVersion, which is this
		// plan's detector verbatim.
		ProveCurrentVersion: ProveJSONSchemaVersion,
	}
}
