package config

const (
	// GlobalConfigSchemaVersion is v0 until config.toml gets an additive
	// schema_version field in a later #1046 phase.
	GlobalConfigSchemaVersion = LegacySchemaVersion
	// RepoConfigSchemaVersion covers both in-repo config and the legacy
	// ~/.agent-factory/repos/<id>/config.json fallback.
	RepoConfigSchemaVersion = LegacySchemaVersion
	// StateSchemaVersion is v0 for the current help-screen state.json object.
	StateSchemaVersion = LegacySchemaVersion
	// InstancesSchemaVersion is v0 for the current array-root instances.json.
	InstancesSchemaVersion = LegacySchemaVersion
)
