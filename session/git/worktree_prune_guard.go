package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// Deletion-boundary guards for `af sessions prune` (#5136 review). Prune
// deletes an archived worktree by the RECORD's pathname, which is not
// identity: an out-of-band move frees the path for an unrelated occupant, and
// a same-repo replacement survives even the bidirectional pointer binding —
// it points into the right metadata root and its registration answers back.
// These helpers add the checks the first cut lacked:
//
//   - the occupant's registered BRANCH must be the session's recorded branch
//     (repo-present mode), and
//   - the pointer's repository half must name the RECORDED origin (repo-gone
//     mode), not merely carry an absent linked-worktree shape.
//
// The third helper answers the question rm -rf cannot take back: whether the
// worktree still holds uncommitted content — the only copy of that work,
// which the tombstone's kept-branch promise does not cover.

// WorktreeDirtyFiles runs a bounded `git status --porcelain
// --untracked-files=normal` inside worktreePath and returns how many
// porcelain entries the tree carries. `normal` is the flag SnapshotAndPushBranch
// trusts for exactly this question (#2101): bare --porcelain honors
// status.showUntrackedFiles, and a worktree shares .git/config with its
// origin, so a user who hides untracked files would otherwise read a dirty
// tree as clean. Any error — a dead gitdir, a stalled mount — is returned, so
// the caller fails closed instead of treating "unknown" as "clean".
func WorktreeDirtyFiles(worktreePath string) (int, error) {
	out, err := runBoundedWorktreeGit(worktreePath, false, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return 0, err
	}
	return countNonEmptyLines(string(out)), nil
}

// VerifyRegisteredWorktreeOccupantBranch is VerifyRegisteredWorktreeOccupant
// plus the session-identity half that check lacks: the registration binding
// proves the occupant belongs to repoPath, and this additionally requires the
// worktree's registered branch to be expectedBranch — the session record's
// own branch. A same-repo worktree parked at a recycled pathname binds to the
// right metadata but is on a different branch, and prune must refuse it:
// deleting it would destroy another session's checkout (#5136 Codex round 2).
// An empty expectedBranch means the record carries no branch to bind; the
// repo-level binding alone then decides, as before.
func VerifyRegisteredWorktreeOccupantBranch(worktreePath, repoPath, expectedBranch string) error {
	if err := VerifyRegisteredWorktreeOccupant(worktreePath, repoPath); err != nil {
		return err
	}
	expectedBranch = strings.TrimSpace(expectedBranch)
	if expectedBranch == "" {
		return nil
	}
	out, err := runBoundedWorktreeGit(repoPath, false, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return fmt.Errorf("could not list %s's worktrees to bind the occupant's branch: %w", repoPath, err)
	}
	branch, matched, err := worktreeListedBranchBounded(string(out), worktreePath)
	if err != nil {
		return err
	}
	if !matched {
		return worktreeIdentityMismatchf(
			"worktree %s binds into %s's metadata but is not in its registration listing — the occupant cannot be matched to a worktree record",
			worktreePath, repoPath)
	}
	if branch != "refs/heads/"+expectedBranch {
		return worktreeIdentityMismatchf(
			"registered occupant of %s is on branch %s, not this session's %s — the occupant belongs to a different session",
			worktreePath, strings.TrimPrefix(branch, "refs/heads/"), expectedBranch)
	}
	return nil
}

// VerifyArchivedWorktreePointerForRepo is VerifyArchivedWorktreePointer plus
// an origin binding for the repo-gone mode: the occupant's `.git` gitdir must
// name a linked-worktree leaf INSIDE the recorded origin's metadata
// (<repoPath>/.git/worktrees/<leaf>, or <repoPath>/worktrees/<leaf> for a bare
// layout), not merely carry the absent-leaf shape of any deleted repository's
// worktree. Without it a foreign worktree orphaned by the deletion of ITS OWN
// origin satisfies the check and gets deleted by this session's say-so.
//
// The comparison is textual-through-ResolveForCompare because the origin is
// gone: only the deepest surviving ancestors resolve, and both sides degrade
// to cleaned paths identically. It cannot bind leaf name to session title —
// git names the leaf after the worktree's basename at creation and af moves
// the directory at archive — so this stays a repo binding layered on the
// linked-worktree shape, strictly stronger than the shape alone.
func VerifyArchivedWorktreePointerForRepo(worktreePath, recordedRepoPath string) error {
	return boundedPointerCheck("archivedrepo\x00"+recordedRepoPath, worktreePath, func(path string) error {
		target, err := verifyWorktreePointerShape(path)
		if err != nil {
			return err
		}
		metadataDir := filepath.Dir(filepath.Dir(target))
		recorded := pathutil.ResolveForCompare(recordedRepoPath)
		resolved := pathutil.ResolveForCompare(metadataDir)
		if resolved != recorded &&
			resolved != pathutil.ResolveForCompare(filepath.Join(recordedRepoPath, ".git")) {
			return worktreeIdentityMismatchf(
				"archived worktree pointer %s names metadata under %s, not the recorded origin %s — the occupant belongs to a different repository",
				filepath.Join(path, ".git"), metadataDir, recordedRepoPath)
		}
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf(
				"archived worktree pointer %s names gitdir %s, which still exists — the occupant belongs to a live repository, or the origin kept a separate git dir that outlived it; run a restore first: a failed repo-gone restore installs the cleanup authorization kill needs",
				filepath.Join(path, ".git"), target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"archived worktree pointer %s: could not establish the state of gitdir %s: %w",
				filepath.Join(path, ".git"), target, err)
		}
		return nil
	})
}
