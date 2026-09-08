package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveOwnerThroughHomeAlias(t *testing.T) {
	for _, source := range []string{"disk", "memory"} {
		t.Run(source, func(t *testing.T) {
			m, repoID, repoPath := newStatusTestManager(t)
			contender, _ := registerArchivable(t, m, repoID, repoPath, "worker")
			dest, err := archivedWorktreePath(repoID, contender.Title)
			require.NoError(t, err)
			parent := filepath.Dir(dest)
			require.NoError(t, os.MkdirAll(dest, 0755))
			alias := filepath.Join(t.TempDir(), "old-home")
			require.NoError(t, os.Symlink(parent, alias))
			recorded := filepath.Join(alias, filepath.Base(dest))
			owner := session.InstanceData{Title: "renamed owner", Status: session.Archived, Worktree: session.GitWorktreeData{WorktreePath: recorded}}
			if source == "disk" {
				require.NoError(t, appendInstanceData(repoID, owner))
			} else {
				inst, err := session.NewInstance(session.InstanceOptions{Title: owner.Title, Path: repoPath, Program: "claude"})
				require.NoError(t, err)
				gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, recorded, owner.Title, "branch", "", false, true)
				require.NoError(t, err)
				inst.SetGitWorktreeForTest(gw)
				inst.SetStatusForTest(session.Archived)
				m.instances[daemonInstanceKey(repoID, owner.Title)] = inst
			}
			_, err = m.checkArchiveDestination(repoID, contender, dest, contender.GetWorktreePath())
			require.ErrorContains(t, err, `belongs to session "renamed owner"`)
			require.Empty(t, m.reservedArchiveDestinations)
		})
	}
}

func TestArchiveMismatchedPathPreservesFutureClaim(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, m, repoID, repoPath, "feature/login", "feature-login")
	oldPath := filepath.Join(filepath.Dir(archived.GetWorktreePath()), "old-name")
	require.NoError(t, archived.RenameArchived(archived.Title, oldPath, ""))
	require.NoError(t, m.persistInstanceErr(repoID, archived))
	installInstantBackend(t)
	_, err := m.CreateSession(context.Background(), CreateSessionRequest{Title: "feature-login", RepoPath: repoPath, Program: "claude"})
	assert.ErrorContains(t, err, `session titled "feature/login" already maps to archive directory "feature-login"`)
	// A refused create must leave the old archive independently restorable.
	_, _, err = m.RestoreArchived(RestoreArchivedRequest{Title: archived.Title, RepoID: repoID})
	require.NoError(t, err)
	require.NotEqual(t, session.LiveArchived, archived.GetLiveness())
}

func TestArchiveReuseSkipsPortableOrphan(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSessionBranchFreed(t, m, repoID, repoPath, "foo", "foo")
	orphan := filepath.Join(filepath.Dir(archived.GetWorktreePath()), "Foo (Archived)")
	require.NoError(t, os.Mkdir(orphan, 0755))
	marker := filepath.Join(orphan, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("orphan data"), 0644))
	_, _, release, renamed, err := m.reserveCreate(CreateSessionRequest{Title: "foo", RepoPath: repoPath, Program: "claude"})
	if release != nil {
		defer release()
	}
	require.NoError(t, err)
	require.NotNil(t, renamed)
	require.Equal(t, "foo (archived 2)", renamed.Title)
	require.Equal(t, "foo (archived 2)", filepath.Base(archived.GetWorktreePath()))
	contents, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "orphan data", string(contents))
}
