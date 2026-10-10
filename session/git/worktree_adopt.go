package git

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// A bare ENOENT from ClaimRelocationSource normally means the worktree was
// deleted outside af (#5102) — but not always. Archive moves the worktree first
// and persists the new path second, and a daemon that dies between
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
// checked out, if anywhere. git's registration follows any `git worktree move`,
// af's or the user's, so it is how archive and rename tell a worktree that was
// moved from one that was deleted. listed is false when no worktree has the
// branch checked out; an error means the listing could not be read completely
// and nothing may be concluded.
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

// adoptedWorktreeIdentity pins the directory AdoptLandedWorktreeMove proved.
type adoptedWorktreeIdentity struct {
	path     string
	identity pathIdentity
}

// AdoptLandedWorktreeMove re-aims the record at dest after proving a previous
// archive or restore move landed there (see VerifyLandedMove). It never touches
// the bytes at either path. It refuses while any relocation claim or recovery
// record is outstanding, when the recorded path is no longer conclusively
// absent, and when dest no longer exists. Those are re-checked under
// relocationMu after the proof, which spawns git and so runs unlocked, and the
// recorded path must be unchanged across the two, so nothing is adopted on a
// stale answer.
//
// The proof is bound to one directory, not to a name: dest's device and inode
// are captured before the proof runs and must be unchanged when the record is
// re-aimed, so a directory swapped in while git was being asked is refused. The
// captured identity is kept for ReconfirmAdoptedWorktree, because the caller
// still has editor, hook and tmux teardown to run before it commits, and a name
// can be swapped under it again in that window.
func (g *GitWorktree) AdoptLandedWorktreeMove(dest string) error {
	before := g.GetWorktreePath()
	if before == dest {
		return nil
	}
	proved, err := boundedRelocationPathIdentity(dest)
	if err != nil {
		return fmt.Errorf("cannot adopt %s: %w", dest, err)
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
	if err := requireSameDirectory(dest, proved); err != nil {
		return fmt.Errorf("cannot adopt %s: %w", dest, err)
	}
	g.setWorktreeLocationLocked(dest)
	g.adoptedWorktree = &adoptedWorktreeIdentity{path: dest, identity: proved}
	return nil
}

// ReconfirmAdoptedWorktree checks that the directory AdoptLandedWorktreeMove
// proved is still the one at the recorded path — device and inode equal to what
// was proven, not merely something existing under that name.
//
// A vanished directory returns an error wrapping os.ErrNotExist: nothing is at
// the path, so there is nothing to protect. A different directory at the path,
// or a check that cannot be answered (a stalled mount), is worse: the record
// names a path that may hold unrelated files, and a recovery that starts an
// agent there or a cleanup that removes it would act on them. So those install a
// claim_stale recovery record carrying the PROVEN identity before returning an
// error joined with ErrRelocateStateUnknown. That record is what makes respawn
// refuse to start an agent and cleanup refuse to delete until a retry
// re-establishes which directory is the session's.
func (g *GitWorktree) ReconfirmAdoptedWorktree() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	adopted := g.adoptedWorktree
	current := g.worktreePath
	if adopted == nil || adopted.path != current {
		return fmt.Errorf("no adopted worktree identity is recorded for %s", current)
	}
	err := requireSameDirectory(current, adopted.identity)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return err
	}
	if g.relocationRecovery == nil {
		g.recordStaleClaimLocked(RelocationClaim{Path: current, identity: adopted.identity})
	}
	g.adoptedWorktree = nil
	return errors.Join(err, ErrRelocateStateUnknown)
}

func requireSameDirectory(path string, want pathIdentity) error {
	got, err := boundedRelocationPathIdentity(path)
	if err != nil {
		return err
	}
	if !got.same(want) {
		return fmt.Errorf("%s is no longer the directory that was proven to be this session's worktree (device/inode changed)", path)
	}
	return nil
}

// FenceUnverifiedWorktree records that the directory now at the recorded path
// has not been verified as this session's worktree: an identity-unknown
// stalled record, the same fence a timed-out relocation probe installs (#5102).
// Respawn and destructive cleanup refuse while it stands; the next archive or
// restore claim re-resolves the path, and the record settles by itself if the
// path turns out absent after all. A record already present is kept — it is
// at least as current. The returned error joins cause with
// ErrRelocateStateUnknown so callers persist the fence before reporting.
func (g *GitWorktree) FenceUnverifiedWorktree(cause error) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery == nil && g.activeRelocationClaim == nil {
		g.relocationRecovery = &RelocationRecovery{State: RelocationRecoveryStalled}
	}
	g.adoptedWorktree = nil
	return errors.Join(cause, ErrRelocateStateUnknown)
}

// ReturnClaimUnverified gives back a claim that resolved to a directory the
// caller refuses to treat as the session's worktree (#5102) — an archive of a
// row flagged worktree-missing that found something back at the path. The
// ordinary PreserveRelocationClaim would record that directory's identity as
// the worktree's, which is precisely the claim being refused, and a later
// removal of the directory would then read as a vanished worktree identity and
// strand the row. Instead a claim that consumed a record goes back to the
// identity-unknown stalled fence it came from, which settles on its own once the
// path is conclusively absent; a record-free claim owns nothing to give back.
func (g *GitWorktree) ReturnClaimUnverified(claim RelocationClaim) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	g.releaseRelocationClaimLocked(&claim)
	if claim.recoveryOwned && g.relocationRecovery == nil {
		g.relocationRecovery = &RelocationRecovery{State: RelocationRecoveryStalled}
	}
}
