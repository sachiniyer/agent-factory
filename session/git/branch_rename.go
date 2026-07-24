package git

import (
	"fmt"
	"strings"
)

// Branch rename for archived-name reuse (#2127).
//
// Archiving relocates a worktree rather than removing it (#2013), so an archived
// session keeps its branch CHECKED OUT. Freeing its title for a new session
// therefore frees a name but not a branch, and the create the rename enabled then
// failed at `git worktree add` on the branch the archived session still held.
//
// The fix moves the branch aside WITH the title, so the reclaim is complete. That
// is only possible because git no longer refuses to rename a branch checked out in
// a linked worktree, and updates that worktree's HEAD to follow — a premise
// TestGitRenamesBranchCheckedOutInLinkedWorktree asks git directly rather than
// assuming, since the whole design collapses without it.

// RenameBranch renames this worktree's branch, in place, keeping the worktree
// attached to it. The recorded branch name moves with it, so the caller's
// persisted record and git agree afterwards.
//
// A no-op rename (same name) is accepted rather than handed to git, which errors
// on it — the caller derives the new name from a title and can legitimately land
// on the one it already has.
func (g *GitWorktree) RenameBranch(newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("cannot rename branch %q: the new name is empty", g.branchName)
	}
	if newName == g.branchName {
		return nil
	}
	if g.branchName == "" {
		return fmt.Errorf("cannot rename to %q: this worktree has no recorded branch", newName)
	}
	// Run from the REPO, not the worktree: the worktree's HEAD is the thing being
	// renamed, and the repo is the one path guaranteed to exist whether or not the
	// archived worktree is currently mounted.
	if _, err := g.runGitCommand(g.repoPath, "branch", "-m", g.branchName, newName); err != nil {
		return fmt.Errorf("failed to rename branch %q to %q: %w", g.branchName, newName, err)
	}
	g.branchName = newName
	return nil
}

// BranchIsPublished reports whether this worktree's branch has an upstream — it
// has been pushed, and may carry an open PR.
//
// Tri-state on purpose (published, known). A rename desynchronizes a published
// branch's local name from the remote it tracks, so it is the one case where
// moving the branch aside is worse than refusing, and callers must be able to tell
// "definitely local" from "could not ask". An unanswerable probe MUST NOT be read
// as "local": that is the fabricated-negative shape this repo keeps paying for —
// a probe that cannot know, answering anyway, and the fake answer authorizing the
// riskier action.
func (g *GitWorktree) BranchIsPublished() (published bool, known bool) {
	if g.branchName == "" {
		return false, false
	}
	// Deliberately not `rev-parse <branch>@{upstream}`: that exits non-zero BOTH
	// when the branch has no upstream and when it does not exist, so a missing
	// branch would read as the confident answer "local, safe to rename".
	//
	// for-each-ref separates them. The format always emits the ref's own name, so
	// an ABSENT branch prints nothing at all while a branch with no upstream
	// prints "<name>|" — "no rows" and "a row with an empty upstream" are then
	// different observations rather than the same empty string.
	out, err := g.runGitCommand(g.repoPath, "for-each-ref",
		"--format=%(refname:short)|%(upstream:short)", "refs/heads/"+g.branchName)
	if err != nil {
		return false, false
	}
	row := strings.TrimSpace(out)
	if row == "" {
		// The branch is not there. Nothing true can be said about its upstream.
		return false, false
	}
	_, upstream, ok := strings.Cut(row, "|")
	if !ok {
		return false, false
	}
	return strings.TrimSpace(upstream) != "", true
}
