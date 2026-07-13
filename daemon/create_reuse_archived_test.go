package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedArchivedSession builds a real linked worktree (dir/branch derived from the
// git-safe slug, so a title carrying spaces/parens still gets a valid branch),
// registers a started instance for it in the manager and on disk, then archives
// it. The instance ends inert (LiveArchived) with its worktree relocated to the
// title-keyed archive dir — the precondition for the reuse-archived-name tests.
// Returns the archived instance and the stable id it was minted with.
func seedArchivedSession(t *testing.T, m *Manager, repoID, repoPath, title, slug string) (*session.Instance, string) {
	t.Helper()
	wtPath := filepath.Join(filepath.Dir(repoPath), "wt-"+slug)
	branch := "af/" + slug
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted-"+slug), 0644))

	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, branch, "", false, true)
	require.NoError(t, err)

	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	inst.SetGitWorktreeForTest(gw)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	id := inst.ID

	// appendInstanceData (not seedDiskInstance) so multiple seeded sessions
	// accumulate on disk instead of clobbering one another.
	require.NoError(t, appendInstanceData(repoID, session.InstanceData{ID: id, Title: title, Path: repoPath, Status: session.Ready}))
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()

	_, _, err = m.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, session.Archived, inst.GetStatus())
	return inst, id
}

// TestReserveCreate_ReusesArchivedName is the headline case (feat: reuse archived
// name): archive "foo", then create a NEW "foo" — the create succeeds and the old
// archived session is renamed to "foo (archived)", keyed and persisted under the
// new name, its worktree relocated to the new title-keyed archive dir, and still
// fully restorable (same worktree contents, branch, and stable id).
func TestReserveCreate_ReusesArchivedName(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	branch := archived.GetBranch()

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	require.NoError(t, err, "creating a new session must succeed when only an archived session holds the name")
	defer release()

	assert.Equal(t, "foo", title, "the new session takes the freed title verbatim")
	require.NotNil(t, renamed, "the collision must have renamed the archived session")
	assert.Equal(t, "foo (archived)", renamed.Title)

	// The archived instance now carries the disambiguated name, re-keyed in the map.
	assert.Equal(t, "foo (archived)", archived.Title)
	manager.mu.Lock()
	_, oldKeyed := manager.instances[daemonInstanceKey(repoID, "foo")]
	renamedInst, newKeyed := manager.instances[daemonInstanceKey(repoID, "foo (archived)")]
	manager.mu.Unlock()
	assert.False(t, oldKeyed, "the archived row must no longer be keyed under the freed name")
	require.True(t, newKeyed, "the archived row must be keyed under its new name")
	assert.Same(t, archived, renamedInst, "re-keying preserves the same instance object (stable id)")

	// The archived worktree moved to the new title-keyed archive dir; the old one is gone.
	oldDir, _ := archivedWorktreePath(repoID, "foo")
	newDir, _ := archivedWorktreePath(repoID, "foo (archived)")
	assert.False(t, exists(oldDir), "the pre-rename archive dir must be vacated")
	assert.True(t, exists(newDir), "the archived worktree must live at the new title-keyed dir")

	// Disk reflects the rename: the old title is gone, the new one carries the
	// relocated worktree path and the preserved stable id + branch.
	assert.Nil(t, recordFor(t, repoID, "foo"), "no on-disk record must survive under the freed name")
	rec := recordFor(t, repoID, "foo (archived)")
	require.NotNil(t, rec, "the archived record must be persisted under the new name")
	assert.Equal(t, id, rec.ID, "the stable id must be preserved across the rename")
	assert.Equal(t, branch, rec.Branch, "the git branch must be preserved across the rename")
	assert.Equal(t, newDir, rec.Worktree.WorktreePath)
	assert.Equal(t, session.Archived, rec.Status)

	// The renamed archive is still restorable and brings the original worktree back.
	restorePath, rerr := manager.RestoreArchived(RestoreArchivedRequest{Title: "foo (archived)", RepoID: repoID})
	require.NoError(t, rerr, "the renamed archived session must still restore")
	dirty, drr := os.ReadFile(filepath.Join(restorePath, "dirty.txt"))
	require.NoError(t, drr, "the original worktree contents must come back on restore")
	assert.Equal(t, "uncommitted-foo", string(dirty))
	assert.Equal(t, branch, archived.GetBranch(), "restore preserves the original branch")
	assert.Equal(t, id, archived.ID, "restore preserves the original stable id")
}

// TestReserveCreate_ReusesArchivedName_SuffixCollision: with BOTH archived "foo"
// and archived "foo (archived)" already present, creating a new "foo" renames the
// old archived "foo" to the next free slot, "foo (archived 2)".
func TestReserveCreate_ReusesArchivedName_SuffixCollision(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	seedArchivedSession(t, manager, repoID, repoPath, "foo (archived)", "foo-archived")

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	require.NoError(t, err)
	defer release()

	assert.Equal(t, "foo", title)
	require.NotNil(t, renamed)
	assert.Equal(t, "foo (archived 2)", renamed.Title, "the first suffix is taken, so the rename skips to (archived 2)")

	manager.mu.Lock()
	_, keyed := manager.instances[daemonInstanceKey(repoID, "foo (archived 2)")]
	manager.mu.Unlock()
	assert.True(t, keyed, "the archived row must be keyed under the disambiguated name")
	assert.NotNil(t, recordFor(t, repoID, "foo (archived)"), "the pre-existing archived (archived) row must be untouched")
	assert.NotNil(t, recordFor(t, repoID, "foo (archived 2)"))
}

// TestReserveCreate_LiveNameStillBlocks: a LIVE (non-archived) "foo" still blocks
// an explicit new "foo" — the reuse-archived rename must NOT fire around a name a
// live session holds.
func TestReserveCreate_LiveNameStillBlocks(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// A live, non-archived session holding "foo".
	registerArchivable(t, manager, repoID, repoPath, "foo") // status Ready

	_, _, _, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	require.Error(t, err, "a live session must still block an explicit same-named create")
	assert.Contains(t, err.Error(), "already exists")
	assert.Nil(t, renamed, "no archived rename may happen around a live collision")
}

// TestReserveCreate_TitleBaseStillAutoSuffixes: the derived-title (title_base)
// path is unchanged — it auto-suffixes around an existing session (live OR
// archived) rather than renaming it.
func TestReserveCreate_TitleBaseStillAutoSuffixes(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, TitleBase: "foo", Program: "claude"})
	require.NoError(t, err)
	defer release()

	assert.Equal(t, "foo-2", title, "title_base auto-suffixes around the archived name instead of reusing it")
	assert.Nil(t, renamed, "title_base must never trigger the archived rename")
	assert.NotNil(t, recordFor(t, repoID, "foo"), "the archived session keeps its original name under the title_base path")
}
