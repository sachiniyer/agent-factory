package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestReserveCreate_ReuseArchivedNameRefusesWhileAnOperationIsInFlight is the
// deterministic half of the #2779 guard.
//
// killsInFlight is the daemon's per-session exclusive-operation fence: restore,
// kill, archive and the root-kill path all claim it under m.mu before touching a
// session. The archived-name-reuse rename never consulted it, so a create could
// relocate an archived session's worktree, rewrite its durable record and re-key
// it while an exclusive operation on that very session was already running.
//
// The fence is claimed exactly as claimRestoreOperation claims it, which is what
// makes this a test of the production predicate rather than of a mock: m.mu is
// the only thing serializing the two, and reserveCreate holds it unbroken across
// the whole rename.
func TestReserveCreate_ReuseArchivedNameRefusesWhileAnOperationIsInFlight(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	archivedPath := archived.GetWorktreePath()
	branch := archived.GetBranch()

	key := daemonInstanceKey(repoID, "foo")
	manager.mu.Lock()
	manager.killsInFlight[key] = struct{}{}
	manager.mu.Unlock()
	t.Cleanup(func() {
		manager.mu.Lock()
		delete(manager.killsInFlight, key)
		manager.mu.Unlock()
	})

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		defer release()
	}

	require.Error(t, err, "the create must be refused while an exclusive operation holds the archived session")
	assert.Contains(t, err.Error(), "in progress",
		"the refusal must say what is blocking it, so retrying is an obvious next step")
	assert.Nil(t, renamed, "no rename may happen around a session another operation owns")

	// Nothing moved: the whole point of refusing BEFORE the rename is that a
	// refusal leaves no half-applied state behind.
	assert.Equal(t, "foo", archived.Title)
	assert.Equal(t, archivedPath, archived.GetWorktreePath())
	assert.True(t, exists(archivedPath), "the archived worktree must stay where the in-flight operation expects it")
	assert.Equal(t, branch, archived.GetBranch())

	manager.mu.Lock()
	_, stillKeyed := manager.instances[key]
	manager.mu.Unlock()
	assert.True(t, stillKeyed, "the archived row must still be addressable under its own title")

	rec := recordFor(t, repoID, "foo")
	require.NotNil(t, rec, "the durable record must survive under its own title")
	assert.Equal(t, id, rec.ID)
	assert.Equal(t, archivedPath, rec.Worktree.WorktreePath)
}

// blockRestoreRelocateGitOnPath installs a `git` shim that parks the restore's
// worktree relocation mid-flight: when git is asked to move a worktree TO
// restoreDest, it announces that it got there and waits for the returned
// proceed() before running the real move.
//
// It blocks BEFORE exec'ing git on purpose. The point is to hold the caller
// inside relocateWorktreeTo — past its source read and its existence guards —
// without holding any git lock, so the concurrent operation under test is free
// to run against the same worktree exactly as it would in production. Every
// other git invocation, including the rename's own `worktree move` to a
// different destination, passes straight through to the real binary.
func blockRestoreRelocateGitOnPath(t *testing.T, restoreDest string) (reached func() bool, proceed func()) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)

	dir := t.TempDir()
	reachedPath := filepath.Join(dir, "reached")
	proceedPath := filepath.Join(dir, "proceed")
	shim := filepath.Join(dir, "git")
	// Args always arrive as: git -C <path> <verb> <subverb> <src> <dest>.
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = \"worktree\" ] && [ \"$4\" = \"move\" ] && [ \"$6\" = " + shellQuoteForShim(restoreDest) + " ]; then\n" +
		"  : > " + shellQuoteForShim(reachedPath) + "\n" +
		"  i=0\n" +
		"  while [ ! -f " + shellQuoteForShim(proceedPath) + " ] && [ $i -lt 600 ]; do\n" +
		"    sleep 0.05\n" +
		"    i=$((i+1))\n" +
		"  done\n" +
		"fi\n" +
		"exec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() bool {
			_, statErr := os.Stat(reachedPath)
			return statErr == nil
		}, func() {
			require.NoError(t, os.WriteFile(proceedPath, nil, 0o644))
		}
}

func shellQuoteForShim(s string) string {
	return "'" + s + "'"
}

// requireEventually polls until cond is true, failing the test if it never
// becomes true. The budget is generous because the thing being waited on is a
// git subprocess reaching a point in its own execution on a possibly-loaded
// runner, and it stays well inside the 60s bound the relocate itself runs under.
func requireEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRestoreArchived_ConcurrentTitleReuseCreateDoesNotHijackTheWorktree is the
// #2779 reproduction, driven through both real production paths at once.
//
// A restore and a title-reuse create both relocate the SAME archived worktree,
// and nothing serialized them. RestoreArchived claims killsInFlight and the
// per-session op lock, then drops m.mu before moving the worktree — while
// reserveCreate holds m.mu across a rename that moves that same worktree
// somewhere else. GitWorktree has no internal synchronization, so the two land
// in relocateWorktreeTo together: one reads the source path the other is
// concurrently rewriting, and the filesystem move interleaves with the other's
// existence checks.
//
// The overlap here is real rather than simulated: the restore is parked INSIDE
// its relocation (the git shim reports it arrived) and the create runs to
// completion against the same worktree before the restore is let go.
//
// Pre-fix this test recorded the corruption: the create succeeded, so the
// session the user asked to restore was renamed to "foo (archived)" and its
// worktree moved out from under the restore, which then failed with a move error
// against a source that no longer existed. The in-flight operation lost to a
// create that started later.
func TestRestoreArchived_ConcurrentTitleReuseCreateDoesNotHijackTheWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	branch := archived.GetBranch()
	archivePath := archived.GetWorktreePath()

	// The destination the restore will pick, resolved the same way restore does,
	// so the shim can recognize exactly that one move.
	restoreDest, err := sessiongit.RestoreWorktreePath(repoPath, "foo", branch)
	require.NoError(t, err)
	reached, proceed := blockRestoreRelocateGitOnPath(t, restoreDest)

	type restoreResult struct {
		path string
		err  error
	}
	restored := make(chan restoreResult, 1)
	go func() {
		path, _, rerr := manager.RestoreArchived(RestoreArchivedRequest{Title: "foo", RepoID: repoID})
		restored <- restoreResult{path: path, err: rerr}
	}()

	// The restore is now inside relocateWorktreeTo on this worktree: its source
	// read and guards are behind it, and the bytes are still at the archive path.
	requireEventually(t, "the restore to reach its worktree relocation", reached)
	require.True(t, exists(archivePath), "precondition: the restore has not moved the bytes yet")

	_, _, release, renamed, createErr := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		defer release()
	}
	proceed()
	result := <-restored

	require.Error(t, createErr, "a create must not reuse the name of a session whose restore is already in flight")
	assert.Contains(t, createErr.Error(), "in progress")
	assert.Nil(t, renamed, "no rename may happen around a restore that is mid-relocation")

	// The restore — the operation that claimed the session first — completes.
	require.NoError(t, result.err, "the in-flight restore must not be broken by a create that started after it")
	assert.Equal(t, restoreDest, result.path)
	dirty, readErr := os.ReadFile(filepath.Join(result.path, "dirty.txt"))
	require.NoError(t, readErr, "the restored worktree must carry the session's uncommitted work")
	assert.Equal(t, "uncommitted-foo", string(dirty))

	// Identity is intact: the session is still the one the user asked to restore.
	assert.Equal(t, "foo", archived.Title, "the restored session must keep the name it was restored under")
	assert.Equal(t, id, archived.ID)
	assert.Equal(t, branch, archived.GetBranch())
	assert.False(t, exists(archivePath), "the archive directory is vacated by the restore, not by a rename")

	rec := recordFor(t, repoID, "foo")
	require.NotNil(t, rec, "the durable record must still be keyed on the restored title")
	assert.Equal(t, id, rec.ID)
	assert.Equal(t, restoreDest, rec.Worktree.WorktreePath)

	renamedRec := recordFor(t, repoID, "foo (archived)")
	assert.Nil(t, renamedRec, fmt.Sprintf("no disambiguated archive row may exist: the refused create must have renamed nothing (got %+v)", renamedRec))
}
