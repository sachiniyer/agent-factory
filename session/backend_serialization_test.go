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

func TestToInstanceDataSandboxRepoPath(t *testing.T) {
	// #1933: a remote session owns no local worktree, so the settled projection
	// used to carry an empty Worktree — and repo-scoped consumers (the web
	// rail's project filter, the project switcher) could not attribute it to
	// ANY project. The pending-create row already publishes the repo as
	// Worktree.RepoPath; the committed projection must keep it.
	t.Run("sandbox backend without a worktree keeps its repo identity", func(t *testing.T) {
		i := &Instance{
			Title:            "remote-inst",
			Path:             "/srv/repo",
			repoIdentityPath: "/srv/repo",
			backend:          &HookBackend{},
		}
		data := i.ToInstanceData()
		assert.Equal(t, "remote", data.BackendType)
		assert.Equal(t, "/srv/repo", data.Worktree.RepoPath)
		// Only the repo identity is synthesized: no local worktree exists, so
		// the rest of the worktree record stays empty.
		assert.Equal(t, "", data.Worktree.WorktreePath)
	})

	t.Run("sandbox backend never projects the workspace as identity", func(t *testing.T) {
		// A record that never carried a canonical identity — anything predating
		// the field — emits an empty RepoPath rather than publishing Path as if
		// it were canonical: consumers hash a nonempty RepoPath verbatim, so the
		// operational linked checkout would move the row to a bogus project.
		i := &Instance{
			Title:   "remote-legacy",
			Path:    "/srv/bare/linked-checkout",
			backend: &HookBackend{},
		}
		assert.Equal(t, "", i.ToInstanceData().Worktree.RepoPath)
	})

	t.Run("sandbox backend projects the canonical identity, not the workspace", func(t *testing.T) {
		// A repo registered through a bare repository's linked worktree has an
		// operational workspace Path that differs from the canonical
		// IdentityPath the pending row and the durable repo key use. The settled
		// projection must carry the identity path or the row would jump projects
		// when creation settles — the same disappearance the field exists to
		// prevent.
		i := &Instance{
			Title:            "remote-linked",
			Path:             "/srv/bare/linked-checkout",
			repoIdentityPath: "/srv/bare.git",
			backend:          &HookBackend{},
		}
		assert.Equal(t, "/srv/bare.git", i.ToInstanceData().Worktree.RepoPath)
	})

	t.Run("sandbox repo identity survives the record round-trip", func(t *testing.T) {
		data := InstanceData{
			Title:       "remote-linked",
			Path:        "/srv/bare/linked-checkout",
			BackendType: "remote",
			Worktree:    GitWorktreeData{RepoPath: "/srv/bare.git"},
		}
		reloaded, err := FromInstanceData(data)
		require.NoError(t, err)
		assert.Equal(t, "/srv/bare.git", reloaded.ToInstanceData().Worktree.RepoPath)
	})

	t.Run("local backend without a worktree leaves RepoPath empty", func(t *testing.T) {
		// Tombstone semantics (#476 area): a local record whose worktree was
		// already cleared must keep the empty worktree — worktreeReaped keys on
		// RepoPath == "" && WorktreePath == "".
		i := &Instance{
			Title:   "local-inst",
			Path:    "/srv/repo",
			backend: &LocalBackend{},
		}
		data := i.ToInstanceData()
		assert.Equal(t, "", data.Worktree.RepoPath)
	})
}

func TestInstanceDataUsesLocalTmux(t *testing.T) {
	for _, tc := range []struct {
		backendType string
		want        bool
	}{
		{"", true}, // legacy rows predate the discriminator and were local
		{"local", true},
		{"remote", false},
		{"docker", false},
		{"ssh", false},
	} {
		t.Run(tc.backendType, func(t *testing.T) {
			assert.Equal(t, tc.want, (InstanceData{BackendType: tc.backendType}).UsesLocalTmux())
		})
	}
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
