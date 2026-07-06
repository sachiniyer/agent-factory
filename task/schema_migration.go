package task

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

type tasksEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Tasks         json.RawMessage `json:"tasks"`
}

func newTasksSchemaMigrationPlan(path string) config.SchemaMigrationPlan {
	registry := config.NewSchemaMigrationRegistry()
	if err := registry.Register(config.LegacySchemaVersion, migrateLegacyTasksArray); err != nil {
		panic(err)
	}
	return config.SchemaMigrationPlan{
		StoreName:      tasksFileName,
		Path:           path,
		CurrentVersion: TasksSchemaVersion,
		DetectVersion:  detectTasksSchemaVersion,
		Migrators:      registry,
		Validate:       validateTasksEnvelope,
		Perm:           0644,
	}
}

func loadAndMigrateTasksFile(path string) ([]byte, config.SchemaMigrationResult, error) {
	return config.LoadAndMigrateSchemaFile(newTasksSchemaMigrationPlan(path))
}

func migrateTasksSchemaBytes(raw []byte, path string) ([]byte, config.SchemaMigrationResult, error) {
	return config.MigrateSchemaBytes(raw, newTasksSchemaMigrationPlan(path))
}

func migrateLegacyTasksArray(raw []byte) ([]byte, error) {
	return marshalTasksEnvelope(raw)
}

func marshalTasksEnvelope(tasks json.RawMessage) ([]byte, error) {
	normalized, err := normalizeTasksArray(tasks)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(tasksEnvelope{
		SchemaVersion: TasksSchemaVersion,
		Tasks:         normalized,
	}, "", "  ")
}

func tasksFromSchemaBytes(raw []byte) ([]Task, error) {
	var envelope tasksEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("failed to parse tasks envelope: %w", err)
	}
	var tasks []Task
	if err := json.Unmarshal(envelope.Tasks, &tasks); err != nil {
		return nil, fmt.Errorf("failed to parse tasks list: %w", err)
	}
	return tasks, nil
}

func validateTasksEnvelope(raw []byte) error {
	var envelope tasksEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("failed to parse tasks envelope: %w", err)
	}
	if envelope.SchemaVersion != TasksSchemaVersion {
		return fmt.Errorf("schema_version = %d, want %d", envelope.SchemaVersion, TasksSchemaVersion)
	}
	if _, err := normalizeTasksArray(envelope.Tasks); err != nil {
		return err
	}
	return nil
}

func detectTasksSchemaVersion(raw []byte) (int, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return config.LegacySchemaVersion, nil
	}
	return config.DetectJSONSchemaVersion(raw)
}

func normalizeTasksArray(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("tasks must be a JSON array")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("[]"), nil
	}
	var tasks []json.RawMessage
	if err := json.Unmarshal(trimmed, &tasks); err != nil {
		return nil, fmt.Errorf("tasks must be a JSON array: %w", err)
	}
	if tasks == nil {
		return json.RawMessage("[]"), nil
	}
	out, err := json.Marshal(tasks)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tasks array: %w", err)
	}
	return json.RawMessage(out), nil
}
