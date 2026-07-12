package session

import (
	"encoding/json"
	"testing"

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
	})

	t.Run("remote", func(t *testing.T) {
		// #1592 Phase 4 PR7: a remote-hook session persists only its backend
		// discriminator; the old remote_meta session-id metadata is gone (the
		// durable handle is the git branch on origin, re-provisioned on restore).
		i := &Instance{
			Title:   "remote-inst",
			backend: &HookBackend{},
		}
		data := i.ToInstanceData()
		assert.Equal(t, "remote", data.BackendType)
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
	})

	t.Run("remote backend serializes correctly", func(t *testing.T) {
		data := InstanceData{
			Title:       "test-remote",
			Path:        "/tmp/test",
			Branch:      "fix-bug",
			Status:      Running,
			BackendType: "remote",
		}

		jsonBytes, err := json.Marshal(data)
		require.NoError(t, err)

		var restored InstanceData
		err = json.Unmarshal(jsonBytes, &restored)
		require.NoError(t, err)

		assert.Equal(t, "remote", restored.BackendType)
		assert.Equal(t, "fix-bug", restored.Branch)
	})

	t.Run("empty backend_type defaults to empty string", func(t *testing.T) {
		// Simulate old data without backend_type
		jsonStr := `{"title":"old-inst","path":"/tmp","branch":"main","status":0}`
		var restored InstanceData
		err := json.Unmarshal([]byte(jsonStr), &restored)
		require.NoError(t, err)
		assert.Equal(t, "", restored.BackendType)
	})
}
