package session

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingShell struct {
	scripts []string
	stdin   []string
}

func (r *recordingShell) Run(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
	r.scripts = append(r.scripts, script)
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	r.stdin = append(r.stdin, body)
	return nil, nil
}

func TestSeedFromBundle(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "x.bundle")
	require.NoError(t, os.WriteFile(bundle, []byte("BUNDLE"), 0o600))
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: "/s", spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/feat", "refs/af-move/k/snapshot"},
	}}
	require.NoError(t, w.seedFromBundle(time.Second))
	require.Len(t, sh.scripts, 2)
	require.Equal(t, "BUNDLE", sh.stdin[0])
	require.Contains(t, sh.scripts[0], "/s/seed.bundle")
	fetch := sh.scripts[1]
	require.Contains(t, fetch, "git -C '/s/workspace' fetch '/s/seed.bundle'")
	require.Contains(t, fetch, "checkout -q --detach")
	require.Contains(t, fetch, "'refs/heads/feat:refs/heads/feat'")
	require.Contains(t, fetch, "'refs/af-move/k/snapshot:refs/af-move/k/snapshot'")
	// The unborn-HEAD else arm must (a) use --update-head-ok so the fetch into
	// the occupied unborn branch is accepted at all, and (b) conditionally
	// detach + reset --hard after the fetch, so a seed that "borns" the unborn
	// branch does not leave it checked out with a stale empty index. See
	// TestSeedFromBundleIntoUnbornHead for the real-git proof.
	require.Contains(t, fetch, "fetch --update-head-ok '/s/seed.bundle'")
	require.Contains(t, fetch, "reset -q --hard")
	require.True(t, strings.HasSuffix(fetch, "rm -f '/s/seed.bundle'"))
}

func TestSeedFromBundleNoopWithoutBundle(t *testing.T) {
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, spec: ProvisionSpec{}}
	require.NoError(t, w.seedFromBundle(time.Second))
	require.Empty(t, sh.scripts)
	w.spec.SeedBundlePath = "/nonexistent"
	require.Error(t, w.seedFromBundle(time.Second))
	w.spec.SeedBundlePath = os.DevNull
	require.ErrorContains(t, w.seedFromBundle(time.Second), "no refs")
}

// localShell runs scripts for real, so the generated git command is exercised.
type localShell struct{}

func (localShell) Run(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = stdin
	return cmd.CombinedOutput()
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

// gitErr asserts the git command FAILS and returns its trimmed combined output
// — the mirror of gitIn for commands whose failure is the assertion (e.g.
// `symbolic-ref HEAD` against a detached HEAD, or `rev-parse --verify HEAD`
// against an unborn one).
func gitErr(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "expected git %s to fail, but it succeeded: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// gitRun runs a git command and returns its exit error, discarding output. Use
// it for a command whose exit status is not itself the assertion (e.g. a commit
// attempt that is expected to be a no-op once the seed leaves a clean tree).
func gitRun(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("git %s (non-fatal): %s", strings.Join(args, " "), out)
	}
	return err
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
}

// assertCleanWorktree asserts the working tree at ws has no staged, unstaged, or
// untracked changes — the invariant the seed must leave behind so a later
// commit cannot silently delete the seeded content.
func assertCleanWorktree(t *testing.T, ws string) {
	t.Helper()
	require.Equal(t, "", gitIn(t, ws, "status", "--porcelain"),
		"seeded workspace must have a clean working tree")
}

// assertDetachedHead asserts HEAD at ws is detached (not symbolic to a branch),
// so the seeded branch is free for the downstream `git worktree add <branch>`.
func assertDetachedHead(t *testing.T, ws string) {
	t.Helper()
	require.Contains(t, gitErr(t, ws, "symbolic-ref", "HEAD"), "not a symbolic ref",
		"expected detached HEAD after seed")
}

// assertWorktreeAddOK asserts `git worktree add <path> <branch>` succeeds
// against the seeded repo — the downstream step (session/git/worktree_ops.go's
// setupFromExistingBranch, which runs without --force) the W7.9 move flow hands
// the seeded branch to. Returns the new worktree's path.
func assertWorktreeAddOK(t *testing.T, repo, branch string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", wt, branch)
	return wt
}

// #5685: the seed carries the branch the fresh clone has checked out. The born
// arm detaches HEAD before the fetch, so the seeded branch is never checked out
// in the clone and the downstream `git worktree add <branch>` succeeds.
func TestSeedFromBundleIntoCheckedOutBranch(t *testing.T) {
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	gitIn(t, origin, "commit", "-q", "--allow-empty", "-m", "base")

	// The session worked directly on main: one extra commit, bundled.
	src := t.TempDir()
	gitIn(t, src, "clone", "-q", origin, ".")
	gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "session work")
	bundle := filepath.Join(t.TempDir(), "s.bundle")
	gitIn(t, src, "bundle", "create", bundle, "refs/heads/main")
	want := gitIn(t, src, "rev-parse", "main")

	sessionDir := t.TempDir()
	gitIn(t, sessionDir, "clone", "-q", origin, "workspace")
	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir, spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/main"},
	}}
	require.NoError(t, w.seedFromBundle(time.Minute))
	ws := filepath.Join(sessionDir, "workspace")
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/main"))
	require.NoFileExists(t, w.SeedBundlePath())

	// HEAD was detached before the fetch, so the seeded branch is not checked
	// out anywhere and the downstream worktree-add on it succeeds.
	assertCleanWorktree(t, ws)
	assertDetachedHead(t, ws)
	assertWorktreeAddOK(t, ws, "main")
}

// An empty origin leaves HEAD unborn. When the seed brings the very branch HEAD
// is unborn on (the #5685 case), the --update-head-ok fetch "borns" it: HEAD
// comes to point at the seeded commit while the index and worktree still
// reflect the empty pre-seed state, and the branch is left checked out in the
// clone's own tree. That breaks the downstream `git worktree add <branch>`
// ("'<branch>' is already used by worktree at '.../workspace'") and stages
// every seeded file for deletion, so a later commit silently empties the
// branch. This is the trigger case for that corruption; the seed must detach +
// reset after the fetch so HEAD is detached, the tree is clean, and the branch
// is free.
func TestSeedFromBundleIntoUnbornHead(t *testing.T) {
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	writeFile(t, src, "a.txt", "a")
	writeFile(t, src, "b.txt", "b")
	gitIn(t, src, "add", "a.txt", "b.txt")
	gitIn(t, src, "commit", "-q", "-m", "work")
	want := gitIn(t, src, "rev-parse", "main")
	bundle := filepath.Join(t.TempDir(), "s.bundle")
	gitIn(t, src, "bundle", "create", bundle, "refs/heads/main")

	sessionDir := t.TempDir()
	ws := filepath.Join(sessionDir, "workspace")
	gitIn(t, sessionDir, "init", "-q", "-b", "main", ws)
	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir, spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/main"},
	}}
	require.NoError(t, w.seedFromBundle(time.Minute))

	// The ref tip lands and the bundle is reaped.
	require.Equal(t, want, gitIn(t, ws, "rev-parse", "refs/heads/main"))
	require.NoFileExists(t, w.SeedBundlePath())

	// HEAD is detached: the fetch born main, and the cleanup detached so the
	// branch is free for worktree add. Before the fix HEAD stayed symbolic to
	// refs/heads/main and the branch was checked out here.
	assertDetachedHead(t, ws)

	// The index/worktree are synced to the seeded commit — no phantom staged
	// deletions. Before the fix `git status --porcelain` was "D  a.txt\nD  b.txt".
	assertCleanWorktree(t, ws)

	// The seeded files are present in the clone's tree and on disk.
	require.Equal(t, "a.txt\nb.txt", gitIn(t, ws, "ls-tree", "-r", "--name-only", "HEAD"))
	require.FileExists(t, filepath.Join(ws, "a.txt"))
	require.FileExists(t, filepath.Join(ws, "b.txt"))

	// The downstream `git worktree add main` succeeds and checks out the seeded
	// content. Before the fix this failed with "'main' is already used by
	// worktree at '.../workspace'".
	wt := assertWorktreeAddOK(t, ws, "main")
	require.FileExists(t, filepath.Join(wt, "a.txt"))
	require.FileExists(t, filepath.Join(wt, "b.txt"))

	// A commit in the seeded clone must NOT silently delete the seeded content.
	// Before the fix the stale index staged every seeded file for deletion, so
	// `git commit -a` emptied the branch (ls-tree HEAD became empty); with a
	// clean tree the commit is a nothing-to-commit no-op and the tree is
	// unchanged either way.
	_ = gitRun(t, ws, "commit", "-q", "-a", "-m", "probe")
	require.Equal(t, "a.txt\nb.txt", gitIn(t, ws, "ls-tree", "-r", "--name-only", "HEAD"),
		"a commit in the seeded clone must not delete the seeded content")
}

// The non-trigger arm of the unborn-HEAD case: the seed brings a branch the
// clone's HEAD is NOT unborn on. The fetch leaves HEAD unborn, so the cleanup
// must NOT run `checkout --detach` — that fails with "You are on a branch yet
// to be born" — and the seeded branch must still be worktree-addable on its own.
func TestSeedFromBundleIntoUnbornHeadDifferentBranch(t *testing.T) {
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "af/task")
	writeFile(t, src, "a.txt", "a")
	writeFile(t, src, "b.txt", "b")
	gitIn(t, src, "add", "a.txt", "b.txt")
	gitIn(t, src, "commit", "-q", "-m", "work")
	bundle := filepath.Join(t.TempDir(), "s.bundle")
	gitIn(t, src, "bundle", "create", bundle, "refs/heads/af/task")

	sessionDir := t.TempDir()
	ws := filepath.Join(sessionDir, "workspace")
	gitIn(t, sessionDir, "init", "-q", "-b", "main", ws) // HEAD unborn on main, NOT on af/task
	w := &sandboxWorkspace{shell: localShell{}, SessionDir: sessionDir, spec: ProvisionSpec{
		SeedBundlePath: bundle, SeedRefs: []string{"refs/heads/af/task"},
	}}
	require.NoError(t, w.seedFromBundle(time.Minute))

	// The seeded branch lands; HEAD stays unborn on main (the seed did not
	// bring main, so nothing born it).
	require.Equal(t, gitIn(t, src, "rev-parse", "af/task"), gitIn(t, ws, "rev-parse", "refs/heads/af/task"))
	gitErr(t, ws, "rev-parse", "-q", "--verify", "HEAD")
	require.Equal(t, "refs/heads/main", gitIn(t, ws, "symbolic-ref", "HEAD"))

	// The clone's tree is empty (nothing was checked out) and clean — the
	// pre-seed state the non-trigger case must preserve, which the conditional
	// cleanup must not disturb.
	assertCleanWorktree(t, ws)

	// The seeded branch is free for the downstream `git worktree add` on its
	// own branch.
	wt := assertWorktreeAddOK(t, ws, "af/task")
	require.FileExists(t, filepath.Join(wt, "a.txt"))
	require.FileExists(t, filepath.Join(wt, "b.txt"))
}
