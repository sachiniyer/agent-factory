package config

const (
	// GlobalConfigSchemaVersion adds schema_version to config.toml
	// additively. Existing config files without the field still load as legacy.
	GlobalConfigSchemaVersion = 1
	// RepoConfigSchemaVersion covers both in-repo config and the legacy
	// ~/.agent-factory/repos/<id>/config.json fallback.
	RepoConfigSchemaVersion = LegacySchemaVersion
	// StateSchemaVersion adds schema_version to the help-screen state.json
	// object additively. Existing files without the field still load.
	StateSchemaVersion = 1
	// TUIStateSchemaVersion starts at v1 because tui-state.json is greenfield
	// (#1240) and must carry schema_version from its first release. There is no
	// v0/bare-version migration for this store.
	TUIStateSchemaVersion = 1
	// InstancesSchemaVersion envelopes the legacy array-root instances.json as
	// {schema_version, instances}.
	InstancesSchemaVersion = 1
)
