package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The factory reset's branch-deletion path, bounded.
//
// The asymmetry this closes is the whole report: the reset's worktree removal
// (RemoveWorktreeDir) has run its git operations under localGitTimeout since
// #3096, which documented the same hazard this file's worktree half already
// fixed — `git worktree remove -f` blocks indefinitely on a hung mount, and so
// does `git branch -D` on the same mount. DeleteLocalBranch is the reset's
// SOLE branch-deletion path (#1736), and it ran `git branch -D` and its
// `show-ref`/`rev-parse` probes as raw exec.Command with no context, no
// timeout, no process group, and no supervision. The codebase's own hardening
// proved the stall is real; the branch-deletion path stayed exposed to it.
//
// These mirror the #3096 wedge tests deliberately, because the property is the
// same one: on a destructive path, RETURNING AND REPORTING is the guarantee,
// not succeeding. Ctrl-C is not an escape when it leaves a reset half-applied.

// THE CORE REGRESSION. Unbounded, this call never returns and `af reset` wedges
// with no completion time and no message. A stalled `git branch -D` must
// surface a bounded error, never hang.
func TestDeleteLocalBranchDoesNotWedgeOnStalledGit(t *testing.T) {
	root := branchRepo(t, "af-stalled")

	stallingGitFor(t, "branch")
	shortenLocalTimeout(t, 200*time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := DeleteLocalBranch(root, "af-stalled"); done <- err }()

	select {
	case err := <-done:
		require.Error(t, err, "a stalled deletion must surface an error, never silent success")
		assert.Less(t, time.Since(start), 30*time.Second,
			"git must be killed at its deadline, not waited on until the stall clears")

		// A timeout leaves the deletion UNKNOWN, so the caller must be able to
		// classify it and retain the session record (commands/reset.go keys
		// unverifiedBranches on any error from DeleteLocalBranch).
		assert.True(t, errors.Is(err, ErrBranchDeletionTimedOut),
			"a stall must classify as a branch-deletion timeout so the message says 'stalled', got %v", err)

		// The branch survives. A stall is not a deletion: git may not have
		// reached the ref write, and the whole point of bounding is that af
		// reports what it could not do rather than guessing it done.
		assert.True(t, refExists(t, root, "af-stalled"),
			"a timeout is not a deletion — the branch must still exist so a re-run can revisit it")
	case <-time.After(60 * time.Second):
		t.Fatal("DeleteLocalBranch HUNG on a stalled `git branch -D`: `af reset` becomes unresponsive with no " +
			"bounded completion time, and Ctrl-C leaves the reset half-applied")
	}
}

// THE PROBE REGRESSION. `git show-ref --verify --quiet` is the existence gate
// for the destructive `git branch -D` that follows. An unbounded stall here
// wedges `af reset` before the delete even runs; a bounded one must surface a
// timeout error, never hang, and never fabricate absence — a failed probe is
// UNKNOWN, not "branch doesn't exist" (#3243).
func TestDeleteLocalBranchStalledShowRefDoesNotWedgeOrFabricateAbsence(t *testing.T) {
	root := branchRepo(t, "af-probe-stalled")

	stallingGitFor(t, "show-ref")
	shortenLocalTimeout(t, 200*time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := DeleteLocalBranch(root, "af-probe-stalled"); done <- err }()

	select {
	case err := <-done:
		require.Error(t, err, "a stalled existence probe must surface an error, never silent success")
		assert.Less(t, time.Since(start), 30*time.Second,
			"the stalled probe must be killed at its deadline, not waited on")
		assert.True(t, errors.Is(err, ErrBranchDeletionTimedOut),
			"a stalled show-ref must classify as a branch-deletion timeout, got %v", err)

		// The branch survives — a stalled probe is not a deletion, and it is not
		// absence either: the caller must retain the record so a re-run can
		// revisit the branch (#3243). Check the loose ref file DIRECTLY rather
		// than via `git show-ref`: the stalling git is still on PATH and would
		// match "show-ref", hanging the verification probe.
		_, statErr := os.Stat(filepath.Join(root, ".git", "refs", "heads", "af-probe-stalled"))
		assert.NoError(t, statErr,
			"a stalled probe must not fabricate absence — the branch's loose ref file still exists")
	case <-time.After(60 * time.Second):
		t.Fatal("DeleteLocalBranch HUNG on a stalled `git show-ref`: the existence probe for the " +
			"destructive `git branch -D` wedges `af reset` with no completion time")
	}
}

// THE REV-PARSE REGRESSION. `git rev-parse --git-common-dir` runs on the
// absence-confirmation path (when show-ref exits 1 with empty stderr). A stall
// here must surface a bounded error, never hang, and never fabricate absence —
// the loose-ref confirmation must not be skipped by a timeout.
func TestDeleteLocalBranchStalledRevParseDoesNotWedgeOrFabricateAbsence(t *testing.T) {
	root := branchRepo(t, "af-real")

	stallingGitFor(t, "rev-parse")
	shortenLocalTimeout(t, 200*time.Millisecond)

	// Probe a NON-EXISTENT branch so show-ref exits 1 with empty stderr
	// (determinate-absence candidate), triggering the rev-parse confirmation
	// path — which stalls. A stall there must not fabricate absence.
	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := DeleteLocalBranch(root, "af-nonexistent"); done <- err }()

	select {
	case err := <-done:
		require.Error(t, err, "a stalled rev-parse must surface an error, never fabricate absence")
		assert.Less(t, time.Since(start), 30*time.Second,
			"the stalled rev-parse must be killed at its deadline, not waited on")
		assert.True(t, errors.Is(err, ErrBranchDeletionTimedOut),
			"a stalled rev-parse must classify as a branch-deletion timeout, got %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("DeleteLocalBranch HUNG on a stalled `git rev-parse --git-common-dir`: the " +
			"absence-confirmation probe wedges `af reset` with no completion time")
	}
}

// The bound must not change the ordinary path: a healthy repo still deletes the
// branch, so this fix costs nothing when nothing is stalled.
func TestDeleteLocalBranchStillDeletesWhenGitIsHealthy(t *testing.T) {
	root := branchRepo(t, "af-healthy")

	deleted, err := DeleteLocalBranch(root, "af-healthy")
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.False(t, refExists(t, root, "af-healthy"),
		"the branch must be gone on a healthy repo — bounding must not break the happy path")
}

// A KILLABLE stall must produce the ordinary timeout error, reaped and
// diagnosed, never the abandon message. The abandon grace exceeds gitWaitDelay
// precisely so this ordering holds (same rule as the worktree runner).
func TestKillableBranchStallTakesTheOrdinaryTimeoutPath(t *testing.T) {
	root := branchRepo(t, "af-killable")

	stallingGitFor(t, "branch")
	shortenLocalTimeout(t, 200*time.Millisecond)

	_, err := DeleteLocalBranch(root, "af-killable")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "made no progress",
		"a KILLABLE stall must report the ordinary deadline, which reaps and diagnoses properly")
	assert.NotContains(t, err.Error(), "could not be killed",
		"the abandon path is for a child that cannot be reaped, and must not claim that of one that can")
}

// …and that runBoundedBranchGit actually USES the supervision. Separate from
// the test above on purpose: a killable child always takes the ordinary deadline
// path, so the abandon branch is unreachable with a real process — and a test
// of awaitCommand alone passes just as well when nothing calls it. That gap was
// real; removing the call site left the direct test green.
func TestRunBoundedBranchGitReportsAnUnreapableChild(t *testing.T) {
	prev := awaitCommandFn
	awaitCommandFn = func(*exec.Cmd, bool, time.Duration) ([]byte, error, bool) {
		return nil, nil, false // the child could not be reaped
	}
	t.Cleanup(func() { awaitCommandFn = prev })

	_, err := runBoundedBranchGit(t.TempDir(), []string{"GIT_TERMINAL_PROMPT=0"}, true, "branch", "-D", "af-test")

	require.Error(t, err, "an unreapable child must be REPORTED, never waited on")
	assert.True(t, errors.Is(err, ErrBranchDeletionTimedOut),
		"and must classify as a stall, so the caller retains the record, got %v", err)
	assert.Contains(t, err.Error(), "could not be killed",
		"the message must say the process is still running, not imply a clean failure")
}
