package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Regression tests for the branchCommitWarning false positive: it decided
// which commits `git branch -D` would orphan with `git log base..branch --not
// --remotes`, which excludes ONLY remote-tracking refs. But `git branch -D
// <session>` deletes only refs/heads/<session>; a commit also reachable from a
// local tag, another local branch, or refs/stash survives. The warning
// reported such commits as "permanently deletes … this cannot be undone" and
// forced the deliberate "k" confirm key, when in fact nothing is lost.
//
// The sibling detachedHeadCommitWarning already accounts for these durable
// refs via for-each-ref --contains + refSurvivesWorktreeCleanup; these tests
// pin the branch half to the same durability notion and guard the genuine-loss
// direction so the fix cannot over-suppress.
// ----------------------------------------------------------------------------

// killGitRaw runs a git subcommand in dir and returns its trimmed combined
// output and error. Unlike killGit it does NOT fail the test on a non-zero
// exit, so callers can assert on the error (e.g. rev-parse a ref that should
// be gone).
func killGitRaw(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// branchSurvivorCase sets up a session branch carrying one local-only commit
// whose tip is ALSO held by addSurvivor — a ref that survives `git branch -D`.
// It asserts the branch path is silent (nothing is actually lost), then runs
// the real cleanup and confirms the survivor ref still reaches the tip, i.e.
// the warning was right to stay silent.
func branchSurvivorCase(t *testing.T, name, branch, survivorRef string, addSurvivor func(t *testing.T, repoDir, wt, tip string)) {
	t.Helper()
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, branch)
	commitInWorktree(t, wt)
	tip := killGit(t, wt, "rev-parse", "HEAD")
	addSurvivor(t, repoDir, wt, tip)

	// Precondition: the worktree is clean, so killConfirmationWarning (the
	// dirty-worktree check) is silent — the survivor check is the ONLY thing
	// that should suppress the loud line.
	require.Empty(t, killGit(t, wt, "status", "--porcelain"),
		"%s: worktree must be clean (only committed work at risk)", name)

	// The branch path must NOT warn: the commit survives branch -D via the
	// durable ref. Both the predicate half and the assembled unmerged warning
	// must be non-severe and empty — no false "permanently deletes" line and
	// no escalation to the deliberate confirm key.
	bline, bsevere := branchCommitWarning(wt, branch, baseSHA)
	assert.Falsef(t, bsevere, "%s: branchCommitWarning must be non-severe when a durable ref holds the tip (%s)", name, tip)
	assert.Emptyf(t, bline, "%s: branchCommitWarning must emit no warning line when a durable ref holds the tip", name)

	uline, usevere := unmergedCommitWarning(wt, branch, baseSHA, true)
	assert.Falsef(t, usevere, "%s: unmergedCommitWarning must be non-severe when a durable ref holds the tip", name)
	assert.Emptyf(t, uline, "%s: unmergedCommitWarning must emit no warning when a durable ref holds the tip", name)

	// Reality check: the real cleanup sequence orphans nothing. Run the
	// exact steps session/git runs (worktree remove --force, then branch -D)
	// and confirm the survivor ref still reaches the tip. A durable ref that
	// still resolves is the real durability signal (not cat-file -e, which
	// succeeds for any loose object until gc whether or not a ref survives).
	killGit(t, repoDir, "worktree", "remove", "--force", wt)
	killGit(t, repoDir, "branch", "-D", branch)
	if _, err := killGitRaw(t, repoDir, "rev-parse", "--verify", "--quiet", survivorRef); err != nil {
		t.Fatalf("%s: AFTER cleanup, durable ref %s must still resolve (it preserves the tip): %v",
			name, survivorRef, err)
	}
}

// TestBranchCommitWarning_DurableRefSuppressesFalsePositive covers the four
// classes of local ref that survive `git branch -D`: a lightweight tag, an
// annotated tag, another local branch, and refs/stash. All four reproduced the
// false-positive severe line before the fix.
func TestBranchCommitWarning_DurableRefSuppressesFalsePositive(t *testing.T) {
	t.Run("tag", func(t *testing.T) {
		branchSurvivorCase(t, "tag", "dev/tag", "refs/tags/keep-tag", func(t *testing.T, repoDir, wt, tip string) {
			killGit(t, wt, "tag", "keep-tag", "HEAD")
		})
	})
	t.Run("annotated_tag", func(t *testing.T) {
		branchSurvivorCase(t, "annotated_tag", "dev/annot", "refs/tags/keep-annot", func(t *testing.T, repoDir, wt, tip string) {
			killGit(t, wt, "tag", "-a", "-m", "annot", "keep-annot", "HEAD")
		})
	})
	t.Run("other_local_branch", func(t *testing.T) {
		branchSurvivorCase(t, "other_local_branch", "dev/other", "refs/heads/keep-branch", func(t *testing.T, repoDir, wt, tip string) {
			killGit(t, repoDir, "branch", "keep-branch", tip)
		})
	})
	t.Run("stash", func(t *testing.T) {
		branchSurvivorCase(t, "stash", "dev/stash", "refs/stash", func(t *testing.T, repoDir, wt, tip string) {
			// `git stash` in a linked worktree writes refs/stash to the common
			// dir, so it survives `git worktree remove --force`. The stash's
			// first parent is the session branch tip, so the tip stays
			// reachable from refs/stash after branch -D.
			require.NoError(t, os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("wip\n"), 0o644))
			killGit(t, wt, "stash", "--include-untracked", "-q")
		})
	})
}

// TestBranchCommitWarning_SessionBranchOnlyIsStillSevere is the genuine-loss
// guard: a commit held ONLY by the session branch (no durable survivor) must
// STILL escalate. The fix excludes only refs that survive cleanup, so it must
// not suppress the warning for the very commit it is supposed to protect.
func TestBranchCommitWarning_SessionBranchOnlyIsStillSevere(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/lonely")
	commitInWorktree(t, wt)
	require.Empty(t, killGit(t, wt, "status", "--porcelain"), "worktree must be clean")
	// No other ref holds the tip: the only ref containing HEAD is the session
	// branch itself, which cleanup deletes (so it is non-durable). The base
	// branch does not contain the new commit (it is its parent), so it is not
	// listed.
	refsContaining := strings.TrimSpace(killGit(t, wt, "for-each-ref", "--contains=HEAD", "--format=%(refname)"))
	require.Equal(t, "refs/heads/dev/lonely", refsContaining,
		"precondition: only the session branch (which cleanup deletes) may hold the tip")

	line, severe := branchCommitWarning(wt, "dev/lonely", baseSHA)
	assert.True(t, severe, "a commit held only by the session branch must remain severe")
	assert.Contains(t, line, "1 commit")
	assert.Contains(t, line, "permanently deletes")
	assert.Contains(t, line, "cannot be undone")

	uline, usevere := unmergedCommitWarning(wt, "dev/lonely", baseSHA, true)
	assert.True(t, usevere)
	assert.Contains(t, uline, "permanently deletes")
}

// TestHandleKill_LocalSurvivorRef_KeepsOrdinaryConfirm wires the fix through
// the rendered kill confirmation: a session whose branch tip is preserved by
// a local tag must keep the ordinary "y" confirm and must NOT print the
// "permanently deletes" consequence, because no work is actually lost. This
// pins the handleKill wiring the predicate-level test above does not reach.
func TestHandleKill_LocalSurvivorRef_KeepsOrdinaryConfirm(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/survivor")
	commitInWorktree(t, wt)
	killGit(t, wt, "tag", "keep-tag", "HEAD") // durable local ref holds the tip
	require.Empty(t, killGit(t, wt, "status", "--porcelain"), "worktree must be clean")

	inst := startedWorktreeInstance(t, "survivor", repoDir, wt, "dev/survivor", baseSHA)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.NotContains(t, rendered, "permanently deletes",
		"a tip held by a local tag survives branch -D — no permanence warning")
	assert.NotContains(t, rendered, "cannot be undone")
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey,
		"a session branch whose tip survives cleanup must keep the ordinary 'y' confirm")
}

// TestHandleKill_DroppedStashIsStillSevere guards the other direction of the
// stash case: a stash that has been dropped before kill no longer preserves
// the tip, so the commit is genuinely at risk and the warning must fire. A
// dropped stash does not appear in for-each-ref --contains, so the fix stays
// severe — proving it keys off --contains (current refs) rather than the
// stash reflog, exactly the over-suppression footgun the fix must not open.
func TestHandleKill_DroppedStashIsStillSevere(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/droppedstash")
	commitInWorktree(t, wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("wip\n"), 0o644))
	killGit(t, wt, "stash", "--include-untracked", "-q") // tip now reachable from refs/stash
	killGit(t, wt, "stash", "drop")                      // drops stash@{0}; tip no longer preserved
	require.Empty(t, killGit(t, wt, "status", "--porcelain"), "worktree must be clean after drop")

	_, err := killGitRaw(t, repoDir, "rev-parse", "--verify", "--quiet", "refs/stash")
	require.Error(t, err, "precondition: refs/stash must be gone after `git stash drop`")

	inst := startedWorktreeInstance(t, "droppedstash", repoDir, wt, "dev/droppedstash", baseSHA)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "1 commit", "the dropped-stash commit is genuinely at risk")
	assert.Contains(t, rendered, "permanently deletes")
	assert.Contains(t, rendered, "cannot be undone")
	assert.Equal(t, unmergedKillConfirmKey, hm.confirmationOverlay.ConfirmKey,
		"a genuinely-lost commit must still escalate the confirm key")
}
