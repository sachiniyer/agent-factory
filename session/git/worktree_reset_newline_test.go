package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newlineWorktreeRepo builds a throwaway repo with one linked worktree whose
// path contains a NEWLINE, and returns (repoRoot, worktreePath).
//
// A newline is legal in a POSIX path, and AF does not choose these paths: the
// worktree lives under the user's repo, so any newline anywhere in the parent
// chain is enough. Skips rather than fails where the filesystem will not accept
// one, so the suite stays honest about what it actually verified.
func newlineWorktreeRepo(t *testing.T) (string, string) {
	t.Helper()

	base := t.TempDir()
	repoRoot := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0o755))

	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := git("init", "-q", "-b", "master", ".")
	require.NoError(t, err, out)
	out, err = git("commit", "-q", "--allow-empty", "-m", "initial")
	require.NoError(t, err, out)

	worktreePath := filepath.Join(base, "wt\nbroken")
	if out, err := git("worktree", "add", "-q", "-b", "nl", worktreePath); err != nil {
		t.Skipf("this filesystem will not hold a path containing a newline: %v: %s", err, out)
	}
	return repoRoot, worktreePath
}

// TestRemoveWorktreeDir_LockedWorktreeWithNewlineInPathIsNotDeleted is the
// #3423 regression, and it is a DATA-LOSS test: the assertion that matters is
// that the directory is still on disk at the end.
//
// It is TestRemoveWorktreeDir_LockedWorktreeIsReportedAsIncomplete (#2110) with
// one byte changed. That test proves a locked worktree survives `af reset`,
// because the ownership probe reports it as still registered and AF refuses to
// delete what git still owns. Put a newline in the path and the very same setup
// loses the directory instead — the guard does not weaken, it inverts.
//
// The mechanism is the parse, not the lock. `git worktree list --porcelain`
// prints the path VERBATIM and unquoted (measured on git 2.43), so a
// newline-terminated reader splits one entry into two fragments, neither of
// which equals the target. worktreeListed then answers "not registered", which
// mayDeleteWorktreeDir reads as "git has let go of this path, it is ours to
// remove" — and os.RemoveAll takes the locked worktree, leaving the
// registration behind to block the branch delete forever.
//
// The lock is here only because it is the realistic way to reach the ownership
// gate at all: it makes `git worktree remove -f` fail, which is what sends
// RemoveWorktreeDir down the probe-then-delete path.
func TestRemoveWorktreeDir_LockedWorktreeWithNewlineInPathIsNotDeleted(t *testing.T) {
	repoRoot, worktreePath := newlineWorktreeRepo(t)

	lock := exec.Command("git", "-C", repoRoot, "worktree", "lock", worktreePath, "--reason", "still in use")
	out, err := lock.CombinedOutput()
	require.NoError(t, err, string(out))

	_, err = RemoveWorktreeDir(repoRoot, worktreePath)

	// THE assertion. Everything below explains this one.
	if _, statErr := os.Stat(worktreePath); statErr != nil {
		t.Fatalf("RemoveWorktreeDir DELETED a locked, still-registered git worktree because its path "+
			"contains a newline (#3423): %v\n"+
			"The same test without the newline (#2110) proves the directory must survive. The "+
			"registration is now orphaned and its branch is permanently undeletable.", statErr)
	}

	require.Error(t, err,
		"a locked worktree could not be removed, so RemoveWorktreeDir must report it")
	require.ErrorIs(t, err, ErrWorktreeStillRegistered,
		"the failure must be classifiable so reset PRESERVES the session record for a re-run")
	assert.Contains(t, err.Error(), "worktree unlock",
		"the error must name the actual recovery command")

	// git's own view: the registration is intact, which is exactly why the
	// directory was never ours to delete.
	list := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain", "-z")
	listOut, listErr := list.Output()
	require.NoError(t, listErr)
	assert.Contains(t, string(listOut), worktreePath,
		"sanity: git still registers the locked worktree at its newline-bearing path")
}

// TestWorktreeRegisteredIn_FindsNewlineBearingPath isolates the probe from the
// removal, so a regression is reported as "the probe lied" rather than only as
// a deleted directory.
func TestWorktreeRegisteredIn_FindsNewlineBearingPath(t *testing.T) {
	repoRoot, worktreePath := newlineWorktreeRepo(t)

	registered, err := worktreeRegisteredIn(repoRoot, worktreePath)
	require.NoError(t, err, "the listing is readable; the probe must not error")
	assert.True(t, registered,
		"git registers this worktree, so the probe must say so — answering \"not registered\" is "+
			"what authorizes deletion (#3423)")

	// The negative case must still work: an unregistered sibling is absent even
	// though a newline-bearing entry is present in the same listing.
	absent := filepath.Join(filepath.Dir(worktreePath), "never\nadded")
	registered, err = worktreeRegisteredIn(repoRoot, absent)
	require.NoError(t, err)
	assert.False(t, registered, "a path git does not register must not be reported as registered")
}

// TestWorktreeListed_UnreadableListingIsAnErrorNotAnAbsence pins the second
// half of #3423, which is the more dangerous half.
//
// worktreeListed feeds mayDeleteWorktreeDir, which reads "not registered" as
// "git has let go of this path, so it is ours to os.RemoveAll". A bool cannot
// say "I could not read that", so every unreadable listing was answering
// "absent" with full confidence — failing OPEN on the one guard that protects
// git-owned paths from deletion. That is the #3476/#3479 class: absence is a
// claim about data you actually read.
//
// Each unreadable case below must therefore NOT return (false, nil). Returning
// false with no error is the exact shape that authorizes the delete.
func TestWorktreeListed_UnreadableListingIsAnErrorNotAnAbsence(t *testing.T) {
	const target = "/repo/wt"

	unreadable := []struct {
		name, porcelain string
	}{
		{
			name:      "empty",
			porcelain: "",
		},
		{
			// The pre-#3423 format. It fails CLOSED now rather than silently
			// reverting to the newline parse this fix removed.
			name:      "newline-delimited output (no -z)",
			porcelain: "worktree /repo/wt\nHEAD abc\nbranch refs/heads/x\n\n",
		},
		{
			// Cut mid-stream: the last entry is a partial path, which compares
			// unequal to everything and fabricates an absence.
			name:      "truncated: no trailing NUL",
			porcelain: "worktree /repo/other\x00HEAD abc\x00\x00worktree /repo/w",
		},
		{
			// #3523 review. The subtle one: cut immediately after a COMPLETE
			// field, so the output does end with a NUL. A check that only
			// required one trailing NUL accepted this, and every entry after
			// the cut — including the target — simply was not there to find.
			name:      "truncated: ends with a field terminator, not an entry terminator",
			porcelain: "worktree /repo/other\x00HEAD abc\x00",
		},
		{
			// The same shape one field earlier, to pin that this is about the
			// entry terminator and not about which attribute happened to be last.
			name:      "truncated: complete first entry, second entry cut after its path",
			porcelain: "worktree /repo/other\x00HEAD abc\x00\x00worktree /repo/second\x00",
		},
		{
			name:      "not a worktree listing (no leading worktree record)",
			porcelain: "HEAD abc\x00branch refs/heads/x\x00\x00",
		},
	}
	for _, tc := range unreadable {
		t.Run(tc.name, func(t *testing.T) {
			listed, err := worktreeListed(tc.porcelain, target)
			require.Error(t, err,
				"an unreadable listing must be reported, not answered: (false, nil) is what "+
					"mayDeleteWorktreeDir reads as permission to delete a git-owned path (#3423)")
			assert.False(t, listed, "no listing was read, so nothing may be claimed as found")
		})
	}

	// The readable cases still answer, or the guard would refuse everything.
	t.Run("well-formed: target present", func(t *testing.T) {
		listed, err := worktreeListed("worktree /repo/other\x00\x00worktree /repo/wt\x00branch refs/heads/x\x00\x00", target)
		require.NoError(t, err)
		assert.True(t, listed)
	})
	t.Run("well-formed: target absent", func(t *testing.T) {
		listed, err := worktreeListed("worktree /repo/other\x00branch refs/heads/x\x00\x00", target)
		require.NoError(t, err, "a complete listing that simply lacks the path is a real answer")
		assert.False(t, listed)
	})
	t.Run("well-formed: newline-bearing path is recovered exactly", func(t *testing.T) {
		listed, err := worktreeListed("worktree /repo/wt\nbroken\x00branch refs/heads/x\x00\x00", "/repo/wt\nbroken")
		require.NoError(t, err)
		assert.True(t, listed, "-z carries the newline through intact")
	})
}

// truncatedWorktreeListGitOnPath puts a `git` earlier on PATH that returns a
// TRUNCATED `worktree list` and delegates every other subcommand to the real
// git. Selective, for the same reason stallGitSubcommand is: the test still
// needs real `worktree lock`, `remove` and `prune` to reach the ownership gate.
//
// The payload is written from Go rather than built in the shell because it
// contains NUL bytes, which command substitution strips.
func truncatedWorktreeListGitOnPath(t *testing.T, worktreePath string) {
	t.Helper()
	realGit := realGitPath(t)
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	// A real first entry, then an entry cut mid-path: no trailing NUL.
	require.NoError(t, os.WriteFile(payload,
		[]byte("worktree /somewhere/else\x00HEAD abc\x00\x00worktree "+worktreePath[:len(worktreePath)-3]), 0o644))
	script := fmt.Sprintf(`#!/bin/sh
m1=0
m2=0
for a in "$@"; do
  if [ "$a" = "worktree" ]; then m1=1; fi
  if [ "$a" = "list" ]; then m2=1; fi
done
if [ "$m1" = 1 ] && [ "$m2" = 1 ]; then
  exec cat %q
fi
exec %q "$@"
`, payload, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRemoveWorktreeDir_UnreadableListingDoesNotAuthorizeDeletion is the
// end-to-end half: it proves the fail-open is closed in production, not just in
// worktreeListed's own return values.
//
// A truncated listing tells us nothing about whether git still owns this path.
// Before #3423 that produced a confident "not registered" and the directory was
// deleted; now the probe reports the failed read, the ownership gate refuses,
// and the record is retained so a re-run can finish the job.
func TestRemoveWorktreeDir_UnreadableListingDoesNotAuthorizeDeletion(t *testing.T) {
	repoRoot, worktreePath, _ := resetRepoWithWorktree(t, "unreadable")

	// Lock it so `worktree remove -f` fails and the ownership gate is reached at
	// all — the same staging the #2110 test uses.
	lock := exec.Command("git", "-C", repoRoot, "worktree", "lock", worktreePath, "--reason", "still in use")
	out, err := lock.CombinedOutput()
	require.NoError(t, err, string(out))

	truncatedWorktreeListGitOnPath(t, worktreePath)

	_, err = RemoveWorktreeDir(repoRoot, worktreePath)

	if _, statErr := os.Stat(worktreePath); statErr != nil {
		t.Fatalf("RemoveWorktreeDir deleted a worktree directory on the strength of a listing it "+
			"could not read (#3423): %v\nAbsence has to be read out of data, not out of a failed parse.", statErr)
	}
	require.Error(t, err, "an unreadable registration probe must be reported, not treated as success")
	require.ErrorIs(t, err, ErrWorktreeStillRegistered,
		"reset must RETAIN the session record when it could not determine who owns the path")
}
