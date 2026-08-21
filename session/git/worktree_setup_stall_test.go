package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3424: session CREATION hangs forever on a stalled `git worktree remove -f`.
//
// The setup paths run the same speculative cleanup teardown does — remove the
// worktree registered at our path, prune stale registrations, drop a leftover
// branch of our own name — and every one of those calls went through the
// unbounded runGitCommand while the identical teardown calls have been bounded
// by localGitTimeout since #1917. So a create over a path on a hung network
// mount spun with no deadline, no cancellation and no recovery but killing the
// daemon.
//
// These tests stall git for real (a `git` earlier on PATH that never exits) and
// require three things of every setup path: it RETURNS inside the bound, the
// error names the path and the command that stalled, and it stops before
// creating or destroying anything.

// realGitPath resolves git BEFORE any test shadows PATH, so assertions can run
// the real binary directly instead of reasoning about which fake a given
// argument list happens to match.
func realGitPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	require.NoError(t, err)
	return path
}

// gitOut runs the real git and returns trimmed stdout, failing on error.
func gitOut(t *testing.T, realGit, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(realGit, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

// stallGitSubcommand puts a `git` earlier on PATH that STALLS only when both
// words appear in its arguments and delegates everything else to the real git.
//
// Selectivity is what makes a setup-path test possible at all: Setup() has to
// reach the cleanup under test through real `show-ref`, `rev-parse` and
// `worktree add` calls, so the blunt stallingGitOnPath used by the teardown
// wedge tests would hang on the very first probe and prove nothing about this
// path.
//
// The stall sleeps in a CHILD and waits, exactly as stallingGitOnPath does: that
// is what makes the process-group kill and gitWaitDelay load-bearing rather than
// decorative — killing only the direct git leaves the child holding the
// inherited capture pipe and Output() blocks on pipe EOF until it dies. The
// returned path is a file the fake writes that child's pid into, so a test can
// assert the child was killed rather than orphaned.
func stallGitSubcommand(t *testing.T, word1, word2 string) (pidFile string) {
	t.Helper()
	realGit := realGitPath(t)
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "stalled-child.pid")
	script := fmt.Sprintf(`#!/bin/sh
m1=0
m2=0
for a in "$@"; do
  if [ "$a" = %q ]; then m1=1; fi
  if [ "$a" = %q ]; then m2=1; fi
done
if [ "$m1" = 1 ] && [ "$m2" = 1 ]; then
  sleep 300 &
  echo $! > %q
  wait
  exit 0
fi
exec %q "$@"
`, word1, word2, pidFile, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile
}

// newStallFixture builds a repo with one commit plus an unused worktree path,
// and returns the GitWorktree a create would use for it. The GitWorktree comes
// from the real constructor so hooksCtx and worktreeDir are populated the way
// production has them.
func newStallFixture(t *testing.T, branch string) (gw *GitWorktree, repoRoot, worktreePath, realGit string) {
	t.Helper()
	sandboxHome(t)
	realGit = realGitPath(t)
	repoRoot = createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	worktreePath = filepath.Join(t.TempDir(), "wt")

	gw, err := NewGitWorktreeFromStorage(repoRoot, worktreePath, "stall-3424", branch, "", false, false)
	require.NoError(t, err)
	return gw, repoRoot, worktreePath, realGit
}

// requireBoundedStallError asserts the error is the #3424 bounded failure and
// that it is ACTIONABLE: a user staring at a spinner has to learn which path is
// stuck and what stalled on it, not read back a bare context.DeadlineExceeded.
func requireBoundedStallError(t *testing.T, err error, worktreePath, command string) {
	t.Helper()
	require.Error(t, err, "a stalled git must fail the setup, not be discarded as it was before #3424")
	assert.True(t, errors.Is(err, ErrWorktreeSetupStalled),
		"the stall needs its own sentinel so callers can tell it from git answering "+
			"'nothing to remove': %v", err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the tripped deadline must stay inspectable through the wrapper: %v", err)
	assert.Contains(t, err.Error(), worktreePath,
		"the error must name the worktree path that stalled")
	assert.Contains(t, err.Error(), command,
		"the error must name the git command that stalled")
	assert.Contains(t, err.Error(), "stalled filesystem",
		"the error must say WHY it stalled and what to do, not just that a context expired")
	assert.NotEqual(t, context.DeadlineExceeded.Error(), err.Error(),
		"a bare context error is not an actionable message")
}

// runBounded runs fn on its own goroutine and fails the test if it does not
// return within limit. Calling fn inline would hang the whole package on a
// regression instead of reporting this test as the failure.
func runBounded(t *testing.T, limit time.Duration, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("%s hung on a stalled git (#3424): session creation has no other deadline — "+
			"WaitForReady's clock only starts once launch returns — so the client spins forever "+
			"and the only recovery is killing the daemon", what)
		return nil
	}
}

// worktreeRegistered reports whether git still lists path as a worktree of the
// repo, read with the real git so a shadowed PATH cannot affect the answer.
func worktreeRegistered(t *testing.T, realGit, repoRoot, path string) bool {
	t.Helper()
	return worktreeListed(gitOut(t, realGit, repoRoot, "worktree", "list", "--porcelain"), path)
}

func branchExists(t *testing.T, realGit, repoRoot, branch string) bool {
	t.Helper()
	cmd := exec.Command(realGit, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), gitEnv...)
	return cmd.Run() == nil
}

// TestSetup_StalledWorktreeRemoveFailsBoundedInsteadOfHanging is the core #3424
// regression on the new-session path: `git worktree remove -f` makes no
// progress, and Setup must come back with an actionable error inside the bound.
//
// It also pins the half-made-workspace half of the contract. Before this fix the
// remove's error was DISCARDED, so bounding it alone would have changed nothing:
// setup would have walked straight into the unbounded `worktree add` on the same
// stalled path and hung one line lower down. Asserting that nothing was
// registered and no branch was created is what proves the abort, not just the
// deadline.
func TestSetup_StalledWorktreeRemoveFailsBoundedInsteadOfHanging(t *testing.T) {
	gw, repoRoot, worktreePath, realGit := newStallFixture(t, "af-3424-new")
	stallGitSubcommand(t, "remove", "-f")
	shortenLocalTimeout(t, 200*time.Millisecond)

	start := time.Now()
	err := runBounded(t, 30*time.Second, "Setup", gw.Setup)
	requireBoundedStallError(t, err, worktreePath, "worktree remove -f")
	assert.Less(t, time.Since(start), 20*time.Second,
		"git must be killed at its deadline, not waited on until the fake git exits")

	assert.False(t, worktreeRegistered(t, realGit, repoRoot, worktreePath),
		"setup must abort before `worktree add`: a registration created over a path "+
			"whose state af could not establish is exactly the half-made workspace #3233-#3237 forbid")
	assert.False(t, branchExists(t, realGit, repoRoot, "af-3424-new"),
		"no branch may be created for a session that never got a workspace")
	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr),
		"setup must not leave a checkout behind at %s", worktreePath)
}

// TestSetup_StalledPruneAlsoAbortsSetup covers the second cited call site. The
// remove answers normally here (there is no worktree to remove) and the PRUNE is
// what stalls, so this fails if only the remove was bounded.
func TestSetup_StalledPruneAlsoAbortsSetup(t *testing.T) {
	gw, repoRoot, worktreePath, realGit := newStallFixture(t, "af-3424-prune")
	stallGitSubcommand(t, "worktree", "prune")
	shortenLocalTimeout(t, 200*time.Millisecond)

	err := runBounded(t, 30*time.Second, "Setup", gw.Setup)
	requireBoundedStallError(t, err, worktreePath, "worktree prune")
	assert.False(t, worktreeRegistered(t, realGit, repoRoot, worktreePath),
		"a stalled prune must stop setup before `worktree add`")
	assert.False(t, branchExists(t, realGit, repoRoot, "af-3424-prune"))
}

// TestSetupNewWorktree_StalledCleanupDoesNotDeleteTheLeftoverBranch is the
// #1917-round-8 hazard on the setup path.
//
// setupNewWorktree deletes a leftover branch of its own name before recreating
// it. When the cleanup that precedes that delete cannot establish what is at the
// path, the branch may still be the only ref to a checkout that is being
// retained — and `branch -D` succeeds precisely BECAUSE a timed-out remove
// deregisters before it stalls. Destroying the only pointer to files nothing
// else can find is worse than either failure alone, so the delete must not run.
func TestSetupNewWorktree_StalledCleanupDoesNotDeleteTheLeftoverBranch(t *testing.T) {
	gw, repoRoot, worktreePath, realGit := newStallFixture(t, "af-3424-leftover")
	// A leftover branch carrying work nothing else references.
	runGit(t, repoRoot, "branch", "af-3424-leftover")
	unique := gitOut(t, realGit, repoRoot, "rev-parse", "af-3424-leftover")

	stallGitSubcommand(t, "remove", "-f")
	shortenLocalTimeout(t, 200*time.Millisecond)

	err := runBounded(t, 30*time.Second, "setupNewWorktree", gw.setupNewWorktree)
	requireBoundedStallError(t, err, worktreePath, "worktree remove -f")

	require.True(t, branchExists(t, realGit, repoRoot, "af-3424-leftover"),
		"the branch delete must not run once the cleanup's outcome is unknown: it would "+
			"destroy the only ref to a workspace the same run just declined to remove")
	assert.Equal(t, unique, gitOut(t, realGit, repoRoot, "rev-parse", "af-3424-leftover"),
		"the leftover branch must still point at its own commit")
}

// TestSetup_StalledRemoveOnExistingBranchKeepsTheBranch covers the
// setupFromExistingBranch route (Setup picks it because the branch exists). The
// branch is the user's here — branchCreatedByUs is false on this path — so the
// only acceptable outcome is a bounded failure that touches nothing.
func TestSetup_StalledRemoveOnExistingBranchKeepsTheBranch(t *testing.T) {
	gw, repoRoot, worktreePath, realGit := newStallFixture(t, "user/existing-work")
	runGit(t, repoRoot, "branch", "user/existing-work")
	head := gitOut(t, realGit, repoRoot, "rev-parse", "user/existing-work")

	stallGitSubcommand(t, "remove", "-f")
	shortenLocalTimeout(t, 200*time.Millisecond)

	err := runBounded(t, 30*time.Second, "Setup", gw.Setup)
	requireBoundedStallError(t, err, worktreePath, "worktree remove -f")

	require.True(t, branchExists(t, realGit, repoRoot, "user/existing-work"),
		"a reused branch is the user's work and must survive a failed create")
	assert.Equal(t, head, gitOut(t, realGit, repoRoot, "rev-parse", "user/existing-work"))
	assert.False(t, worktreeRegistered(t, realGit, repoRoot, worktreePath))
}

// TestRebuildFreshFromRecordedBase_StalledRemoveFailsBounded covers the recovery
// call site (worktree_ops.go:120 in the report). It runs inside the daemon's
// restore loop, so an unbounded stall there wedges recovery for every session
// behind it.
func TestRebuildFreshFromRecordedBase_StalledRemoveFailsBounded(t *testing.T) {
	gw, repoRoot, worktreePath, realGit := newStallFixture(t, "af-3424-rebuild")
	stallGitSubcommand(t, "remove", "-f")
	shortenLocalTimeout(t, 200*time.Millisecond)

	err := runBounded(t, 30*time.Second, "RebuildFreshFromRecordedBase", gw.RebuildFreshFromRecordedBase)
	requireBoundedStallError(t, err, worktreePath, "worktree remove -f")
	assert.False(t, worktreeRegistered(t, realGit, repoRoot, worktreePath))
	assert.False(t, branchExists(t, realGit, repoRoot, "af-3424-rebuild"),
		"a rebuild that could not clear the path must not create the branch either")
}

// TestSetup_StalledBranchDeleteAlsoAbortsSetup covers the third bounded command.
// The leftover-branch delete is destructive metadata that teardown has bounded
// and gated since #1917 while setup ran it unbounded, so it is the same class as
// the remove and the prune — and leaving it out would have put the hang back one
// line below the two the report named.
func TestSetup_StalledBranchDeleteAlsoAbortsSetup(t *testing.T) {
	gw, repoRoot, worktreePath, realGit := newStallFixture(t, "af-3424-branchdel")
	// Both cleanup commands answer here (there is no worktree at the path); only
	// the branch delete stalls.
	stallGitSubcommand(t, "branch", "-D")
	shortenLocalTimeout(t, 200*time.Millisecond)

	err := runBounded(t, 30*time.Second, "Setup", gw.Setup)
	requireBoundedStallError(t, err, worktreePath, "branch -D")
	assert.False(t, worktreeRegistered(t, realGit, repoRoot, worktreePath),
		"a stalled branch delete must stop setup before `worktree add -b`, which would "+
			"fail on the branch af could not delete anyway")
	assert.NotContains(t, err.Error(), "no branch was deleted",
		"a SIGKILLed `branch -D` cannot prove what it did or did not delete; the message "+
			"must not claim an outcome nobody established (#3233-#3237)")
	assert.Contains(t, err.Error(), "unknown",
		"the message must say the killed step's own outcome is unknown")
}

// TestSetup_StalledCleanupKillsTheGitChild is the cancellation half: the
// deadline must tear the stalled command DOWN, not abandon it. The fake git
// sleeps in a child, so this fails if the deadline killed only the direct git
// process — the surviving child would keep holding the inherited capture pipe
// (and, in production, keep hammering the stalled mount) for its full lifetime.
func TestSetup_StalledCleanupKillsTheGitChild(t *testing.T) {
	gw, _, worktreePath, _ := newStallFixture(t, "af-3424-orphan")
	pidFile := stallGitSubcommand(t, "remove", "-f")
	shortenLocalTimeout(t, 200*time.Millisecond)

	err := runBounded(t, 30*time.Second, "Setup", gw.Setup)
	requireBoundedStallError(t, err, worktreePath, "worktree remove -f")

	raw, readErr := os.ReadFile(pidFile)
	require.NoError(t, readErr, "the fake git must have recorded the child it spawned")
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, convErr)

	// The kill is a signal, so the exit is asynchronous; poll rather than
	// assert once. ESRCH means the process is gone AND reaped.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stalled git's child (pid %d) outlived the setup that spawned it: the "+
				"deadline must SIGKILL the whole process group, or a cancelled create leaves a "+
				"process holding the capture pipe and the stalled path", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSetup_OrdinaryCleanupFailureIsStillIgnored is the other half of the
// classification, and the one that would break every create if it regressed.
//
// `git worktree remove -f` against a path with no worktree FAILS, and that is
// the common case — the whole reason these calls discarded their errors. Only a
// tripped deadline may abort; an ordinary non-zero exit must stay ignored, so a
// perfectly normal create still succeeds.
func TestSetup_OrdinaryCleanupFailureIsStillIgnored(t *testing.T) {
	gw, repoRoot, worktreePath, realGit := newStallFixture(t, "af-3424-normal")

	// A git that FAILS FAST on the remove (as real git does when there is
	// nothing at the path) and is real for everything else.
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  if [ "$a" = "remove" ]; then
    echo "fatal: '$2' is not a working tree" >&2
    exit 128
  fi
done
exec %q "$@"
`, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shortenLocalTimeout(t, 10*time.Second)

	require.NoError(t, runBounded(t, 30*time.Second, "Setup", gw.Setup),
		"a failing-but-answering cleanup command must not fail the create")
	assert.True(t, worktreeRegistered(t, realGit, repoRoot, worktreePath),
		"the ordinary create must still add the worktree")
	assert.True(t, branchExists(t, realGit, repoRoot, "af-3424-normal"))
}
