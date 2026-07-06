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
	// TUIStateSchemaVersion starts at v1 because tui-state.json is greenfield
	// (#1240) and must carry schema_version from its first release. There is no
	// v0/bare-version migration for this store.
	TUIStateSchemaVersion = 1
	// InstancesSchemaVersion is v0 for the current array-root instances.json.
	InstancesSchemaVersion = LegacySchemaVersion
)
