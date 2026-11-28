package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFetchRestoreBranch_ScriptDetachesHead pins the script shape the ssh/hook
// provisioner emits: the fresh clone has the origin's default branch checked
// out, and git refuses `fetch origin <branch>:<branch>` when that branch is the
// checked-out one. The script must detach HEAD before the fetch (mirroring
// seedFromBundle), and carry the unborn-HEAD arm (--update-head-ok plus a
// conditional detach + reset --hard) so a fetch that births an unborn HEAD
// frees the branch for the agent-server's later `git worktree add`.
func TestFetchRestoreBranch_ScriptDetachesHead(t *testing.T) {
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: "/s"}
	require.NoError(t, w.fetchRestoreBranch("main", time.Second))
	require.Len(t, sh.scripts, 1)
	script := sh.scripts[0]
	require.Contains(t, script, "git -C '/s/workspace'")
	require.Contains(t, script, "checkout -q --detach")
	require.Contains(t, script, "fetch origin 'main:main'")
	require.Contains(t, script, "fetch --update-head-ok origin 'main:main'")
	require.Contains(t, script, "reset -q --hard")
	// The refspec is one quoted word (matching seedFromBundle), not two
	// separately-quoted halves.
	require.Contains(t, script, "'main:main'")
	require.NotContains(t, script, "'main':'main'")
}

// TestFetchRestoreBranchIntoCheckedOutDefaultBranch is THE bug case: the
// archived session branch is the origin's default branch name, so the fresh
// non-bare clone has it checked out. `git fetch origin main:main` would be
// refused with exit 128 ("refusing to fetch into branch 'refs/heads/main'
// checked out at ...") — even though the branch is fully pushed and origin is
// ahead. Detaching HEAD first makes the fetch fast-forward the local ref and
// leaves the branch free for the downstream `git worktree add main`.
func TestFetchRestoreBranchIntoCheckedOutDefaultBranch(t *testing.T) {
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "--bare", "-b", "main")

	// The session worked directly on main: a base commit plus session work,
	// pushed to origin at archive time.
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "base")
	gitIn(t, src, "remote", "add", "origin", origin)
	gitIn(t, src, "push", "-q", "origin", "main")

	// A fresh non-bare clone — main is checked out in the main worktree.
	sessionDir := t.TempDir()
	gitIn(t, sessionDir, "clone", "-q", origin, "workspace")

	// The session's archived tip advances on origin after the clone (the
	// restore fetch must fast-forward the local ref, not merely tolerate a
	// no-op).
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "session work")
	gitIn(t, src, "push", "-q", "origin", "main")
	want := gitIn(t, src, "rev-parse", "main")

	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir}
	require.NoError(t, w.fetchRestoreBranch("main", time.Minute))
	ws := filepath.Join(sessionDir, "workspace")

	// The local ref fast-forwarded to the archived tip.
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/main"))

	// HEAD was detached before the fetch, so main is not checked out anywhere
	// and the downstream worktree-add on it succeeds — the step the in-sandbox
	// agent-server's LOCAL backend runs (setupFromExistingBranch).
	assertDetachedHead(t, ws)
	assertWorktreeAddOK(t, ws, "main")
}

// TestFetchRestoreBranchIntoCheckedOutDefaultBranchUpToDate proves the git
// refusal is UNCONDITIONAL: it fires even when the fetched branch is already up
// to date with origin (clone's main == origin's main, nothing to fetch). This
// is the exact symptom the bug report names — a fully pushed, up-to-date branch
// failing the restore forever with a misleading "was it pushed to origin?".
// With the detach guard the no-op fetch succeeds.
func TestFetchRestoreBranchIntoCheckedOutDefaultBranchUpToDate(t *testing.T) {
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "--bare", "-b", "main")

	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "base")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "session work")
	gitIn(t, src, "remote", "add", "origin", origin)
	gitIn(t, src, "push", "-q", "origin", "main")
	want := gitIn(t, src, "rev-parse", "main")

	// A fresh clone — main is checked out and ALREADY at origin's tip.
	sessionDir := t.TempDir()
	gitIn(t, sessionDir, "clone", "-q", origin, "workspace")
	ws := filepath.Join(sessionDir, "workspace")
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/main"),
		"control: the clone already has main at the archived tip")

	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir}
	require.NoError(t, w.fetchRestoreBranch("main", time.Minute))

	// The ref is unchanged (the fetch was a no-op) and the branch is free.
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/main"))
	assertDetachedHead(t, ws)
	assertWorktreeAddOK(t, ws, "main")
}

// TestFetchRestoreBranchNonDefaultBranch is the control: restoring a branch
// that is NOT the origin's default branch never collided with the checked-out
// branch, so it must keep working (regression guard for the detach guard
// leaving the non-default fetched ref free for worktree-add).
func TestFetchRestoreBranchNonDefaultBranch(t *testing.T) {
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "--bare", "-b", "main")

	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "base")
	gitIn(t, src, "checkout", "-q", "-b", "af/task")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "session work")
	gitIn(t, src, "remote", "add", "origin", origin)
	gitIn(t, src, "push", "-q", "origin", "main")
	gitIn(t, src, "push", "-q", "origin", "af/task")
	want := gitIn(t, src, "rev-parse", "af/task")

	// A fresh clone — main is checked out; af/task is not.
	sessionDir := t.TempDir()
	gitIn(t, sessionDir, "clone", "-q", origin, "workspace")
	ws := filepath.Join(sessionDir, "workspace")
	require.Equal(t, "refs/heads/main", gitIn(t, ws, "symbolic-ref", "HEAD"),
		"control: the clone has the default branch main checked out")

	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir}
	require.NoError(t, w.fetchRestoreBranch("af/task", time.Minute))

	// The local ref landed at the archived tip.
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/af/task"))

	// HEAD was detached before the fetch, so neither branch is checked out in
	// the main tree and the downstream worktree-add on the restored branch
	// succeeds.
	assertDetachedHead(t, ws)
	assertWorktreeAddOK(t, ws, "af/task")
}

// TestFetchRestoreBranchIntoUnbornHeadDifferentBranch mirrors
// TestSeedFromBundleIntoUnbornHeadDifferentBranch: an origin whose default
// branch is unborn (HEAD points at a branch with no commits) leaves the fresh
// clone's HEAD unborn, but the restore branch is a different, populated
// branch. The unborn arm must fetch with --update-head-ok and NOT run the
// detach + reset cleanup (HEAD is still unborn, `checkout --detach` would fail
// with "You are on a branch yet to be born"), leaving the restored branch free
// for worktree-add.
func TestFetchRestoreBranchIntoUnbornHeadDifferentBranch(t *testing.T) {
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	// Put work on a non-default branch, then point origin's HEAD back at the
	// unborn main so the clone's HEAD is unborn on main, not on af/task.
	gitIn(t, origin, "checkout", "-q", "-b", "af/task")
	writeFile(t, origin, "a.txt", "a")
	writeFile(t, origin, "b.txt", "b")
	gitIn(t, origin, "add", "a.txt", "b.txt")
	gitIn(t, origin, "commit", "-q", "-m", "work")
	gitIn(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	want := gitIn(t, origin, "rev-parse", "af/task")

	// A fresh clone — HEAD is unborn on main; origin/af/task is fetched.
	sessionDir := t.TempDir()
	gitIn(t, sessionDir, "clone", "-q", origin, "workspace")
	ws := filepath.Join(sessionDir, "workspace")
	gitErr(t, ws, "rev-parse", "-q", "--verify", "HEAD")
	require.Equal(t, "refs/heads/main", gitIn(t, ws, "symbolic-ref", "HEAD"),
		"control: the clone's HEAD is unborn on main")

	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir}
	require.NoError(t, w.fetchRestoreBranch("af/task", time.Minute))

	// The restored branch lands; HEAD stays unborn on main (the fetch did not
	// bring main, so nothing born it).
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/af/task"))
	gitErr(t, ws, "rev-parse", "-q", "--verify", "HEAD")
	require.Equal(t, "refs/heads/main", gitIn(t, ws, "symbolic-ref", "HEAD"))

	// The clone's tree is empty (nothing was checked out) and clean.
	assertCleanWorktree(t, ws)

	// The restored branch is free for the downstream `git worktree add`.
	wt := assertWorktreeAddOK(t, ws, "af/task")
	require.FileExists(t, filepath.Join(wt, "a.txt"))
	require.FileExists(t, filepath.Join(wt, "b.txt"))
}

// TestDockerFetchRestoreBranch_ScriptDetachesHead pins the script shape the
// docker provisioner emits through execSessionSh: it must carry the same
// detach-before-fetch guard as the ssh path, so restoring a session whose
// archived branch equals the origin's default branch into the fresh /workspace
// clone succeeds instead of failing with exit 128.
func TestDockerFetchRestoreBranch_ScriptDetachesHead(t *testing.T) {
	var script string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		// execSessionSh argv: ["exec", <containerID>, "/bin/sh", "-c", <script>]
		// (no credentialMounts/Account.Dir/containerUser → no env prefix, no
		// trust-path wrapper, no --user).
		if len(args) > 0 && args[0] == "exec" {
			script = args[len(args)-1]
		}
		return nil, nil
	}))
	p := &dockerProvisioner{containerID: "cid"}
	require.NoError(t, p.fetchRestoreBranch("main"))
	require.Contains(t, script, "git -C '/workspace'")
	require.Contains(t, script, "checkout -q --detach")
	require.Contains(t, script, "fetch origin 'main:main'")
	require.Contains(t, script, "fetch --update-head-ok origin 'main:main'")
	require.Contains(t, script, "reset -q --hard")
	require.Contains(t, script, "'main:main'")
	require.NotContains(t, script, "'main':'main'")
}
