// The worktree-relocation CLAIM plumbing for archive and restore (#1028): the
// i.mu-guarded accessors that snapshot i.gitWorktree and delegate to the
// GitWorktree relocation-recovery API — consuming and preserving durable
// claims, the repo-gone finalization checkpoint, stalled-record settle and
// fence repair, and the pre-commit destruction-admission gate. Everything
// here follows the same shape: take i.mu (read-locked except where the call
// consumes the record), snapshot the worktree handle, then hand off to gw —
// so every read of i.gitWorktree stays under i.mu like the rest of the
// package. instance_backend.go owns the lifecycle operations that USE the
// claims (archive teardown, worktree restore, archived rename); this file
// owns the claim machinery itself.
package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
)

// ClaimWorktreeRelocationForRetry atomically consumes any durable recovery record
// and returns the point-in-time directory claim later archive steps must
// revalidate. Resolution never leaves a settled record behind for another reader
// to reinterpret.
func (i *Instance) ClaimWorktreeRelocationForRetry() (git.RelocationClaim, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.gitWorktree == nil {
		return git.RelocationClaim{}, fmt.Errorf("cannot resolve worktree relocation for %q: instance has no worktree", i.Title)
	}
	return i.gitWorktree.ClaimRelocationSource()
}

// PreserveWorktreeRelocationClaimForRetry returns ownership of a consumed
// recovery claim when an earlier archive gate aborts before the worktree use
// boundary. Claims made from record-free state are no-ops.
func (i *Instance) PreserveWorktreeRelocationClaimForRetry(claim git.RelocationClaim) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw != nil {
		gw.PreserveRelocationClaim(claim)
	}
}

// PreserveWorktreeRelocationClaimAsUnresolved fences a resolved archive when a
// later read-only gate cannot answer. Unlike the ordinary abort helper, it also
// materializes record-free claims so kill cannot read absence as permission.
func (i *Instance) PreserveWorktreeRelocationClaimAsUnresolved(claim git.RelocationClaim) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw != nil {
		gw.PreserveRelocationClaimAsUnresolved(claim)
	}
}

// PrepareWorktreeRelocationClaimForCleanup persists a resolved archived-path
// identity as a cleanup-only obligation. It is the non-relocating completion
// path used when the origin repo is gone.
func (i *Instance) PrepareWorktreeRelocationClaimForCleanup(claim git.RelocationClaim) error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot prepare worktree cleanup for %q: instance has no worktree", i.Title)
	}
	return gw.PrepareRelocationClaimForCleanup(claim)
}

// SetRepoGoneFinalizationCheckpoint installs the daemon's durable writer for
// the post-content, pre-root cleanup boundary. Kill runs backend teardown outside
// i.mu, so the callback may safely snapshot this instance for persistence.
func (i *Instance) SetRepoGoneFinalizationCheckpoint(checkpoint func() error) func() {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return func() {}
	}
	return gw.SetRepoGoneFinalizationCheckpoint(checkpoint)
}

// WorktreeRelocationRecovery snapshots the durable relocation lifecycle — a
// recovery record or an active consumed claim, projected to record form — for
// this instance's worktree. Deliberately not gated on started: destruction
// admission runs against archived instances, whose runtime is inert.
func (i *Instance) WorktreeRelocationRecovery() (git.RelocationRecovery, bool) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return git.RelocationRecovery{}, false
	}
	return gw.GetRelocationRecovery()
}

// SettleStalledWorktreeRelocationForAbsentPath clears an identity-unknown
// stalled relocation record once its pathname conclusively answers ENOENT.
// See GitWorktree.SettleStalledRelocationForAbsentPath.
func (i *Instance) SettleStalledWorktreeRelocationForAbsentPath() error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot settle stalled worktree relocation for %q: instance has no worktree", i.Title)
	}
	return gw.SettleStalledRelocationForAbsentPath()
}

// RestoreStalledWorktreeFenceAfterFailedSettle re-materializes the reclaimable
// stalled fence after a failed settle. See
// GitWorktree.RestoreStalledFenceAfterFailedSettle.
func (i *Instance) RestoreStalledWorktreeFenceAfterFailedSettle() error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot restore stalled worktree fence for %q: instance has no worktree", i.Title)
	}
	return gw.RestoreStalledFenceAfterFailedSettle()
}

// SettleWorktreeRelocationClaim revalidates a consumed recovery claim and
// releases its ownership without relocating anything. The kill admission
// producer uses it when a reclaimed stalled record turns out to need no
// cleanup authority because the origin repository answered present.
func (i *Instance) SettleWorktreeRelocationClaim(claim git.RelocationClaim) error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot settle worktree relocation for %q: instance has no worktree", i.Title)
	}
	return gw.SettleRelocationClaim(claim)
}

// ValidateWorktreeDestructionAdmission is the pre-commit guard. The local
// backend separately consumes and revalidates the exact cleanup identity before
// pane teardown; it must not repeat the origin-path admission after the durable
// kill has committed.
func (i *Instance) ValidateWorktreeDestructionAdmission() error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return nil
	}
	// An identity-unknown stall over a conclusively absent path guards nothing
	// and must not make the kill inadmissible forever (#5102).
	gw.SettleAbsentIdentityUnknownStall()
	path, recovery, unresolved := gw.RelocationSnapshot()
	if !unresolved || recovery.State == git.RelocationRecoveryCleanupStalled {
		return nil
	}
	if recovery.State == git.RelocationRecoveryCleanupReady ||
		recovery.State == git.RelocationRecoveryCleanupFinalizing {
		if err := gw.ValidateRelocationCleanupAdmission(); err != nil {
			return fmt.Errorf("cleanup worktree in state %s at %s is not admissible for repo-gone kill: %w", recovery.State, path, err)
		}
		return nil
	}
	return fmt.Errorf(
		"worktree recovery state %s is unresolved at %s; retry archive or restore before destructive cleanup",
		recovery.State, path,
	)
}
