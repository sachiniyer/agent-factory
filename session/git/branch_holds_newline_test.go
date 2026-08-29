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

// newlineHoldRepo builds a repo with two linked worktrees whose paths contain a
// newline: one where the newline is INSIDE the path and one where the path ENDS
// with it. Both are legal POSIX paths, and AF does not choose them — the
// worktree lives under the user's repo, so a newline anywhere in the parent
// chain is enough.
//
// Returns the repo root and the two worktree paths. Skips where the filesystem
// will not hold such a name, so the suite stays honest about what it verified.
func newlineHoldRepo(t *testing.T) (repoRoot, midPath, endPath string) {
	t.Helper()

	base := t.TempDir()
	repoRoot = filepath.Join(base, "repo")
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

	midPath = filepath.Join(base, "mid\nnewline")
	if out, err := git("worktree", "add", "-q", "-b", "midbranch", midPath); err != nil {
		t.Skipf("this filesystem will not hold a path containing a newline: %v: %s", err, out)
	}
	// The path's LAST byte is the newline — the shape that makes the trailing
	// fragment read as a record separator.
	endPath = filepath.Join(base, "trailing") + "\n"
	if out, err := git("worktree", "add", "-q", "-b", "endbranch", endPath); err != nil {
		t.Skipf("this filesystem will not hold a path ending in a newline: %v: %s", err, out)
	}
	return repoRoot, midPath, endPath
}

// TestBranchesHeldByWorktrees_ReportsHoldsAtNewlineBearingPaths is the #3524
// regression, and the assertion that matters is that a branch which IS checked
// out is REPORTED as held.
//
// BranchesHeldByWorktrees is the authority the session-name resolver consults to
// decide whether an existing branch is reusable (#2091). Splitting
// `git worktree list --porcelain` on "\n" makes a path's own newline
// indistinguishable from a record terminator, so:
//
//   - a path ENDING in a newline leaves an empty trailing fragment, which the
//     parser reads as a record separator. It clears the current worktree path,
//     so the record's `branch` line is DROPPED and a held branch is reported as
//     free. That is the fail-open.
//   - a path with a newline INSIDE it records the hold under a TRUNCATED path,
//     so the map's value names a directory that does not exist — and that value
//     is what gets reported back to the user.
//
// git emits -z for exactly this, and the repo has required it since #3278.
func TestBranchesHeldByWorktrees_ReportsHoldsAtNewlineBearingPaths(t *testing.T) {
	repoRoot, midPath, endPath := newlineHoldRepo(t)

	held, err := BranchesHeldByWorktrees(repoRoot)
	require.NoError(t, err, "the listing is readable; this must not error")

	// THE fail-open assertion.
	holder, ok := held["endbranch"]
	require.True(t, ok,
		"endbranch IS checked out at a path ending in a newline, but was reported as NOT held (#3524). "+
			"The resolver would hand that branch to a new session, and `git worktree add` then refuses "+
			"with \"already used by worktree at …\" — the confusing failure #2091 exists to prevent.")
	assert.Equal(t, endPath, holder, "the hold must name the worktree that actually holds it")

	holder, ok = held["midbranch"]
	require.True(t, ok, "midbranch is checked out and must be reported as held")
	assert.Equal(t, midPath, holder,
		"a newline INSIDE the path must not truncate the recorded holder — this value is "+
			"reported back to the user")
}

// TestParseWorktreeBranchHolds_UnreadableListingIsAnErrorNotAnEmptyMap is the
// second half, and it is the one that generalises.
//
// The parser returned a bare map, so a listing it could only partly read came
// back looking exactly like a listing that genuinely held nothing — a type with
// no third state forces every read failure into the permissive answer. The
// caller (worktreeHeldBranchesLocked) already has a deliberate, safe response to
// "the probe could not answer": log it and proceed, letting `git worktree add`
// refuse loudly if the name really is held (#2127). It could just never be
// REACHED, because a partial parse raised nothing.
//
// So this is not about changing what the caller does on unknown. It is about
// making the parser ABLE to say "I could not read that" at all.
func TestParseWorktreeBranchHolds_UnreadableListingIsAnErrorNotAnEmptyMap(t *testing.T) {
	unreadable := []struct {
		name, porcelain string
	}{
		{name: "empty", porcelain: ""},
		{
			// The pre-#3524 format: it now fails CLOSED rather than silently
			// reverting to the newline parse this fix removes.
			name:      "newline-delimited output (no -z)",
			porcelain: "worktree /repo/wt\nHEAD abc\nbranch refs/heads/held\n\n",
		},
		{
			// Cut mid-stream. The dangerous shape: enough parsed to look like a
			// real answer, with the last hold missing.
			name:      "truncated: no trailing NUL",
			porcelain: "worktree /repo/a\x00branch refs/heads/one\x00\x00worktree /repo/b\x00branch refs/heads/t",
		},
		{
			// #3523 review, and it bites here for the same reason: cut after a
			// COMPLETE field, so the output does end with a NUL. The second
			// worktree's branch record is gone, so its hold would be reported
			// as free. Entries end with an EMPTY record, so a complete listing
			// ends with two NULs.
			name:      "truncated: ends with a field terminator, not an entry terminator",
			porcelain: "worktree /repo/a\x00branch refs/heads/one\x00\x00worktree /repo/b\x00",
		},
		{
			name:      "not a worktree listing (no leading worktree record)",
			porcelain: "branch refs/heads/one\x00\x00",
		},
	}
	for _, tc := range unreadable {
		t.Run(tc.name, func(t *testing.T) {
			holds, err := parseWorktreeBranchHolds(tc.porcelain)
			require.Error(t, err,
				"an unreadable listing must be reported, not returned as a map: a partial map is "+
					"indistinguishable from a complete one and silently answers \"not held\" (#3524)")
			assert.Nil(t, holds, "no listing was read, so no holds may be claimed")
		})
	}

	// The readable cases must still answer, or every create would lose the #2091
	// skip and fall back to failing at `worktree add`.
	t.Run("well-formed", func(t *testing.T) {
		holds, err := parseWorktreeBranchHolds(
			"worktree /repo/main\x00HEAD abc\x00branch refs/heads/master\x00\x00" +
				"worktree /repo/wt\nbroken\x00HEAD abc\x00branch refs/heads/held\x00\x00")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{
			"master": "/repo/main",
			"held":   "/repo/wt\nbroken",
		}, holds, "-z carries a newline-bearing path through intact")
	})
	t.Run("well-formed: detached and bare worktrees hold nothing", func(t *testing.T) {
		holds, err := parseWorktreeBranchHolds(
			"worktree /repo/b.git\x00bare\x00\x00worktree /repo/d\x00HEAD abc\x00detached\x00\x00")
		require.NoError(t, err)
		assert.Empty(t, holds, "a complete listing with no branch holds is a real answer")
	})
}

// truncatedListGitOnPath puts a `git` earlier on PATH that returns a TRUNCATED
// `worktree list` and delegates every other subcommand to the real git. The
// payload is written from Go because it contains NUL bytes, which command
// substitution strips.
func truncatedListGitOnPath(t *testing.T) {
	t.Helper()
	realGit := realGitPath(t)
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	require.NoError(t, os.WriteFile(payload,
		[]byte("worktree /somewhere\x00branch refs/heads/one\x00\x00worktree /cut/off/mid-pa"), 0o644))
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

// TestBranchesHeldByWorktrees_TruncatedListingErrors proves the fail-open is
// closed end-to-end and INDEPENDENTLY of the newline half: no newline appears
// anywhere here, only a listing cut mid-record.
func TestBranchesHeldByWorktrees_TruncatedListingErrors(t *testing.T) {
	base := t.TempDir()
	repoRoot := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0o755))
	init := exec.Command("git", "-C", repoRoot, "init", "-q", "-b", "master", ".")
	out, err := init.CombinedOutput()
	require.NoError(t, err, string(out))

	truncatedListGitOnPath(t)

	holds, err := BranchesHeldByWorktrees(repoRoot)
	require.Error(t, err,
		"a listing cut mid-record must be reported: returning the holds it managed to parse "+
			"presents a partial read as the complete set (#3524)")
	assert.Nil(t, holds, "\"I could not read the listing\" and \"nothing is held\" are different answers")
}
