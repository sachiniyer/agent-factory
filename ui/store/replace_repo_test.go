package store

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRepoProjectionInstance builds a listed instance whose RepoName() resolves
// to repoName — for an off-box row that is filepath.Base(Path) (W8.8) — so the
// projection's repo bookkeeping (which drives the multi-repo indicator) can be
// exercised by membership rather than only by count.
func newRepoProjectionInstance(t *testing.T, title, repoName string) *session.Instance {
	t.Helper()
	inst, err := session.FromInstanceData(session.InstanceData{
		ID: title, Title: title, Path: filepath.Join("/srv", repoName),
		Program: "claude", BackendType: "docker", Liveness: session.LiveReady,
	})
	require.NoError(t, err)
	return inst
}

// TestReplaceInstanceRepoTrackingStaleEntry: replacing an instance with one from a
// DIFFERENT repo must drop the outgoing instance's repo from the bookkeeping —
// otherwise a kill+recreate that moves a session to another repo leaves a stale
// repo entry and shows a phantom multi-repo indicator (#971).
func TestReplaceInstanceRepoTrackingStaleEntry(t *testing.T) {
	p := NewProjection()

	a := newRepoProjectionInstance(t, "a", "repo-A")
	c := newRepoProjectionInstance(t, "c", "repo-C")
	p.AddInstance(a)()
	p.AddInstance(c)()
	require.Equal(t, 2, p.NumRepos())

	// Replace the repo-A instance with one from repo-B.
	b := newRepoProjectionInstance(t, "b", "repo-B")
	require.True(t, p.ReplaceInstance(a, b))

	_, staleA := p.repos["repo-A"]
	assert.False(t, staleA, "outgoing instance's repo (repo-A) must be dropped after a cross-repo replace")
	assert.Equal(t, 2, p.NumRepos(), "repos must be {repo-B, repo-C} after the replace")
}

// TestReplaceInstanceRepoTrackingMissingNew: replacing an instance with one from a
// DIFFERENT repo must register the incoming instance's repo — otherwise the new
// repo is invisible to the multi-repo indicator until the next full reload (#971).
func TestReplaceInstanceRepoTrackingMissingNew(t *testing.T) {
	p := NewProjection()

	a := newRepoProjectionInstance(t, "a", "repo-A")
	p.AddInstance(a)()
	require.Equal(t, 1, p.NumRepos())

	// Replace the only instance with one from repo-B.
	b := newRepoProjectionInstance(t, "b", "repo-B")
	require.True(t, p.ReplaceInstance(a, b))

	_, hasB := p.repos["repo-B"]
	assert.True(t, hasB, "incoming instance's repo (repo-B) must be registered after a cross-repo replace")
	_, staleA := p.repos["repo-A"]
	assert.False(t, staleA, "outgoing repo-A must not linger")
	assert.Equal(t, 1, p.NumRepos(), "repos must be exactly {repo-B} after the replace")
}
