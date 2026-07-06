package task

import "github.com/sachiniyer/agent-factory/config"

const (
	// TasksSchemaVersion is v0 for the current array-root tasks.json. The #1046
	// envelope migration will advance it in a later PR.
	TasksSchemaVersion = config.LegacySchemaVersion
)
