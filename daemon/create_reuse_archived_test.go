package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedArchivedSession builds a real linked worktree (dir derived from the
// git-safe slug, so a title carrying spaces/parens still gets a valid path),
// registers a started instance for it in the manager and on disk, then archives
// it. The instance ends inert (LiveArchived) with its worktree relocated to the
// title-keyed archive dir — the precondition for the reuse-archived-name tests.
// Returns the archived instance and the stable id it was minted with.
//
// The branch is m.branchForTitle(title) — the SAME derivation a real create
// applies to the same title — not a fixture-local invention (#2127). Seeding
// "af/"+slug while branchForTitle yields "<prefix><title>" meant the fixture's
// branch could never collide with the branch a create derives, so these tests
// passed on a shape production never produces: they exercised freeing the TITLE
// while silently skipping the BRANCH hold that is the whole difficulty here.
func seedArchivedSession(t *testing.T, m *Manager, repoID, repoPath, title, slug string) (*session.Instance, string) {
	t.Helper()
	wtPath := filepath.Join(filepath.Dir(repoPath), "wt-"+slug)
	branch := m.branchForTitle(title)
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted-"+slug), 0644))

	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, branch, "", false, true)
	require.NoError(t, err)

	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	inst.SetGitWorktreeForTest(gw)
	// Provision sets i.Branch on a real create; SetGitWorktreeForTest does not, and
	// InstanceData.Branch reads i.Branch — so without this every "the branch is
	// preserved" assertion below compared "" to "" and asserted nothing.
	inst.Branch = branch
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	id := inst.ID

	// appendInstanceData (not seedDiskInstance) so multiple seeded sessions
	// accumulate on disk instead of clobbering one another.
	require.NoError(t, appendInstanceData(repoID, session.InstanceData{ID: id, Title: title, Path: repoPath, Branch: branch, Status: session.Ready}))
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()

	_, _, err = m.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, session.Archived, inst.GetStatus())
	return inst, id
}

// seedArchivedSessionBranchFreed seeds an archived session exactly as
// seedArchivedSession does, then detaches the archived worktree's HEAD so its
// branch is no longer checked out anywhere.
//
// This is the shape in which reuse-archived-name can actually COMPLETE, and it
// is what the tests below that exercise the rename machinery — the suffix walk,
// the Loading-ghost overwrite, the #2106 double-failure recovery — need in order
// to reach that machinery at all: with the branch still held, #2127's guard
// refuses before the rename runs, which is the whole point of the guard and not
// what those tests are about.
//
// Detaching is a constructed shape, not what archiving produces (#2013 leaves
// the branch checked out), and saying so is the honest framing: it is reachable
// — a user can check something else out inside an archived worktree — and it is
// what option 1 on #2127 would make the DEFAULT if archiving is ever changed to
// release the branch. The worktree itself is untouched, so relocation and
// restore behave identically.
func seedArchivedSessionBranchFreed(t *testing.T, m *Manager, repoID, repoPath, title, slug string) (*session.Instance, string) {
	t.Helper()
	inst, id := seedArchivedSession(t, m, repoID, repoPath, title, slug)

	archivedPath, err := archivedWorktreePath(repoID, title)
	require.NoError(t, err)
	out, err := exec.Command("git", "-C", archivedPath, "checkout", "--detach").CombinedOutput()
	require.NoError(t, err, string(out))

	// Not merely detached on paper: git must no longer report the branch as held,
	// or the tests relying on this would silently be testing the guarded shape.
	held, err := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, err)
	require.NotContains(t, held, m.branchForTitle(title),
		"the seeded archived worktree must have released its branch")
	return inst, id
}

// TestReserveCreate_ReusesArchivedName is the headline case (feat: reuse archived
// name): archive "foo", then create a NEW "foo" — the create succeeds and the old
// archived session is renamed to "foo (archived)", keyed and persisted under the
// new name, its worktree relocated to the new title-keyed archive dir, and still
// fully restorable (same worktree contents, branch, and stable id).
//
// Seeded with the branch FREED, which is now load-bearing rather than incidental
// (#2127). The reuse can only complete when the archived session is not still
// holding the branch the new session derives; with it held, the guard refuses
// (TestReserveCreate_HeldArchivedBranchRefusesBeforeRename below). The old
// fixture hid that distinction entirely by seeding an "af/"+slug branch that
// branchForTitle never produces, so this test used to pass while asserting
// nothing about the branch at all.
func TestReserveCreate_ReusesArchivedName(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	branch := archived.GetBranch()

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	require.NoError(t, err, "creating a new session must succeed when only an archived session holds the name")
	defer release()

	assert.Equal(t, "foo", title, "the new session takes the freed title verbatim")
	require.NotNil(t, renamed, "the collision must have renamed the archived session")
	assert.Equal(t, "foo (archived)", renamed.Title)

	// The gate this test was missing (#2127): a granted title is worth nothing if
	// the create it authorizes cannot build its worktree. Freeing the NAME while
	// the BRANCH stays checked out is exactly the failure the guard now refuses
	// up front, and only actually running the add can tell the two apart.
	//
	// `worktree add <path> <branch>` with no -b is the form AF itself runs when
	// the derived branch already exists (setupFromExistingBranch), and it is the
	// command that reports "already used by worktree at …" on a held branch.
	dest := filepath.Join(t.TempDir(), "new-session")
	out, addErr := exec.Command("git", "-C", repoPath, "worktree", "add", dest,
		manager.branchForTitle(title)).CombinedOutput()
	require.NoError(t, addErr, "the granted title %q must be usable by the create it was granted for: %s", title, string(out))
	// Vacate it again so the restore assertions below see the archive layout the
	// rest of this test is about, not this probe's leftovers.
	out, err = exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", dest).CombinedOutput()
	require.NoError(t, err, string(out))

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

// seedLoadingGhost plants a legacy Loading-status ghost record (#551) alongside
// whatever is already on disk for repoID. It reads the current rows, appends the
// ghost with a literal Status==Loading and an empty id/worktree, and writes the
// combined array RAW — deliberately NOT through appendInstanceData, because
// ForStorage() would recompose the Loading status into Ready and the row would
// stop being a ghost. This mirrors exactly what an older TUI binary left on disk.
func seedLoadingGhost(t *testing.T, repoID, title, repoPath string) {
	t.Helper()
	data, err := loadRepoInstanceData(repoID)
	require.NoError(t, err)
	data = append(data, session.InstanceData{Title: title, Path: repoPath, Status: session.Loading})
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, raw))
}

// countRecordsWithTitle counts the persisted rows carrying exactly `title` in
// repoID's instances.json. recordFor returns only the first match, so it cannot
// see a duplicate; this is what the #1951 corruption assertion needs.
func countRecordsWithTitle(t *testing.T, repoID, title string) int {
	t.Helper()
	data, err := loadRepoInstanceData(repoID)
	require.NoError(t, err)
	n := 0
	for i := range data {
		if data[i].Title == title {
			n++
		}
	}
	return n
}

// TestReserveCreate_ReusesArchivedName_OverwritesLoadingGhost is the #1951
// regression test. A legacy TUI binary (#551) could persist a Loading-status
// ghost record to disk; the rest of the codebase treats such ghosts as
// overwritable, never as real title reservations — appendInstanceData overwrites
// a same-titled Loading ghost, and findTitleConflictLocked skips Loading rows
// when deciding a title is free. renameInstanceDataTitle (the reuse-archived-name
// rewrite from #1719) copied the stable-id collision guard but NOT that ghost
// handling, so when its rename lands on a title a Loading ghost holds it wrote a
// SECOND record beside the ghost instead of replacing it — two rows sharing one
// title, i.e. instances.json corruption and ambiguous lookups.
//
// Repro on the real production path (reserveCreate -> renameArchivedForReuseLocked
// -> renameInstanceDataTitle): archive "foo", plant a Loading ghost on the exact
// slot the rename will choose ("foo (archived)" — the availability check skips the
// ghost, so uniqueArchivedTitleLocked picks it rather than "(archived 2)"), then
// create a new "foo". Exactly ONE record must carry "foo (archived)" afterward,
// and it must be the real archived session (stable id + branch preserved), proving
// the ghost was overwritten rather than the archived record dropped.
func TestReserveCreate_ReusesArchivedName_OverwritesLoadingGhost(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	branch := archived.GetBranch()

	// A legacy Loading ghost squatting on the exact title the rename will pick.
	seedLoadingGhost(t, repoID, "foo (archived)", repoPath)

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	require.NoError(t, err)
	defer release()

	assert.Equal(t, "foo", title)
	require.NotNil(t, renamed, "the collision must have renamed the archived session")
	assert.Equal(t, "foo (archived)", renamed.Title, "the rename must land on the ghost-held slot, not skip past it")

	// The corruption assertion: the ghost must be REPLACED, leaving exactly one
	// record under the reused title — not two. (Pre-fix: two.)
	assert.Equal(t, 1, countRecordsWithTitle(t, repoID, "foo (archived)"),
		"exactly one record must carry the reused title; a surviving Loading ghost beside the renamed record is the #1951 corruption")

	// The surviving record must be the real archived session, not the ghost —
	// stable id and branch preserved, status Archived.
	rec := recordFor(t, repoID, "foo (archived)")
	require.NotNil(t, rec, "the archived record must be persisted under the reused name")
	assert.Equal(t, id, rec.ID, "the surviving record must be the archived session (its stable id), not the empty-id ghost")
	assert.Equal(t, branch, rec.Branch, "the git branch must be preserved across the rename")
	assert.Equal(t, session.Archived, rec.Status)
}

// TestReserveCreate_ReusesArchivedName_SuffixCollision: with BOTH archived "foo"
// and archived "foo (archived)" already present, creating a new "foo" renames the
// old archived "foo" to the next free slot, "foo (archived 2)".
func TestReserveCreate_ReusesArchivedName_SuffixCollision(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo (archived)", "foo-archived")

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
