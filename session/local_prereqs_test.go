package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLocalPrereqsRequired is the one predicate behind every surface's
// local-prerequisite gate (#2592). It follows the same selection precedence
// NewInstance uses, which is the whole reason it exists: a surface that gates
// on the explicit pick alone misses the repo's `backend` key, and that is the
// shape the CLI bug arrived in.
func TestLocalPrereqsRequired(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("no selection anywhere is a local create", func(t *testing.T) {
		got, err := LocalPrereqsRequired(InstanceOptions{}, t.TempDir())
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("an explicit sandbox backend is not", func(t *testing.T) {
		for _, kind := range []BackendKind{BackendDocker, BackendSSH, BackendHook} {
			got, err := LocalPrereqsRequired(InstanceOptions{Backend: kind}, t.TempDir())
			require.NoError(t, err, "backend %s", kind)
			assert.False(t, got, "backend %s runs the agent off-box", kind)
		}
	})

	t.Run("an explicit local backend is", func(t *testing.T) {
		got, err := LocalPrereqsRequired(InstanceOptions{Backend: BackendLocal}, t.TempDir())
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("the legacy ForceRemote selector is not", func(t *testing.T) {
		got, err := LocalPrereqsRequired(InstanceOptions{ForceRemote: true}, t.TempDir())
		require.NoError(t, err)
		assert.False(t, got)
	})

	// The case the CLI gate was missing: nothing is passed in at all, and the
	// repo's own config is what makes the session non-local.
	t.Run("the repo backend key decides an unflagged create", func(t *testing.T) {
		repoRoot := initTempGitRepo(t)
		writeInRepoConfig(t, repoRoot, map[string]any{
			"backend": "docker",
			"docker":  map[string]any{"image": "alpine:3.20"},
		})
		got, err := LocalPrereqsRequired(InstanceOptions{}, repoRoot)
		require.NoError(t, err)
		assert.False(t, got)
	})

	// An explicit pick outranks the repo key in BOTH directions, so a `local`
	// pick in a docker repo still needs the local prerequisites.
	t.Run("an explicit local pick outranks a docker repo", func(t *testing.T) {
		repoRoot := initTempGitRepo(t)
		writeInRepoConfig(t, repoRoot, map[string]any{
			"backend": "docker",
			"docker":  map[string]any{"image": "alpine:3.20"},
		})
		got, err := LocalPrereqsRequired(InstanceOptions{Backend: BackendLocal}, repoRoot)
		require.NoError(t, err)
		assert.True(t, got)
	})

	// The third outcome. A backend nobody can resolve is neither answer, and the
	// error is how a caller tells "the prerequisites do not apply" apart from
	// "we could not work out whether they apply" — collapsing the two is what
	// made an unknown backend read as a missing tmux.
	t.Run("an unresolvable backend answers neither way", func(t *testing.T) {
		repoRoot := initTempGitRepo(t)
		writeInRepoConfig(t, repoRoot, map[string]any{"backend": "moonbase"})
		_, err := LocalPrereqsRequired(InstanceOptions{}, repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "moonbase")
	})
}
