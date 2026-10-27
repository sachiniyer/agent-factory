package config

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareGlobalConfigSnapshotBuiltInIsMergedNotDefault(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	repo, err := RepoFromPath(repoPath)
	require.NoError(t, err)

	global := DefaultConfig()
	require.Equal(t, "claude", global.DefaultProgram, "sanity: built-in default is claude")
	global.DefaultProgram = "codex" // operator override away from the built-in default

	resolved, err := ResolveConfigForRepoInspectionWithGlobal(repo, global)
	require.NoError(t, err)

	rv, ok := resolved.ResolvedValue("default_program")
	require.True(t, ok, "default_program is a manifest key")
	require.Equal(t, "codex", rv.Value, "effective value still correct (global wins precedence)")

	var builtInCandidate *CandidateTrace
	for i := range rv.Candidates {
		c := &rv.Candidates[i]
		if c.Layer == SourceBuiltIn.String() {
			builtInCandidate = c
			break
		}
	}
	require.NotNil(t, builtInCandidate, "built-in candidate must appear")
	// Expected: "claude" (real default). Actual with the bug: "codex".
	assert.Equal(t, "claude", builtInCandidate.Value,
		"built-in candidate row must show the real default, not the merged global value")
}
