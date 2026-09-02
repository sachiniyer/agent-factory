package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hookScopeInstanceData(t *testing.T, prefix string) InstanceData {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	return InstanceData{
		ID:       "8f0e1d2c-4444-4000-8000-1a2b3c4d5e6f",
		Title:    "scoped",
		Path:     t.TempDir(),
		Liveness: LiveArchived,
		Worktree: GitWorktreeData{
			RepoPath:            t.TempDir(),
			WorktreePath:        wt,
			SessionName:         "scoped",
			BranchName:          "af/scoped",
			HookScopeUnitPrefix: prefix,
		},
	}
}

// The recorded handle has to come back, or a daemon that restarts mid-build has
// no way to name the scope its predecessor left running (#3650).
func TestHookScopeUnitPrefixRoundTripsThroughStorage(t *testing.T) {
	const prefix = "af-hook-8f0e1d2c-4444-4000-8000-1a2b3c4d5e6f"
	restored, err := FromInstanceData(hookScopeInstanceData(t, prefix))
	require.NoError(t, err)
	require.NotNil(t, restored.gitWorktree)
	assert.Equal(t, prefix, restored.gitWorktree.HookScopeUnitPrefix())
	assert.Equal(t, prefix, restored.ToInstanceData().ForStorage().Worktree.HookScopeUnitPrefix)
}

// Absence is the contract, not an oversight: a legacy record and a record whose
// hooks never entered a scope must be INDISTINGUISHABLE, because both mean "take
// the behaviour that shipped before #3650" — no scope sweep, no systemd call.
func TestAbsentHookScopeUnitPrefixMeansPreScopeBehaviour(t *testing.T) {
	restored, err := FromInstanceData(hookScopeInstanceData(t, ""))
	require.NoError(t, err)
	require.NotNil(t, restored.gitWorktree)
	assert.Equal(t, "", restored.gitWorktree.HookScopeUnitPrefix())

	encoded, err := json.Marshal(restored.ToInstanceData().ForStorage())
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "hook_scope_unit_prefix",
		"an unset handle must be omitted entirely so it is byte-identical to a pre-#3650 record")
}

// A binary rolled back past this change decodes instances.json into a shape with
// no such field and writes it back without one. The current reader must load
// that stripped record as "no scope", not as an error or a wedge.
func TestRolledBackWriterDropsTheHookScopeHandleSafely(t *testing.T) {
	const prefix = "af-hook-8f0e1d2c-4444-4000-8000-1a2b3c4d5e6f"
	current, err := json.Marshal(hookScopeInstanceData(t, prefix))
	require.NoError(t, err)
	require.Contains(t, string(current), "hook_scope_unit_prefix")

	// The old reader's shape: every field it knew about, and nothing else.
	var oldReader struct {
		ID       string          `json:"id,omitempty"`
		Title    string          `json:"title"`
		Path     string          `json:"path"`
		Liveness Liveness        `json:"liveness,omitempty"`
		Worktree json.RawMessage `json:"worktree"`
	}
	require.NoError(t, json.Unmarshal(current, &oldReader))
	var oldWorktree struct {
		RepoPath     string `json:"repo_path"`
		WorktreePath string `json:"worktree_path"`
		SessionName  string `json:"session_name"`
		BranchName   string `json:"branch_name"`
	}
	require.NoError(t, json.Unmarshal(oldReader.Worktree, &oldWorktree))
	rewritten, err := json.Marshal(map[string]any{
		"id":       oldReader.ID,
		"title":    oldReader.Title,
		"path":     oldReader.Path,
		"liveness": oldReader.Liveness,
		"worktree": oldWorktree,
	})
	require.NoError(t, err)
	require.False(t, strings.Contains(string(rewritten), "hook_scope_unit_prefix"))

	var afterRollback InstanceData
	require.NoError(t, json.Unmarshal(rewritten, &afterRollback))
	restored, err := FromInstanceData(afterRollback)
	require.NoError(t, err)
	require.NotNil(t, restored.gitWorktree)
	assert.Equal(t, "", restored.gitWorktree.HookScopeUnitPrefix(),
		"a handle dropped by a rolled-back writer must land in pre-#3650 behaviour, not a half state")
}
