package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wsGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestMeasureWorkspace(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		f := MeasureWorkspace(filepath.Join(t.TempDir(), "gone"))
		assert.False(t, f.Exists)
		assert.Empty(t, f.Unverified)
	})

	t.Run("an unanswerable stat is unverified, not missing", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, nil, 0o644))
		f := MeasureWorkspace(filepath.Join(file, "child")) // ENOTDIR, not ENOENT
		assert.True(t, f.Exists)
		assert.Equal(t, []string{"exists"}, f.Unverified)
	})

	t.Run("clean repo with a remote", func(t *testing.T) {
		origin := t.TempDir()
		wsGit(t, origin, "init", "-q", "--bare")
		dir := t.TempDir()
		wsGit(t, dir, "init", "-q", "-b", "main")
		wsGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base")
		wsGit(t, dir, "remote", "add", "origin", origin)
		wsGit(t, dir, "push", "-q", "origin", "main")
		wsGit(t, dir, "fetch", "-q", "origin")
		f := MeasureWorkspace(dir)
		assert.Equal(t, WorkspaceFacts{Exists: true}, f)
	})

	t.Run("dirty, unpushed, extra branch, stash", func(t *testing.T) {
		dir := t.TempDir()
		wsGit(t, dir, "init", "-q", "-b", "main")
		id := []string{"-c", "user.name=t", "-c", "user.email=t@t"}
		wsGit(t, dir, append(id, "commit", "-q", "--allow-empty", "-m", "one")...)
		wsGit(t, dir, "branch", "side")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked"), []byte("a"), 0o644))
		wsGit(t, dir, "add", "tracked")
		wsGit(t, dir, append(id, "stash", "push", "-q")...)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked"), []byte("b"), 0o644))
		f := MeasureWorkspace(dir)
		assert.True(t, f.Exists)
		assert.Equal(t, 1, f.DirtyFiles)
		assert.Equal(t, 1, f.UnpushedCommits)
		assert.Equal(t, 1, f.ExtraBranches)
		assert.Equal(t, 1, f.Stashes)
		assert.Empty(t, f.Unverified)
	})

	t.Run("not a git repo is unverified, never clean", func(t *testing.T) {
		f := MeasureWorkspace(t.TempDir())
		assert.True(t, f.Exists)
		assert.NotEmpty(t, f.Unverified)
	})
}

// commitIn makes an empty commit with a fixed identity.
func commitIn(t *testing.T, dir, msg string) {
	t.Helper()
	wsGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", msg)
}

// cloneWithLinkedCheckout builds what an off-box session leaves on its host: a
// clone (the main worktree) and the agent's linked checkout on its own branch with
// one commit that exists nowhere else.
func cloneWithLinkedCheckout(t *testing.T) (clone, checkout string) {
	t.Helper()
	root := t.TempDir()
	clone = filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(clone, 0o755))
	wsGit(t, clone, "init", "-q", "-b", "main")
	commitIn(t, clone, "base")
	checkout = filepath.Join(root, "workspace-sess")
	wsGit(t, clone, "worktree", "add", "-q", "-b", "af/sess", checkout)
	commitIn(t, checkout, "agent work")
	return clone, checkout
}

// #5964: a vanished linked checkout is not an empty workspace. The session branch,
// its commits and the stash live in the clone's shared git directory, which
// teardown removes with the session directory.
func TestMeasureWorkspaceAtMissingCheckoutMeasuresTheSurvivingClone(t *testing.T) {
	clone, checkout := cloneWithLinkedCheckout(t)
	require.NoError(t, os.RemoveAll(checkout))

	f := MeasureWorkspaceAt(checkout, clone)
	assert.True(t, f.Exists, "the clone is still on the host")
	assert.True(t, f.CheckoutMissing)
	assert.Equal(t, 2, f.UnpushedCommits, "the base commit and the agent's, both on a remote-less clone")
	assert.Equal(t, 1, f.ExtraBranches, "the session branch is no longer checked out anywhere")
	assert.Empty(t, f.Unverified)
}

func TestMeasureWorkspaceAtOnlyAbsenceOfBothIsGone(t *testing.T) {
	root := t.TempDir()
	f := MeasureWorkspaceAt(filepath.Join(root, "co"), filepath.Join(root, "clone"))
	assert.False(t, f.Exists)
	assert.False(t, f.CheckoutMissing)
	assert.Empty(t, f.Unverified)
}

// The scope is the whole repository, the one the orphan sweep's "clean" uses.
func TestMeasureWorkspaceAtCoversEveryWorktreeAndBranch(t *testing.T) {
	clone, checkout := cloneWithLinkedCheckout(t)
	// Uncommitted state in the agent's checkout AND in the clone's own worktree.
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b"), 0o644))
	// A second linked worktree holding a detached commit nothing else contains.
	other := filepath.Join(t.TempDir(), "other")
	wsGit(t, clone, "worktree", "add", "-q", "--detach", other)
	commitIn(t, other, "detached work")
	// And a side branch with a commit.
	wsGit(t, clone, "branch", "side", "main")

	f := MeasureWorkspaceAt(checkout, clone)
	assert.Equal(t, 2, f.DirtyFiles, "both worktrees' uncommitted files")
	assert.Equal(t, 3, f.UnpushedCommits, "base, the agent's commit and the detached one (no remote in this fixture)")
	assert.Equal(t, 1, f.ExtraBranches, "side is checked out nowhere; main and af/sess are")
	assert.Empty(t, f.Unverified)
}

// A branch is judged against ITS upstream, not against any remote: a commit that
// another remote has but the branch's own upstream lacks is not safe.
func TestMeasureWorkspaceAtJudgesABranchAgainstItsUpstream(t *testing.T) {
	clone, _ := cloneWithLinkedCheckout(t)
	r1, r2 := t.TempDir(), t.TempDir()
	wsGit(t, r1, "init", "-q", "--bare")
	wsGit(t, r2, "init", "-q", "--bare")
	wsGit(t, clone, "remote", "add", "r1", r1)
	wsGit(t, clone, "remote", "add", "r2", r2)
	wsGit(t, clone, "push", "-q", "r1", "main", "af/sess") // r1 has everything
	wsGit(t, clone, "push", "-q", "r2", "main")            // r2 lacks af/sess's commit
	wsGit(t, clone, "fetch", "-q", "--all")
	wsGit(t, clone, "branch", "-q", "--set-upstream-to=r2/main", "af/sess")
	f := MeasureWorkspaceAt("", clone)
	assert.Equal(t, 1, f.UnpushedCommits, "r1 has the commit but the branch's own upstream, r2/main, does not")
	assert.Empty(t, f.Unverified)
}

// #5965: a stash git cannot read is unknown, not a verified zero. The stashed
// commit is deleted, so `reflog show` fails while the ref and its reflog remain.
func TestMeasureWorkspaceAtACorruptStashIsUnverifiedNotZero(t *testing.T) {
	clone, checkout := cloneWithLinkedCheckout(t)
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "tracked"), []byte("t"), 0o644))
	wsGit(t, checkout, "add", "tracked")
	wsGit(t, checkout, "-c", "user.name=t", "-c", "user.email=t@t", "stash", "push", "-q")
	require.Equal(t, 1, MeasureWorkspaceAt(checkout, clone).Stashes, "premise: a healthy stash is counted")

	out, err := exec.Command("git", "-C", checkout, "rev-parse", "refs/stash").Output()
	require.NoError(t, err)
	hash := strings.TrimSpace(string(out))
	common := strings.TrimSpace(string(mustOut(t, exec.Command("git", "-C", checkout, "rev-parse", "--git-common-dir"))))
	if !filepath.IsAbs(common) {
		common = filepath.Join(checkout, common)
	}
	require.NoError(t, os.Remove(filepath.Join(common, "objects", hash[:2], hash[2:])), "the stash commit must be loose to delete")

	f := MeasureWorkspaceAt(checkout, clone)
	assert.Contains(t, f.Unverified, "stashes", "an unreadable stash is unknown")
}

func TestMeasureWorkspaceAtNoStashIsAVerifiedZero(t *testing.T) {
	clone, checkout := cloneWithLinkedCheckout(t)
	f := MeasureWorkspaceAt(checkout, clone)
	assert.Zero(t, f.Stashes)
	assert.NotContains(t, f.Unverified, "stashes")
}

func mustOut(t *testing.T, cmd *exec.Cmd) []byte {
	t.Helper()
	out, err := cmd.Output()
	require.NoError(t, err)
	return out
}

// refTableRepo initialises a repository on the reftable ref backend, or skips when
// this git cannot (it needs 2.45).
func refTableRepo(t *testing.T) (clone, checkout string) {
	t.Helper()
	root := t.TempDir()
	clone = filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(clone, 0o755))
	cmd := exec.Command("git", "-C", clone, "init", "-q", "-b", "main", "--ref-format=reftable")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this git cannot create a reftable repository: %v: %s", err, out)
	}
	commitIn(t, clone, "base")
	checkout = filepath.Join(root, "workspace-sess")
	wsGit(t, clone, "worktree", "add", "-q", "-b", "af/sess", checkout)
	commitIn(t, checkout, "agent work")
	return clone, checkout
}

// #5999: on a reftable repository there are no loose refs, reflog files or
// packed-refs, so a stash read from those files is a "verified zero" over an
// intact stash. The measurement asks git instead.
func TestMeasureWorkspaceAtCountsAStashOnAReftableRepository(t *testing.T) {
	clone, checkout := refTableRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "tracked"), []byte("t"), 0o644))
	wsGit(t, checkout, "add", "tracked")
	wsGit(t, checkout, "-c", "user.name=t", "-c", "user.email=t@t", "stash", "push", "-q")
	_, statErr := os.Stat(filepath.Join(clone, ".git", "refs", "stash"))
	require.Error(t, statErr, "premise: reftable keeps no loose refs/stash file")

	f := MeasureWorkspaceAt(checkout, clone)
	assert.Equal(t, 1, f.Stashes, "the stash is counted though no ref file names it")
	assert.Empty(t, f.Unverified)

	// And with the checkout gone, from the clone alone.
	require.NoError(t, os.RemoveAll(checkout))
	f = MeasureWorkspaceAt(checkout, clone)
	assert.True(t, f.CheckoutMissing)
	assert.Equal(t, 1, f.Stashes)
}

func TestMeasureWorkspaceAtReftableWithNoStashIsAVerifiedZero(t *testing.T) {
	clone, checkout := refTableRepo(t)
	f := MeasureWorkspaceAt(checkout, clone)
	assert.Zero(t, f.Stashes)
	assert.NotContains(t, f.Unverified, "stashes")
	assert.Equal(t, 2, f.UnpushedCommits, "branches are read through git on reftable too")
}

// #6000: a worktree path containing a newline is one record, not a truncated one
// that is silently skipped.
func TestMeasureWorkspaceAtCountsAWorktreeWhosePathHasANewline(t *testing.T) {
	clone, _ := cloneWithLinkedCheckout(t)
	odd := filepath.Join(t.TempDir(), "odd\nbranch refs/heads/fake")
	wsGit(t, clone, "worktree", "add", "-q", "--detach", odd)
	require.NoError(t, os.WriteFile(filepath.Join(odd, "dirty.txt"), []byte("d"), 0o644))
	commitIn(t, odd, "work in the oddly named worktree")

	f := MeasureWorkspaceAt("", clone)
	assert.Equal(t, 1, f.DirtyFiles, "the uncommitted file in the newline-named worktree is counted")
	assert.GreaterOrEqual(t, f.UnpushedCommits, 3, "base, the agent's commit and the detached one")
	assert.Empty(t, f.Unverified, "a NUL-terminated listing is trusted")
}

func TestWorktreeListWithoutZIsDistrustedWhenAPathSpansLines(t *testing.T) {
	// The line-by-line reading of an older git: a path continued onto a second line
	// is not a keyword, so the listing cannot be trusted.
	trees, ok := parseWorktreeList("worktree /a\nHEAD abc\nbranch refs/heads/main\n\nworktree /b\nodd\nHEAD def\n", "\n")
	assert.False(t, ok)
	assert.Nil(t, trees)
	trees, ok = parseWorktreeList("worktree /a\nHEAD abc\nbranch refs/heads/main\n\nworktree /b\nHEAD def\ndetached\n", "\n")
	assert.True(t, ok)
	assert.Len(t, trees, 2)
}

// #6012 review: if refs/stash is gone but its reflog survives, the stash entries
// still exist and teardown destroys them. That is never a zero.
func TestMeasureWorkspaceAtAStashReflogOutlivingItsRefIsNotZero(t *testing.T) {
	clone, checkout := cloneWithLinkedCheckout(t)
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "tracked"), []byte("t"), 0o644))
	wsGit(t, checkout, "add", "tracked")
	wsGit(t, checkout, "-c", "user.name=t", "-c", "user.email=t@t", "stash", "push", "-q")
	common := strings.TrimSpace(string(mustOut(t, exec.Command("git", "-C", checkout, "rev-parse", "--git-common-dir"))))
	if !filepath.IsAbs(common) {
		common = filepath.Join(checkout, common)
	}
	ref := filepath.Join(common, "refs", "stash")
	if _, err := os.Stat(ref); err != nil {
		t.Skip("refs/stash is not a loose file on this ref backend")
	}
	require.NoError(t, os.Remove(ref), "delete the ref, keep its reflog")
	require.FileExists(t, filepath.Join(common, "logs", "refs", "stash"), "premise: the reflog survives")

	f := MeasureWorkspaceAt(checkout, clone)
	assert.True(t, f.Stashes > 0 || contains(f.Unverified, "stashes"),
		"a surviving stash reflog must not read as a verified zero: %+v", f)
	assert.Contains(t, f.Unverified, "stashes")
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
