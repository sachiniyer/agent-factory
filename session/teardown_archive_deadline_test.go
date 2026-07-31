package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The archive mode used to document a hole it could not yet fall into:
//
//	stateKnown either way: MoveWorktree runs on the UNBOUNDED local-git runner,
//	so it cannot report an unknown ... If the move is ever bounded, a tripped
//	deadline must return stateUnknown here.
//
// #2575 bounded it. A git SIGKILLed mid-move or mid-repair may have moved the
// bytes, written part of its registration, or neither — and the pre-existing
// stateKnown would have let teardown finalize over that: finalize clears the
// tmux refs and the worktree pointer, which are exactly what a retry needs to
// find intact, so the record gets dropped on top of a half-relocated workspace.
//
// These tests drive a REAL bound: a git that makes no progress, the production
// runner, the real sentinel, and the real classification.

// stallingGitOnPathForArchive puts a `git` earlier on PATH that never exits.
// The sleep runs in a CHILD so the capture pipe outlives the direct process —
// passing requires the process-group kill, not just a context deadline.
func stallingGitOnPathForArchive(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 300 &\nwait\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// archiveWorktreeForDeadlineTest builds a real repo with a real linked worktree,
// using real git — before any stalling git is installed on PATH.
func archiveWorktreeForDeadlineTest(t *testing.T) *git.GitWorktree {
	t.Helper()
	repoRoot := initTempGitRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		// Supply the identity explicitly. A CI runner has no global gitconfig, so
		// a fixture that commits without it fails with "Author identity unknown"
		// — matching session/git's runGitInPlaceTest, which learned the same.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("commit", "--allow-empty", "-m", "init")
	wtPath := filepath.Join(filepath.Dir(repoRoot), "wt-archive-deadline")
	run("worktree", "add", "-b", "arch/deadline", wtPath)

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "arch", "arch/deadline", "", false, true)
	require.NoError(t, err)
	return gw
}

func TestTeardownArchive_RelocateCutOffByDeadlineIsUnknown(t *testing.T) {
	gw := archiveWorktreeForDeadlineTest(t)
	dest := filepath.Join(t.TempDir(), "archived", "arch")

	stallingGitOnPathForArchive(t)
	t.Cleanup(git.SetLocalGitTimeoutForTest(200 * time.Millisecond))

	state, err := teardownArchive{dest: dest}.handleWorktree(gw, "arch")

	require.Error(t, err, "a relocate that was cut off must not report success")
	require.ErrorIs(t, err, git.ErrRelocateStateUnknown,
		"the git layer must mark a deadline-cut relocate as unestablished")
	require.Equal(t, stateUnknown, state,
		"archive must not finalize over a relocate whose effect it never established: finalize "+
			"clears the tmux refs and worktree pointer a retry needs (session/teardown.go)")
}

// The other direction matters just as much: bounding must not turn every failed
// archive into an unknown. An ordinary move failure — here, a destination that
// already exists, rejected before any git runs — keeps the pre-#1917 contract of
// stateKnown, so the daemon's rollback to Lost (which requires finalize to have
// run) still fires exactly as it did.
func TestTeardownArchive_OrdinaryMoveFailureStaysKnown(t *testing.T) {
	gw := archiveWorktreeForDeadlineTest(t)
	dest := filepath.Join(t.TempDir(), "already-there")
	require.NoError(t, os.MkdirAll(dest, 0o755))

	state, err := teardownArchive{dest: dest}.handleWorktree(gw, "arch")

	require.Error(t, err)
	require.NotErrorIs(t, err, git.ErrRelocateStateUnknown,
		"a move that failed cleanly established its outcome: nothing was left half-done")
	require.Equal(t, stateKnown, state,
		"an ordinary failed archive must still finalize so the daemon can roll the session back to Lost")
}

// And the teardown core must act on the classification, not merely receive it.
func TestTeardownTabs_ArchiveUnknownRelocateKeepsTheRecord(t *testing.T) {
	mode := &gateStubMode{closeState: stateKnown, worktreeState: stateUnknown}
	inst := instanceWithTmuxTab(t, &tmux.TmuxSession{})
	gw := &git.GitWorktree{}
	inst.gitWorktree = gw

	err := inst.teardownTabs(mode)

	require.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"an unestablished worktree action must be reported so the caller keeps the record")
	require.False(t, mode.finalizeCalled,
		"finalize must be skipped so a retry still finds the workspace")
	require.Same(t, gw, inst.gitWorktree, "the retry must not lose the workspace pointer")
}
