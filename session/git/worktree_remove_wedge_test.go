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

// A DEADLINE ALONE DOES NOT BOUND THIS, which is the trap the first cut of the
// fix fell into. exec.Cmd.Wait blocks on c.Process.Wait() before it consults the
// context or WaitDelay, and waitpid does not return until the process exits — so
// against the issue's own motivating case (git wedged in an uninterruptible
// syscall on a dead mount, SIGKILL delivered but pending) the deadline fires and
// Output() still blocks forever.
//
// Tested at the supervision boundary rather than by simulating D-state, which is
// not creatable portably from userspace: `run` here simply never returns, which
// is exactly what an unreapable child produces.
func TestAwaitCommandAbandonsAChildItCannotReap(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	// A command whose wait never completes, standing in for waitpid on a process
	// that cannot be reaped.
	cmd := exec.Command("sh", "-c", "sleep 300")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	start := time.Now()
	done := make(chan bool, 1)
	go func() {
		// Wait on a process nothing is going to kill, with a short abandon limit.
		_, _, reaped := awaitCommand(exec.Command("sh", "-c", "sleep 300"), false, 300*time.Millisecond)
		done <- reaped
	}()

	select {
	case reaped := <-done:
		assert.False(t, reaped, "a child that has not exited must be reported as UNREAPED, not waited on")
		assert.Less(t, time.Since(start), 20*time.Second,
			"the caller must return at its abandon limit rather than block on waitpid")
	case <-time.After(30 * time.Second):
		t.Fatal("awaitCommand BLOCKED on a child that never exits (#3096): Cmd.Wait waits on waitpid " +
			"BEFORE consulting the context or WaitDelay, so a deadline alone cannot make it return — " +
			"which is exactly the D-state case this issue is about")
	}
}

// And the ordinary path still wins: a killable stall must produce the proper
// timeout error, reaped and diagnosed, never the abandon message. The abandon
// grace exceeds gitWaitDelay precisely so this ordering holds.
func TestKillableStallTakesTheOrdinaryTimeoutPath(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	stallingGitFor(t, "list")
	shortenLocalTimeout(t, 200*time.Millisecond)

	_, err := worktreeRegisteredIn(repoRoot, filepath.Join(t.TempDir(), "wt"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "made no progress",
		"a KILLABLE stall must report the ordinary deadline, which reaps and diagnoses properly")
	assert.NotContains(t, err.Error(), "could not be killed",
		"the abandon path is for a child that cannot be reaped, and must not claim that of one that can")
}

// …and that runBoundedWorktreeGit actually USES the supervision. Separate from
// the test above on purpose: a killable child always takes the ordinary deadline
// path, so the abandon branch is unreachable with a real process — and a test of
// awaitCommand alone passes just as well when nothing calls it. That gap was
// real; removing the call site left the direct test green.
func TestRunBoundedWorktreeGitReportsAnUnreapableChild(t *testing.T) {
	prev := awaitCommandFn
	awaitCommandFn = func(*exec.Cmd, bool, time.Duration) ([]byte, error, bool) {
		return nil, nil, false // the child could not be reaped
	}
	t.Cleanup(func() { awaitCommandFn = prev })

	_, err := runBoundedWorktreeGit(t.TempDir(), true, "worktree", "remove", "-f", "/somewhere")

	require.Error(t, err, "an unreapable child must be REPORTED, never waited on")
	assert.True(t, errors.Is(err, ErrWorktreeRemovalTimedOut),
		"and must classify as a stall, so RemoveWorktreeDir retains the record, got %v", err)
	assert.Contains(t, err.Error(), "could not be killed",
		"the message must say the process is still running, not imply a clean failure")
}
