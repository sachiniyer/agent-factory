package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1917: a session kill that wedges forever, leaving the session permanently
// undeletable until the daemon restarts.
//
// GitWorktree.Cleanup runs on the kill teardown (session/teardown.go) inside the
// daemon's per-session kills-in-flight guard, and every git command it ran went
// through runGitCommand — i.e. context.Background(), no deadline — on the theory
// that local git "cannot stall the way a fetch can". `git worktree remove -f`
// disproves that: it recursively unlinks a whole checkout and blocks indefinitely
// on a hung mount or a D-state process holding a file in the tree.

// shortenLocalTimeout lowers localGitTimeout so the wedge tests resolve in
// milliseconds instead of the production 60s, restoring it after.
func shortenLocalTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := localGitTimeout
	localGitTimeout = d
	t.Cleanup(func() { localGitTimeout = orig })
}

// stallingGitOnPath puts a `git` earlier on PATH that never exits, standing in
// for a `worktree remove` wedged on a stalled filesystem. Mirrors
// stallingTmuxOnPath and stallOriginViaFakeSSH: the script sleeps in a CHILD, so
// it also covers the case that makes a naive deadline useless — killing only the
// direct git process leaves the child holding the inherited capture pipe, and
// Output() blocks on pipe EOF until it dies. Passing therefore requires the
// process-group kill AND gitWaitDelay, not just exec.CommandContext.
//
// Call this AFTER building the repo fixture: everything the fixture does runs
// real git through this same PATH.
func stallingGitOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 300 &\nwait\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCleanup_DoesNotWedgeOnStalledGit is the core #1917 regression: with git
// making no progress, Cleanup must return a real, actionable, joined error
// within the bound instead of hanging the kill forever.
func TestCleanup_DoesNotWedgeOnStalledGit(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	// The worktree directory must exist for Cleanup to attempt removal at all
	// (it stats the path first).
	worktreePath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))

	gw := &GitWorktree{
		repoPath:          repoRoot,
		worktreePath:      worktreePath,
		branchName:        "af-wedge-1917",
		branchCreatedByUs: true,
	}

	stallingGitOnPath(t)
	shortenLocalTimeout(t, 200*time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- gw.Cleanup() }()

	select {
	case err := <-done:
		require.Error(t, err, "a stalled git must surface a timeout error, not silent success")
		assert.Contains(t, err.Error(), "timed out",
			"the timeout must be actionable: it names the command and why it was killed")
		assert.Less(t, time.Since(start), 30*time.Second,
			"each git step should be killed at its deadline, not wait for the fake git to exit")
	case <-time.After(60 * time.Second):
		t.Fatal("Cleanup hung on a stalled git (#1917): the daemon holds its kills-in-flight " +
			"guard across this call, so the session would be undeletable until the daemon restarts")
	}
}

// TestCleanup_StalledRemoveDoesNotDeleteTheDirectory guards the #1917 timeout
// guard in shouldRemoveWorktreeDir, and it is the reason bounding alone is not
// enough. On a timeout `git worktree remove -f` never FINISHED — it was SIGKILLed
// mid-delete — so its registration state is a half-done snapshot, not a verdict.
// Reading it as the #802 "git let go of the worktree" case sends Cleanup into
// os.RemoveAll, which takes no context and would stall on the very same paths
// that stalled git: the bound would buy nothing and the hang would come back one
// line later, now unkillable.
//
// Here the fake git makes `worktree list` stall too, so listErr != nil and the
// pre-#1917 tree would fall through to the "validation failed" string gate and
// keep the directory. That makes the DURABLE assertion the one that bites: the
// error must be reported, and it must be the timeout — never a claim that the
// directory was dealt with.
func TestCleanup_StalledRemoveDoesNotDeleteTheDirectory(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))
	canary := filepath.Join(worktreePath, "uncommitted-work.txt")
	require.NoError(t, os.WriteFile(canary, []byte("user work"), 0o644))

	gw := &GitWorktree{
		repoPath:          repoRoot,
		worktreePath:      worktreePath,
		branchName:        "af-wedge-1917",
		branchCreatedByUs: true,
	}

	stallingGitOnPath(t)
	shortenLocalTimeout(t, 200*time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- gw.Cleanup() }()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		// Cleanup must not have quietly swallowed the stall as a successful
		// removal: the tree it could not confirm removed is still there for a
		// later kill to retry.
		assert.FileExists(t, canary,
			"a timed-out `worktree remove` must not lead to deleting the directory: "+
				"git may still have been mid-delete, and os.RemoveAll would stall on the same paths")
	case <-time.After(60 * time.Second):
		t.Fatal("Cleanup hung on a stalled git (#1917)")
	}
}

// TestCleanup_HealthyWorktreeStillSucceeds guards the other direction: the
// deadline must not break the normal teardown. A real worktree, removed by real
// git, must still leave no directory, no branch, and no error — so a regression
// that always trips the timeout (or that mis-reads a fast exit as a deadline)
// fails here rather than silently making every kill best-effort.
func TestCleanup_HealthyWorktreeStillSucceeds(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repoRoot, "worktree", "add", "-b", "af-healthy-1917", worktreePath)
	require.DirExists(t, worktreePath)

	gw := &GitWorktree{
		repoPath:          repoRoot,
		worktreePath:      worktreePath,
		branchName:        "af-healthy-1917",
		branchCreatedByUs: true,
	}

	// Long enough that real git never trips it, short enough to keep the test fast.
	shortenLocalTimeout(t, 30*time.Second)

	require.NoError(t, gw.Cleanup(), "a healthy teardown must not be affected by the bound")
	assert.NoDirExists(t, worktreePath, "the worktree directory must still be removed")

	out, err := (&GitWorktree{}).runGitCommand(repoRoot, "branch", "--list", "af-healthy-1917")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out), "the branch we created must still be deleted")
}
