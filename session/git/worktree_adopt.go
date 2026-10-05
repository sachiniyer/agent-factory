package git

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// A bare ENOENT from ClaimRelocationSource normally means the worktree was
// deleted outside af (#5102) — but not always. Archive and restore move the
// worktree first and persist the new path second, and a daemon that dies between
// the two leaves a durable record naming the vacated source while the bytes sit,
// fully registered with git, at the move's destination. No recovery record
// describes that window, so the claim cannot tell it apart from a deletion.
// Treating it as one would commit a row pointing at nothing and orphan the moved
// worktree. The primitives below let the daemon prove the move landed and adopt
// its destination instead.
//
// Proof is git's own registration plus the occupant's back-binding — the same
// evidence kill trusts before it may touch an archived checkout — never the mere
// existence of a directory at the expected name.

// VerifyLandedMove proves that dest holds this session's own worktree: git
// registers dest as a worktree checked out on this session's branch, and the
// directory at dest carries the linked-worktree pointer into this repository
// whose registration points back at it. nil means proven; any error means
// unproven — a different or detached branch, an unregistered occupant, a foreign
// repository's worktree, or a probe that could not answer.
func (g *GitWorktree) VerifyLandedMove(dest string) error {
	branch := strings.TrimSpace(g.GetBranchName())
	if branch == "" {
		return fmt.Errorf("cannot prove %s is this session's worktree: no branch is recorded for it", dest)
	}
	listing, err := g.boundedWorktreeListing()
	if err != nil {
		return err
	}
	listed, registered, err := worktreeListedBranchBounded(listing, dest)
	if err != nil {
		return fmt.Errorf("cannot read the worktree registration for %s: %w", dest, err)
	}
	if !registered {
		return fmt.Errorf("git does not register %s as a worktree", dest)
	}
	if want := "refs/heads/" + branch; listed != want {
		return fmt.Errorf("git registers %s on %q, not this session's branch %q", dest, listed, want)
	}
	if err := VerifyRegisteredWorktreeOccupant(dest, g.GetRepoPath()); err != nil {
		return fmt.Errorf("the directory at %s is not the worktree git registers there: %w", dest, err)
	}
	return nil
}

// RegisteredPathForBranch reports where git registers this session's branch as
// checked out, if anywhere. Restore uses it to find a landed move: its
// destination is not deterministic (an occupied candidate gets a collision
// suffix), but git's registration follows the move. listed is false when no
// worktree has the branch checked out; an error means the listing could not be
// read completely and nothing may be concluded.
func (g *GitWorktree) RegisteredPathForBranch() (path string, listed bool, err error) {
	branch := strings.TrimSpace(g.GetBranchName())
	if branch == "" {
		return "", false, nil
	}
	listing, err := g.boundedWorktreeListing()
	if err != nil {
		return "", false, err
	}
	want := "branch refs/heads/" + branch
	current := ""
	for _, field := range strings.Split(listing, "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			current = strings.TrimPrefix(field, "worktree ")
		case field == want && current != "":
			return current, true, nil
		}
	}
	return "", false, nil
}

func (g *GitWorktree) boundedWorktreeListing() (string, error) {
	out, err := runBoundedWorktreeGit(g.GetRepoPath(), false, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", fmt.Errorf("cannot list worktrees of %s: %w", g.GetRepoPath(), err)
	}
	listing := string(out)
	if err := requireCompleteWorktreeListing(listing); err != nil {
		return "", fmt.Errorf("cannot read the complete worktree listing of %s: %w", g.GetRepoPath(), err)
	}
	return listing, nil
}

// AdoptLandedWorktreeMove re-aims the record at dest after proving a previous
// archive or restore move landed there (see VerifyLandedMove). It never touches
// the bytes at either path. It refuses while any relocation claim or recovery
// record is outstanding, when the recorded path is no longer conclusively
// absent, and when dest no longer exists. Those are re-checked under
// relocationMu after the proof, which spawns git and so runs unlocked, and the
// recorded path must be unchanged across the two, so nothing is adopted on a
// stale answer.
func (g *GitWorktree) AdoptLandedWorktreeMove(dest string) error {
	before := g.GetWorktreePath()
	if before == dest {
		return nil
	}
	if err := g.VerifyLandedMove(dest); err != nil {
		return err
	}
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil || g.activeRelocationClaim != nil {
		return errors.Join(fmt.Errorf(
			"cannot adopt %s: a worktree relocation is still unresolved", dest,
		), ErrRelocateStateUnknown)
	}
	if g.worktreePath != before {
		return fmt.Errorf("cannot adopt %s: the recorded worktree path changed from %s to %s while it was being verified", dest, before, g.worktreePath)
	}
	if _, err := BoundedLstat(before); err == nil {
		return fmt.Errorf("cannot adopt %s: the recorded worktree %s exists again", dest, before)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot confirm recorded worktree %s absent: %w", before, err)
	}
	if _, err := BoundedLstat(dest); err != nil {
		return fmt.Errorf("cannot adopt %s: %w", dest, err)
	}
	g.setWorktreeLocationLocked(dest)
	return nil
}
