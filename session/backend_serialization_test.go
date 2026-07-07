package session

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Serialization round-trip ---

func TestToInstanceDataIncludesBackendType(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		i := &Instance{
			Title:   "local-inst",
			backend: &LocalBackend{},
		}
		data := i.ToInstanceData()
		assert.Equal(t, "local", data.BackendType)
		assert.Nil(t, data.RemoteMeta)
	})

	t.Run("remote", func(t *testing.T) {
		meta := map[string]interface{}{"name": "test", "status": "running"}
		i := &Instance{
			Title:      "remote-inst",
			backend:    &HookBackend{Hooks: config.RemoteHooks{}},
			remoteMeta: meta,
		}
		data := i.ToInstanceData()
		assert.Equal(t, "remote", data.BackendType)
		assert.Equal(t, "test", data.RemoteMeta["name"])
		assert.Equal(t, "running", data.RemoteMeta["status"])
	})
}

func TestInstanceDataJSONRoundTrip(t *testing.T) {
	t.Run("local backend serializes correctly", func(t *testing.T) {
		data := InstanceData{
			Title:       "test-local",
			Path:        "/tmp/test",
			Branch:      "main",
			Status:      Running,
			BackendType: "local",
			Program:     "claude",
		}

		jsonBytes, err := json.Marshal(data)
		require.NoError(t, err)

		var restored InstanceData
		err = json.Unmarshal(jsonBytes, &restored)
		require.NoError(t, err)

		assert.Equal(t, "local", restored.BackendType)
		assert.Equal(t, "test-local", restored.Title)
		assert.Nil(t, restored.RemoteMeta)
	})

	t.Run("remote backend serializes correctly", func(t *testing.T) {
		meta := map[string]interface{}{
			"name":   "fix-bug",
			"status": "running",
			"host":   "remote-1.example.com",
		}
		data := InstanceData{
			Title:       "test-remote",
			Path:        "/tmp/test",
			Branch:      "fix-bug",
			Status:      Running,
			BackendType: "remote",
			RemoteMeta:  meta,
		}

		jsonBytes, err := json.Marshal(data)
		require.NoError(t, err)

		var restored InstanceData
		err = json.Unmarshal(jsonBytes, &restored)
		require.NoError(t, err)

		assert.Equal(t, "remote", restored.BackendType)
		assert.Equal(t, "fix-bug", restored.RemoteMeta["name"])
		assert.Equal(t, "running", restored.RemoteMeta["status"])
		assert.Equal(t, "remote-1.example.com", restored.RemoteMeta["host"])
	})

	t.Run("empty backend_type defaults to empty string", func(t *testing.T) {
		// Simulate old data without backend_type
		jsonStr := `{"title":"old-inst","path":"/tmp","branch":"main","status":0}`
		var restored InstanceData
		err := json.Unmarshal([]byte(jsonStr), &restored)
		require.NoError(t, err)
		assert.Equal(t, "", restored.BackendType)
		assert.Nil(t, restored.RemoteMeta)
	})
}
