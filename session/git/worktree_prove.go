package git

import (
	"errors"
	"fmt"
)

// A worktree-missing row (#5102) records that the directory af tracked was
// deleted, so a directory found at the recorded path afterwards is unverified
// by default: a mkdir, an unrelated checkout or a restore from backup all look
// the same as the session's worktree to a bare existence check. The primitives
// below are the one way such a directory re-qualifies, and they use the proof
// archive already trusts for a landed move (VerifyLandedMove): git registers
// the path as a worktree checked out on this session's branch, and the
// directory's linked-worktree pointer binds back into this repository. Only a
// `git worktree add` or `git worktree move` of the session's own branch —
// af's rebuild, or the user putting the checkout back — produces that.
//
// The proof spawns git, so it runs without relocationMu; the directory's
// identity is captured before it and re-checked under the lock after it, so a
// directory swapped in while git was being asked is never the one accepted.

// ProveRecordedWorktree proves that the directory at the recorded worktree
// path is this session's own worktree and pins its identity for
// ReconfirmAdoptedWorktree, which the caller runs at each later use boundary
// (a persist, an agent start). It refuses while any relocation claim or
// recovery record is outstanding — that owner decides where the worktree is —
// and when the recorded path changed while the proof ran.
//
// It is used where af itself is about to rely on the directory: a respawn of a
// flagged row whose own earlier rebuild left the directory behind, and a
// restore that just rebuilt the worktree, before either records or starts an
// agent in it.
func (g *GitWorktree) ProveRecordedWorktree() error {
	path := g.GetWorktreePath()
	if path == "" {
		return fmt.Errorf("cannot prove an empty worktree path")
	}
	proved, err := boundedRelocationPathIdentity(path)
	if err != nil {
		return fmt.Errorf("cannot prove %s is this session's worktree: %w", path, err)
	}
	if err := g.VerifyLandedMove(path); err != nil {
		return err
	}
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil || g.activeRelocationClaim != nil {
		return errors.Join(fmt.Errorf(
			"cannot prove %s: a worktree relocation is still unresolved", path,
		), ErrRelocateStateUnknown)
	}
	if g.worktreePath != path {
		return fmt.Errorf("cannot prove %s: the recorded worktree path changed to %s while it was being verified", path, g.worktreePath)
	}
	if err := requireSameDirectory(path, proved); err != nil {
		return fmt.Errorf("cannot prove %s: %w", path, err)
	}
	g.adoptedWorktree = &adoptedWorktreeIdentity{path: path, identity: proved}
	g.absentStallSettled = false
	return nil
}

// VerifyClaimedWorktree proves that the directory an active relocation claim
// resolved is this session's own worktree, for an archive or restore of a
// flagged row that found a directory back at the recorded path. The claim
// already captured that directory's identity; it must still be the active
// claim, at the recorded path, naming the same directory after the proof.
//
// It has no side effects. On an error the caller still owns the claim and
// hands it back unverified (ReturnClaimUnverified), so the refused directory
// is never recorded as the worktree's identity.
func (g *GitWorktree) VerifyClaimedWorktree(claim RelocationClaim) error {
	if claim.Path == "" {
		return fmt.Errorf("cannot verify an empty claimed worktree path")
	}
	if err := g.VerifyLandedMove(claim.Path); err != nil {
		return err
	}
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if claim.recoveryOwned && !g.ownsRelocationClaimLocked(claim) {
		return fmt.Errorf("cannot verify %s: the relocation claim is no longer the active owner", claim.Path)
	}
	if g.worktreePath != claim.Path {
		return fmt.Errorf("cannot verify %s: the recorded worktree path changed to %s while it was being verified", claim.Path, g.worktreePath)
	}
	identity, err := boundedRelocationPathIdentity(claim.Path)
	if err != nil {
		return fmt.Errorf("cannot verify %s: %w", claim.Path, err)
	}
	if !claim.identity.same(identity) {
		return fmt.Errorf("cannot verify %s: it is no longer the directory the claim resolved (device/inode changed)", claim.Path)
	}
	return nil
}
