package git

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// gitStdoutTest runs a git command in dir and returns trimmed stdout ONLY.
// runGitInPlaceTest folds stderr in, which is fine for assertions on prose but
// not for capturing a value: `git stash create` writes the sha to stdout and
// advice to stderr, and a mixed capture cannot be fed back to update-ref.
func gitStdoutTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "git %v: %s", args, stderr.String())
	return strings.TrimSpace(stdout.String())
}

// twoSessionWorktrees builds a repo with two tracked files and hands back two
// session worktrees created through af's OWN Setup() path — the same call the
// daemon makes when a session is created — plus the repo root.
//
// The worktrees must come from Setup() rather than a hand-rolled `git worktree
// add`: the claim under test is about what an af *session* gets, and a test
// that built its own worktrees could keep passing after af switched sessions to
// a mechanism (a clone, a separate GIT_DIR) that does not share refs.
func twoSessionWorktrees(t *testing.T) (repoRoot, wtA, wtB string) {
	t.Helper()
	sandboxHome(t)

	repoRoot = createGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.txt"), []byte("base\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "b.txt"), []byte("base\n"), 0644))
	runGitInPlaceTest(t, repoRoot, "add", "-A")
	runGitInPlaceTest(t, repoRoot, "commit", "-m", "initial")

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gwA, _, err := NewGitWorktree(repoRoot, "stash-sess-a")
	require.NoError(t, err)
	require.NoError(t, gwA.Setup())
	t.Cleanup(func() { _, _ = gwA.Cleanup() })

	gwB, _, err := NewGitWorktree(repoRoot, "stash-sess-b")
	require.NoError(t, err)
	require.NoError(t, gwB.Setup())
	t.Cleanup(func() { _, _ = gwB.Cleanup() })

	wtA, wtB = gwA.GetWorktreePath(), gwB.GetWorktreePath()
	require.NotEqual(t, wtA, wtB, "the two sessions must land in distinct worktrees")
	return repoRoot, wtA, wtB
}

// TestSessionWorktreesShareOneStashStack pins the trap behind #2801: two af
// session worktrees of one repo share a single `refs/stash`, so one session's
// `git stash pop` can take a sibling's entry. Nothing errors — the pop succeeds
// and delivers the wrong content, which is how a session commits changes it
// never wrote.
//
// This asserts the hazard, not a fix. git-worktree(1) documents refs/bisect,
// refs/worktree, and refs/rewritten as the only unshared namespaces under
// refs/, and there is no config that moves refs/stash into that set, so af
// cannot isolate the stack — it can only tell sessions not to use it. The
// assertion therefore guards a documentation claim: if this test ever fails
// because the pop stopped crossing worktrees, the prohibition in CLAUDE.md
// ("Git hygiene in a shared repo") and .claude/skills/dispatch-af-session.md is
// obsolete and should be deleted along with this test.
func TestSessionWorktreesShareOneStashStack(t *testing.T) {
	_, wtA, wtB := twoSessionWorktrees(t)

	// B stashes first, so A's entry lands on top of the shared stack. This
	// ordering is what makes the theft deterministic; in a real fleet it is
	// whichever two sessions happen to interleave.
	require.NoError(t, os.WriteFile(filepath.Join(wtB, "b.txt"), []byte("base\nB session work\n"), 0644))
	runGitInPlaceTest(t, wtB, "stash", "push", "-m", "B work")

	require.NoError(t, os.WriteFile(filepath.Join(wtA, "a.txt"), []byte("base\nA session work\n"), 0644))
	runGitInPlaceTest(t, wtA, "stash", "push", "-m", "A work")

	// B pops what it believes is its own entry.
	runGitInPlaceTest(t, wtB, "stash", "pop")

	popped, err := os.ReadFile(filepath.Join(wtB, "a.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(popped), "A session work",
		"B's pop took A's stash entry: refs/stash is shared across worktrees")

	// B's own work is still on the stack — it did not get what it asked for,
	// and A's entry is gone from A's point of view.
	stillStashed, err := os.ReadFile(filepath.Join(wtB, "b.txt"))
	require.NoError(t, err)
	assert.NotContains(t, string(stillStashed), "B session work",
		"B's own entry should still be stashed — the pop returned A's, not B's")

	listInA := runGitInPlaceTest(t, wtA, "stash", "list")
	assert.NotContains(t, listInA, "A work",
		"A's entry was consumed by a pop in another worktree")
	assert.Contains(t, listInA, "B work",
		"A sees B's entry: one stack, both sessions")
}

// TestStashCreateWithWorktreeRefIsPerWorktree is the guard on the substitute
// CLAUDE.md now tells every session to use instead of `git stash`. Two claims
// have to hold for that advice to be safe, and both are git behavior this repo
// depends on rather than owns:
//
//  1. `git stash create` records a dangling commit and does NOT push onto the
//     shared `refs/stash`, so it adds no writer to the contended stack; and
//  2. `refs/worktree/<name>` really is per-worktree, so two sessions can use
//     the SAME ref name without seeing each other.
//
// If a future git breaks either one, the documented recipe becomes as dangerous
// as the thing it replaced, and this test is what says so.
func TestStashCreateWithWorktreeRefIsPerWorktree(t *testing.T) {
	_, wtA, wtB := twoSessionWorktrees(t)

	const ref = "refs/worktree/af-wip"

	require.NoError(t, os.WriteFile(filepath.Join(wtA, "a.txt"), []byte("base\nA session work\n"), 0644))
	shaA := gitStdoutTest(t, wtA, "stash", "create", "A wip")
	require.NotEmpty(t, shaA, "stash create must record a commit for a dirty tree")
	runGitInPlaceTest(t, wtA, "update-ref", ref, shaA)
	runGitInPlaceTest(t, wtA, "checkout", "--", ".")

	require.NoError(t, os.WriteFile(filepath.Join(wtB, "b.txt"), []byte("base\nB session work\n"), 0644))
	shaB := gitStdoutTest(t, wtB, "stash", "create", "B wip")
	require.NotEmpty(t, shaB)
	runGitInPlaceTest(t, wtB, "update-ref", ref, shaB)
	runGitInPlaceTest(t, wtB, "checkout", "--", ".")

	require.NotEqual(t, shaA, shaB, "the two sessions set aside different work")

	// The same ref name resolves differently in each worktree. This is the
	// property `refs/stash` does not have.
	assert.Equal(t, shaA, gitStdoutTest(t, wtA, "rev-parse", ref),
		"A's %s must still be A's commit after B wrote the same ref name", ref)
	assert.Equal(t, shaB, gitStdoutTest(t, wtB, "rev-parse", ref),
		"B's %s must be B's own commit", ref)

	// Nothing reached the shared stack.
	assert.Empty(t, gitStdoutTest(t, wtA, "stash", "list"),
		"stash create must not push onto the shared refs/stash")
	assert.Empty(t, gitStdoutTest(t, wtB, "stash", "list"))

	// Restoring in A brings back A's work and nothing of B's.
	runGitInPlaceTest(t, wtA, "stash", "apply", ref)
	restored, err := os.ReadFile(filepath.Join(wtA, "a.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(restored), "A session work")
	foreign, err := os.ReadFile(filepath.Join(wtA, "b.txt"))
	require.NoError(t, err)
	assert.NotContains(t, string(foreign), "B session work",
		"A must not receive B's set-aside work")
}

// TestStashCreateEdgeCasesFailLoudly covers the ways the documented recipe can
// bite a session that pastes it without reading the caveats. Each is worth
// pinning because the failure has to stay LOUD: a `stash create` that quietly
// recorded nothing, followed by a clean, would destroy the work the session was
// trying to protect.
func TestStashCreateEdgeCasesFailLoudly(t *testing.T) {
	_, wtA, _ := twoSessionWorktrees(t)

	// A clean tree records nothing and prints an empty sha — hence the
	// `[ -n "$sha" ]` guard in the documented recipe. update-ref then refuses
	// rather than silently pointing the ref at nothing.
	assert.Empty(t, gitStdoutTest(t, wtA, "stash", "create", "nothing to save"),
		"stash create on a clean tree must record nothing")
	cmd := exec.Command("git", "update-ref", "refs/worktree/af-wip", "")
	cmd.Dir = wtA
	out, err := cmd.CombinedOutput()
	assert.Error(t, err, "update-ref with an empty sha must fail, not create a dangling ref")
	assert.Contains(t, string(out), "not a valid SHA1")

	// An untracked file is not captured at all: `stash create` reports nothing
	// for it. `git checkout -- .` also leaves untracked files in place, so the
	// content survives — but a session that expected it in the set-aside commit
	// would not find it there.
	require.NoError(t, os.WriteFile(filepath.Join(wtA, "untracked.txt"), []byte("new work\n"), 0644))
	assert.Empty(t, gitStdoutTest(t, wtA, "stash", "create", "untracked only"),
		"stash create does not capture untracked files")

	// Half-adding it with `git add -N` does not help — it makes stash create
	// fail outright. Fully `git add` it (or use the scratch-commit recipe).
	runGitInPlaceTest(t, wtA, "add", "-N", "untracked.txt")
	cmd = exec.Command("git", "stash", "create", "intent to add")
	cmd.Dir = wtA
	out, err = cmd.CombinedOutput()
	assert.Error(t, err, "stash create must fail loudly on an intent-to-add entry")
	assert.Contains(t, string(out), "not uptodate")

	runGitInPlaceTest(t, wtA, "add", "untracked.txt")
	sha := gitStdoutTest(t, wtA, "stash", "create", "fully added")
	require.NotEmpty(t, sha, "a fully added file IS captured")
	assert.Equal(t, "new work", gitStdoutTest(t, wtA, "cat-file", "-p", sha+":untracked.txt"))

	// Two more guards the recipe's own text depends on, both of which a session
	// only trips when it pastes the block somewhere slightly different.
	assertUpdateRefRefusesAnExistingSave(t, wtA, sha)
	assertCleanNeedsTheRootPathspec(t, wtA)
}

// assertUpdateRefRefusesAnExistingSave pins the trailing `""` old-value guard in
// the documented `git update-ref refs/worktree/af-wip "$sha" ""`. Without it, a
// session that sets work aside twice under the same name silently repoints the
// ref at the second commit and leaves the first reachable only through fsck.
func assertUpdateRefRefusesAnExistingSave(t *testing.T, wt, sha string) {
	t.Helper()
	const ref = "refs/worktree/af-wip-twice"

	runGitInPlaceTest(t, wt, "update-ref", ref, sha, "")
	require.Equal(t, sha, gitStdoutTest(t, wt, "rev-parse", ref))

	second := gitStdoutTest(t, wt, "rev-parse", "HEAD")
	require.NotEqual(t, sha, second)
	cmd := exec.Command("git", "update-ref", ref, second, "")
	cmd.Dir = wt
	out, err := cmd.CombinedOutput()
	assert.Error(t, err, "a second save under a taken name must be refused, not silently overwrite")
	assert.Contains(t, string(out), "already exists")
	assert.Equal(t, sha, gitStdoutTest(t, wt, "rev-parse", ref),
		"the first save must still be reachable under its name")
}

// assertCleanNeedsTheRootPathspec pins why the recipe cleans with `:/` rather
// than `.`. The two halves are asymmetric: `git stash create` records the whole
// worktree, while `git checkout -- .` only restores paths below the current
// directory. Run from a subdirectory, the bare form hands back a tree that reads
// as clean where it isn't, and the next build or commit picks up the leftovers.
func assertCleanNeedsTheRootPathspec(t *testing.T, wt string) {
	t.Helper()
	sub := filepath.Join(wt, "sub")
	require.NoError(t, os.MkdirAll(sub, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("base\n"), 0644))
	runGitInPlaceTest(t, wt, "add", "-A")
	runGitInPlaceTest(t, wt, "commit", "-m", "add a subdirectory")

	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("base\nroot edit\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("base\nsub edit\n"), 0644))
	require.NotEmpty(t, gitStdoutTest(t, wt, "stash", "create", "both levels"),
		"create records the whole worktree, not just the cwd")

	// gitStdoutTest trims, so porcelain's leading unstaged column reads as "M
	// a.txt" here. One line, so this also pins that the sub/ edit WAS cleaned —
	// the bare pathspec cleans what is under the cwd and nothing above it.
	runGitInPlaceTest(t, sub, "checkout", "--", ".")
	assert.Equal(t, "M a.txt", gitStdoutTest(t, wt, "status", "--porcelain"),
		"`checkout -- .` from a subdirectory leaves the root edit behind")

	runGitInPlaceTest(t, sub, "checkout", "--", ":/")
	assert.Empty(t, gitStdoutTest(t, wt, "status", "--porcelain"),
		"`checkout -- :/` cleans from the repo root, which is what the recipe documents")
}
