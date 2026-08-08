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

// The factory reset's removal path, bounded (#3096).
//
// The asymmetry this closes is the whole report: Cleanup() has run these exact
// git operations under localGitTimeout since #1917 — which documented that
// `git worktree remove -f` genuinely stalls forever on a hung mount — while
// RemoveWorktreeDir, the reset's ONLY removal path, ran them unbounded. The
// codebase's own hardening proved the stall is real; the reset could not use it.
//
// These mirror the #1917 wedge tests deliberately, because the property is the
// same one: on a destructive path, RETURNING AND REPORTING is the guarantee, not
// succeeding. Ctrl-C is not an escape when it leaves a reset half-applied.

// stallingGitFor puts a `git` earlier on PATH that hangs on the given
// subcommand and delegates everything else to the real one. Targeting a SINGLE
// subcommand is what lets these tests separate the three call sites — a blanket
// stall cannot tell "the remove hung" from "the verification probe hung", and
// those two have different correct answers.
//
// The sleep runs in a CHILD, matching stallingGitOnPath: killing only the direct
// git leaves the child holding the inherited capture pipe, so passing requires
// the process-group kill AND the WaitDelay, not just exec.CommandContext.
func stallingGitFor(t *testing.T, subcommand string) {
	t.Helper()
	real, err := findRealGit()
	require.NoError(t, err)
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = \"" + subcommand + "\" ]; then sleep 300 & wait; fi\n" +
		"done\n" +
		"exec " + real + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func findRealGit() (string, error) {
	return exec.LookPath("git")
}

// THE CORE REGRESSION. Unbounded, this call never returns and `af reset` wedges
// with no completion time and no message.
func TestRemoveWorktreeDirDoesNotWedgeOnStalledGit(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))

	stallingGitFor(t, "remove")
	shortenLocalTimeout(t, 200*time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := RemoveWorktreeDir(repoRoot, worktreePath); done <- err }()

	select {
	case err := <-done:
		require.Error(t, err, "a stalled removal must surface an error, never silent success")
		assert.Less(t, time.Since(start), 30*time.Second,
			"git must be killed at its deadline, not waited on until the stall clears")

		// RETAIN, not drop. A timeout leaves the registration UNKNOWN, and dropping
		// the session record on an answer nobody got would orphan a possibly-registered
		// worktree and its branch with nothing left to plan a re-run from (#2531).
		assert.True(t, errors.Is(err, ErrWorktreeStillRegistered),
			"a stall must classify as still-registered so reset keeps the record, got %v", err)
		assert.True(t, errors.Is(err, ErrWorktreeRemovalTimedOut),
			"and must be distinguishable from a real lock, so the message does not blame one it never saw")
		assert.Contains(t, err.Error(), worktreePath, "the operator must be told WHICH worktree could not be removed")

		// The directory survives. A stall is not permission to delete: git may still
		// own the path, and the whole ownership rule exists to keep a locked or
		// undetermined worktree's directory in place.
		assert.DirExists(t, worktreePath,
			"a timeout is not an ownership verdict — deleting here is exactly the data loss the gate prevents")
	case <-time.After(60 * time.Second):
		t.Fatal("RemoveWorktreeDir HUNG on a stalled git (#3096): `af reset` becomes unresponsive with no " +
			"bounded completion time, and Ctrl-C leaves the reset half-applied")
	}
}

// THE SUBTLE ONE. `worktree list --porcelain` is the verification read, and a
// bound that let a stalled probe answer "not registered" would convert a hang
// into SILENT DATA LOSS: mayDeleteWorktreeDir would then treat git's own
// worktree as af's to delete. A failed read is not an empty result.
func TestStalledRegistrationProbeIsNotAnAbsence(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))

	stallingGitFor(t, "list")
	shortenLocalTimeout(t, 200*time.Millisecond)

	// Bounded by a select, so an UNBOUNDED probe fails RED instead of hanging the
	// suite until the whole run is killed. A test that hangs reports nothing, which
	// is the same failure this change is about one level up.
	type probe struct {
		registered bool
		err        error
	}
	got := make(chan probe, 1)
	go func() {
		reg, err := worktreeRegisteredIn(repoRoot, worktreePath)
		got <- probe{reg, err}
	}()
	var registered bool
	var err error
	select {
	case p := <-got:
		registered, err = p.registered, p.err
	case <-time.After(30 * time.Second):
		t.Fatal("worktreeRegisteredIn HUNG on a stalled `git worktree list` (#3096): this is the " +
			"verification read on the ERROR path, so an unbounded stall wedges a reset that has " +
			"already decided something went wrong")
	}

	require.Error(t, err, "a probe that could not be asked must ERROR, never answer")
	assert.False(t, registered,
		"the boolean is meaningless on error, which is why every caller checks err first")
	assert.True(t, errors.Is(err, ErrWorktreeRemovalTimedOut), "and the error must say it stalled, got %v", err)

	// The gate must refuse on that error rather than read it as "not ours".
	assert.False(t, mayDeleteWorktreeDir(registered, err == nil, errors.New("git worktree remove failed")),
		"a probe that could not be asked must never resolve to 'not registered, delete it'")
}

// The bound must not change the ordinary path: a healthy repo still removes and
// deregisters, so this fix costs nothing when nothing is stalled.
func TestRemoveWorktreeDirStillRemovesWhenGitIsHealthy(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repoRoot, "worktree", "add", "-b", "af-3096-healthy", worktreePath)
	require.DirExists(t, worktreePath)

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)

	require.NoError(t, err)
	assert.True(t, removed)
	assert.NoDirExists(t, worktreePath)
	stillThere, probeErr := worktreeRegisteredIn(repoRoot, worktreePath)
	require.NoError(t, probeErr)
	assert.False(t, stillThere, "the registration must be gone, or the branch delete that follows is blocked")
}
