package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestReserveCreate_InPlaceOnConfiguredRemoteBackendRefusesBeforeAnyRename is
// the mutation-ordering half of #2778.
//
// NewInstance now refuses an in-place create whose backend resolves to a sandbox
// runtime — but NewInstance runs after reserveCreate, and reserveCreate mutates.
// For an explicit title held only by an archived session it renames that session
// out of the way first: worktree relocated, durable record rewritten, manager map
// re-keyed, none of it reversible. A refusal that arrives afterwards leaves that
// rename standing for a create that could never have succeeded — the exact state
// reserveCreate's admission comment promises it never produces, and the same
// invariant #2127 and #2415 exist to protect.
//
// Note the namespaces deliberately differ: the archived session is local, the
// create resolves to docker. They still collide, because titlesCollide compares
// the derived branch and knows nothing about runtimes — which is what makes the
// rename reachable here at all.
func TestReserveCreate_InPlaceOnConfiguredRemoteBackendRefusesBeforeAnyRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Seeded BEFORE the backend config lands: the fixture builds a real local
	// worktree, which a docker-configured repo would send to a provisioner.
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	archivedPath := archived.GetWorktreePath()
	writeRepoBackendConfig(t, repoPath, map[string]any{"backend": "docker"})

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "foo",
		Program:  "claude",
		InPlace:  true,
	})
	if release != nil {
		defer release()
	}

	require.Error(t, err, "an in-place create cannot run on the repo's docker backend")
	assert.ErrorIs(t, err, session.ErrInPlaceRemoteBackend)
	assert.Nil(t, renamed, "the refusal must land before the archived-name reuse rename, not after it")

	// The archived session is untouched — the point of refusing early.
	assert.Equal(t, "foo", archived.Title)
	assert.Equal(t, archivedPath, archived.GetWorktreePath())
	assert.True(t, exists(archivedPath))
	assert.Nil(t, recordFor(t, repoID, "foo (archived)"),
		"no disambiguated archive row may exist: nothing was renamed")
	rec := recordFor(t, repoID, "foo")
	require.NotNil(t, rec)
	assert.Equal(t, id, rec.ID)
	assert.Equal(t, archivedPath, rec.Worktree.WorktreePath)
}

// TestReserveCreate_InPlaceOnLocalRepoStillReservesTheTitle is the
// non-regression half: the guard must not touch the ordinary in-place create,
// which is how the daemon's own root agent is made.
func TestReserveCreate_InPlaceOnLocalRepoStillReservesTheTitle(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	repo, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "here",
		Program:  "claude",
		InPlace:  true,
	})
	require.NoError(t, err)
	require.NotNil(t, release)
	defer release()

	assert.Equal(t, repoID, repo.ID)
	assert.Equal(t, "here", title)
	assert.Nil(t, renamed)
}

// TestReserveCreate_RootAgentSurvivesARemoteBackendRepo pins the one in-place
// caller that is not a user request: the daemon's always-ensured root agent.
//
// A root agent is documented as the `af sessions create --here` shape — in-place
// at the repo root, no worktree, no branch — so it is local by construction. Left
// to resolve the repo's `backend` key it would have been read as a docker/ssh/hook
// create, which before #2778 silently produced a root running in a sandbox clone
// that could not see the working tree its record claimed, and after #2778 would
// have taken the always-on guarantee away from those repos instead.
//
// The reserved title makes this a real root create rather than a lookalike.
func TestReserveCreate_RootAgentSurvivesARemoteBackendRepo(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	writeRepoBackendConfig(t, repoPath, map[string]any{"backend": "docker"})

	repo, title, release, _, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath:      repoPath,
		Title:         session.RootSessionTitle,
		Program:       "claude",
		InPlace:       true,
		Backend:       string(session.BackendLocal),
		allowReserved: true,
	})
	require.NoError(t, err, "the daemon's in-place root agent must still be creatable in a repo whose config selects a sandbox backend")
	require.NotNil(t, release)
	defer release()

	assert.Equal(t, repoID, repo.ID)
	assert.Equal(t, session.RootSessionTitle, title)
}
