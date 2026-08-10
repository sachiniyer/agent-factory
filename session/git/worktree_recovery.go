package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RelocationRecoveryState is the explicit lifecycle of a durable worktree
// recovery record. A record is present from the instant an operation becomes
// unestablished until a bounded resolver selects its source. Selection clears
// the record atomically and returns a point-in-time RelocationClaim instead.
type RelocationRecoveryState string

const (
	RelocationRecoveryMoveUnknown    RelocationRecoveryState = "move_unknown"
	RelocationRecoveryStalled        RelocationRecoveryState = "relocation_stalled"
	RelocationRecoveryClaimStale     RelocationRecoveryState = "claim_stale"
	RelocationRecoveryCleanupStalled RelocationRecoveryState = "cleanup_stalled"
	RelocationRecoveryCleanupReady   RelocationRecoveryState = "cleanup_ready"
)

// RelocationClaim is a point-in-time assertion that Path named one exact
// directory when recovery was resolved. Every consumer must revalidate it
// immediately before using the pathname. Claims which consume durable recovery
// remain owned by GitWorktree until their use completes; snapshots project that
// ownership back into durable recovery form.
type RelocationClaim struct {
	Path          string
	AlternatePath string
	identity      pathIdentity
	// recoveryOwned is true when producing this claim consumed a durable record.
	// The owner must either reach a revalidated use boundary or rematerialize the
	// claim if an earlier gate aborts the operation.
	recoveryOwned bool
	// cleanupAuthorized preserves the claim's cleanup-ready purpose across an
	// answered transient error. Only an actual failed identity validation may
	// downgrade it to claim_stale.
	cleanupAuthorized bool
	claimID           uint64
}

// SetRelocationIdentityErrorForTest makes identity inspection of one exact path
// return err and returns a restore function. It exists for callers outside this
// package which must prove that a lifecycle record created by a failed bounded
// probe is persisted before control returns.
func SetRelocationIdentityErrorForTest(path string, err error) func() {
	previous := relocationPathIdentity
	relocationPathIdentity = func(observed string) (pathIdentity, error) {
		if observed == path {
			return pathIdentity{}, err
		}
		return previous(observed)
	}
	return func() { relocationPathIdentity = previous }
}

func normalizeRecovery(recovery RelocationRecovery) RelocationRecovery {
	if recovery.State == "" {
		// Records written before the explicit lifecycle always represented two
		// candidates from a timed-out git worktree move.
		recovery.State = RelocationRecoveryMoveUnknown
		recovery.IdentityKnown = true
	}
	return recovery
}

// GetRelocationRecovery snapshots durable recovery ownership under the same lock
// as worktreePath. An active consumed claim is projected back into record form;
// absence therefore means no unresolved operation is known.
func (g *GitWorktree) GetRelocationRecovery() (RelocationRecovery, bool) {
	_, recovery, ok := g.RelocationSnapshot()
	return recovery, ok
}

// RelocationSnapshot returns the pathname and the recovery record which
// qualifies it from one critical section. Persistence and admission checks use
// this rather than composing two individually locked reads into a state that may
// never have existed.
func (g *GitWorktree) RelocationSnapshot() (string, RelocationRecovery, bool) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil {
		return g.worktreePath, *g.relocationRecovery, true
	}
	if g.activeRelocationClaim != nil {
		return g.worktreePath, recoveryFromClaim(*g.activeRelocationClaim), true
	}
	return g.worktreePath, RelocationRecovery{}, false
}

// RestoreRelocationRecovery reinstates a persisted lifecycle record without
// touching either candidate. Legacy records without State remain readable as an
// ambiguous move.
func (g *GitWorktree) RestoreRelocationRecovery(recovery RelocationRecovery) error {
	recovery = normalizeRecovery(recovery)
	switch recovery.State {
	case RelocationRecoveryMoveUnknown:
		if recovery.AlternatePath == "" {
			return fmt.Errorf("move recovery alternate path is empty")
		}
		if !recovery.IdentityKnown {
			return fmt.Errorf("move recovery directory identity is missing")
		}
	case RelocationRecoveryClaimStale:
		if !recovery.IdentityKnown {
			return fmt.Errorf("stale relocation claim identity is missing")
		}
	case RelocationRecoveryCleanupReady:
		if !recovery.IdentityKnown {
			return fmt.Errorf("cleanup-ready relocation identity is missing")
		}
	case RelocationRecoveryStalled:
		// A read-only probe may time out before any identity can be captured.
	case RelocationRecoveryCleanupStalled:
		// A generic git cleanup stall is process-epoch state and a fresh daemon
		// may retry its bounded git commands. An identity-qualified stall came
		// from the separately bounded repo-gone descriptor walk; preserve it as a
		// cleanup-ready obligation so restart retries the exact claimed archive.
		if !recovery.IdentityKnown {
			return nil
		}
		// Absence is not completion proof: the same state is also written when
		// the origin-repo probe times out before deletion starts, and another
		// process may rename the archive before restart. Only the live descriptor
		// worker can reconcile its own late success.
		recovery.State = RelocationRecoveryCleanupReady
	default:
		return fmt.Errorf("unknown relocation recovery state %q", recovery.State)
	}

	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil || g.activeRelocationClaim != nil {
		return fmt.Errorf("cannot restore relocation recovery over an active lifecycle")
	}
	if recovery.AlternatePath != "" && recovery.AlternatePath == g.worktreePath {
		return fmt.Errorf("relocation recovery paths are identical: %s", g.worktreePath)
	}
	g.relocationRecovery = &recovery
	return nil
}

func (g *GitWorktree) HasUnresolvedRelocation() bool {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.relocationRecovery != nil || g.activeRelocationClaim != nil
}

func (g *GitWorktree) hasRelocationRecoveryRecord() bool {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.relocationRecovery != nil
}

func (g *GitWorktree) CleanupRetryPending() bool {
	recovery, ok := g.GetRelocationRecovery()
	return ok && recovery.State == RelocationRecoveryCleanupStalled
}

func (g *GitWorktree) beginRelocationRecovery(destination, source string, identity pathIdentity) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	g.setWorktreeLocationLocked(destination)
	g.activeRelocationClaim = nil
	g.relocationRecovery = &RelocationRecovery{
		State:         RelocationRecoveryMoveUnknown,
		AlternatePath: source,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
	}
}

func (g *GitWorktree) recordStall(state RelocationRecoveryState, claim *RelocationClaim) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if claim != nil && claim.recoveryOwned && !g.ownsRelocationClaimLocked(*claim) {
		// The operation completed at the deadline boundary before this latch won
		// the lock. Never recreate a stall after completion released ownership.
		return
	}
	// An ambiguous move or stale claim contains stronger evidence than a later
	// generic stall. Never replace it with a less precise record.
	if g.relocationRecovery != nil &&
		g.relocationRecovery.State != RelocationRecoveryCleanupStalled &&
		g.relocationRecovery.State != RelocationRecoveryStalled {
		g.releaseRelocationClaimLocked(claim)
		return
	}
	recovery := RelocationRecovery{State: state}
	if claim != nil && claim.Path == g.worktreePath {
		recovery.IdentityKnown = true
		recovery.Device = claim.identity.device
		recovery.Inode = claim.identity.inode
		recovery.FileType = claim.identity.fileType
		recovery.AlternatePath = claim.AlternatePath
	}
	g.relocationRecovery = &recovery
	g.releaseRelocationClaimLocked(claim)
}

func (g *GitWorktree) markRelocationStalled(claim *RelocationClaim) {
	g.recordStall(RelocationRecoveryStalled, claim)
}

// markCleanupStalled is retained as Cleanup's single timeout chokepoint. Unlike
// the old process-local bool, it creates durable lifecycle state.
func (g *GitWorktree) markCleanupStalled() {
	g.markCleanupStalledWithClaim(nil)
}

func (g *GitWorktree) markCleanupStalledWithClaim(claim *RelocationClaim) {
	g.recordStall(RelocationRecoveryCleanupStalled, claim)
}

func (g *GitWorktree) cleanupHasStalled() bool {
	return g.HasUnresolvedRelocation()
}

// ClaimRelocationSource resolves any durable record and consumes it in the same
// critical section that selects worktreePath. The returned claim is the only
// thing later readers may use, and it must be revalidated at each use boundary.
func (g *GitWorktree) ClaimRelocationSource() (RelocationClaim, error) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.activeRelocationClaim != nil {
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"worktree relocation claim for %s is already in use", g.activeRelocationClaim.Path,
		), ErrRelocateStateUnknown)
	}

	primary := g.worktreePath
	if primary == "" {
		return RelocationClaim{}, fmt.Errorf("cannot claim an empty worktree path")
	}
	recovery := g.relocationRecovery
	if recovery == nil {
		identity, err := boundedRelocationPathIdentity(primary)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				g.relocationRecovery = &RelocationRecovery{State: RelocationRecoveryStalled}
				return RelocationClaim{}, errors.Join(fmt.Errorf(
					"cannot claim worktree path %s: %w", primary, err,
				), ErrRelocateStateUnknown)
			}
			return RelocationClaim{}, fmt.Errorf("cannot claim worktree path %s: %w", primary, err)
		}
		return RelocationClaim{Path: primary, identity: identity}, nil
	}

	normalized := normalizeRecovery(*recovery)
	g.relocationRecovery = &normalized
	switch normalized.State {
	case RelocationRecoveryStalled, RelocationRecoveryCleanupStalled, RelocationRecoveryCleanupReady:
		if normalized.IdentityKnown && normalized.AlternatePath != "" {
			// A stalled operation can retain the same two identity-qualified
			// candidates as an interrupted move. Resolve both under this lock;
			// a vanished or replaced primary is not evidence that the known
			// alternate disappeared with it.
			return g.resolveCandidateRecordLocked(primary, normalized)
		}
		identity, err := boundedRelocationPathIdentity(primary)
		if err != nil {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cannot resolve stalled worktree path %s: %w", primary, err,
			), ErrRelocateStateUnknown)
		}
		if normalized.IdentityKnown && !normalized.identity().same(identity) {
			g.relocationRecovery.State = RelocationRecoveryClaimStale
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"worktree path %s no longer identifies the directory captured before the stall", primary,
			), ErrRelocateStateUnknown)
		}
		g.relocationRecovery = nil
		return g.activateRelocationClaimLocked(RelocationClaim{
			Path: primary, AlternatePath: normalized.AlternatePath,
			identity: identity, cleanupAuthorized: normalized.State == RelocationRecoveryCleanupReady,
		}), nil
	case RelocationRecoveryMoveUnknown, RelocationRecoveryClaimStale:
		return g.resolveCandidateRecordLocked(primary, normalized)
	default:
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"cannot resolve unknown relocation recovery state %q", normalized.State,
		), ErrRelocateStateUnknown)
	}
}

func (g *GitWorktree) resolveCandidateRecordLocked(primary string, recovery RelocationRecovery) (RelocationClaim, error) {
	expected := recovery.identity()
	primaryIdentity, primaryErr := boundedRelocationPathIdentity(primary)
	if primaryErr == nil && expected.same(primaryIdentity) {
		g.relocationRecovery = nil
		return g.activateRelocationClaimLocked(RelocationClaim{
			Path: primary, AlternatePath: recovery.AlternatePath,
			identity: expected, cleanupAuthorized: recovery.State == RelocationRecoveryCleanupReady,
		}), nil
	}
	if recovery.AlternatePath == "" {
		if primaryErr != nil && !errors.Is(primaryErr, os.ErrNotExist) {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cannot resolve stale relocation claim: candidate %s could not be verified and no identity-qualified alternate exists: %w",
				primary, primaryErr,
			), ErrRelocateStateUnknown)
		}
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"cannot resolve stale relocation claim: %s no longer identifies the original worktree", primary,
		), ErrRelocateStateUnknown)
	}
	// A timeout or other non-answer from the primary establishes nothing about
	// the alternate. Give the identity-qualified alternate its own bounded probe;
	// only an exact match below may consume the durable recovery record.
	alternateIdentity, alternateErr := boundedRelocationPathIdentity(recovery.AlternatePath)
	if alternateErr != nil || !expected.same(alternateIdentity) {
		primaryFailure := primaryErr
		if primaryFailure == nil {
			primaryFailure = errors.New("identity mismatch")
		}
		alternateFailure := alternateErr
		if alternateFailure == nil {
			alternateFailure = errors.New("identity mismatch")
		}
		if primaryErr != nil && !errors.Is(primaryErr, os.ErrNotExist) {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cannot resolve interrupted relocation: neither candidate was established (%s: %w; %s: %v)",
				primary, primaryErr, recovery.AlternatePath, alternateFailure,
			), ErrRelocateStateUnknown)
		}
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"cannot resolve interrupted relocation: neither candidate was established (%s: %v; %s: %v)",
			primary, primaryFailure, recovery.AlternatePath, alternateFailure,
		), ErrRelocateStateUnknown)
	}
	selected := recovery.AlternatePath
	g.setWorktreeLocationLocked(selected)
	g.relocationRecovery = nil
	return g.activateRelocationClaimLocked(RelocationClaim{
		Path: selected, AlternatePath: primary,
		identity: expected, cleanupAuthorized: recovery.State == RelocationRecoveryCleanupReady,
	}), nil
}

func (g *GitWorktree) activateRelocationClaimLocked(claim RelocationClaim) RelocationClaim {
	g.nextRelocationClaimID++
	claim.recoveryOwned = true
	claim.claimID = g.nextRelocationClaimID
	active := claim
	g.activeRelocationClaim = &active
	return claim
}

func recoveryFromClaim(claim RelocationClaim) RelocationRecovery {
	alternate := claim.AlternatePath
	if alternate == claim.Path {
		alternate = ""
	}
	state := RelocationRecoveryClaimStale
	if claim.cleanupAuthorized {
		state = RelocationRecoveryCleanupReady
	}
	return RelocationRecovery{
		State:         state,
		AlternatePath: alternate,
		IdentityKnown: true,
		Device:        claim.identity.device,
		Inode:         claim.identity.inode,
		FileType:      claim.identity.fileType,
	}
}

func (g *GitWorktree) ownsRelocationClaimLocked(claim RelocationClaim) bool {
	return claim.recoveryOwned && claim.claimID != 0 &&
		g.activeRelocationClaim != nil && g.activeRelocationClaim.claimID == claim.claimID
}

func (g *GitWorktree) releaseRelocationClaimLocked(claim *RelocationClaim) {
	if claim != nil && g.ownsRelocationClaimLocked(*claim) {
		g.activeRelocationClaim = nil
	}
}

// PreserveRelocationClaim rematerializes a durable record when an operation
// which consumed one aborts before completing its path decision. A claim made
// from an ordinary record-free path owns nothing and is intentionally a no-op.
func (g *GitWorktree) PreserveRelocationClaim(claim RelocationClaim) {
	if !claim.recoveryOwned {
		return
	}
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil {
		// A use-boundary failure or a later deadline already installed more current
		// recovery evidence. Never overwrite it with the earlier claim.
		g.releaseRelocationClaimLocked(&claim)
		return
	}
	recovery := recoveryFromClaim(claim)
	g.relocationRecovery = &recovery
	g.releaseRelocationClaimLocked(&claim)
}

// RevalidateRelocationClaim checks a point-in-time claim immediately before a
// pathname is consumed. Failure recreates durable state before returning, so a
// stale claim can never degrade into an absent record.
func (g *GitWorktree) RevalidateRelocationClaim(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.revalidateRelocationClaimLocked(claim, false)
}

// SettleRelocationClaim revalidates a consumed recovery claim and releases its
// ownership in the same critical section. It is used when a caller has resolved
// the archived directory but intentionally will not relocate it (for example,
// because the origin repository is gone).
func (g *GitWorktree) SettleRelocationClaim(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.revalidateRelocationClaimLocked(claim, true)
}

// completeRemovedRelocationClaim releases ownership after a descriptor-anchored
// delete has removed the claimed directory. Revalidating the pathname here would
// necessarily fail because successful deletion made it absent; ownership, not a
// fresh path lookup, is the completion proof.
func (g *GitWorktree) completeRemovedRelocationClaim(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.worktreePath == claim.Path && g.ownsRelocationClaimLocked(claim) && g.relocationRecovery == nil {
		g.releaseRelocationClaimLocked(&claim)
		return nil
	}
	if g.worktreePath == claim.Path && g.relocationRecovery != nil &&
		g.relocationRecovery.State == RelocationRecoveryCleanupStalled &&
		g.relocationRecovery.IdentityKnown && g.relocationRecovery.identity().same(claim.identity) {
		// The caller bound fired first and rematerialized the claim; the exact
		// descriptor worker then completed. Reconcile that late success even
		// though nobody is waiting on its buffered result anymore.
		g.relocationRecovery = nil
		return nil
	}
	return errors.Join(fmt.Errorf(
		"worktree relocation claim for removed path %s is no longer the active owner", claim.Path,
	), ErrRelocateStateUnknown)
}

// PrepareRelocationClaimForCleanup turns a resolved point-in-time claim into a
// new durable obligation which only the kill cleanup path may consume. The old
// recovery record was already cleared when the claim was resolved; this record
// represents the later destructive decision and keeps its identity across the
// time between a repo-gone restore and an explicit kill.
func (g *GitWorktree) PrepareRelocationClaimForCleanup(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if err := g.revalidateRelocationClaimLocked(claim, false); err != nil {
		return err
	}
	recovery := recoveryFromClaim(claim)
	recovery.State = RelocationRecoveryCleanupReady
	g.relocationRecovery = &recovery
	g.releaseRelocationClaimLocked(&claim)
	return nil
}

// ValidateRelocationCleanupAdmission rechecks a cleanup-ready record without
// consuming it. Kill calls this before its tombstone and pane teardown; the
// cleanup itself consumes a fresh claim and validates once more immediately
// before deleting the directory.
func (g *GitWorktree) ValidateRelocationCleanupAdmission() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery == nil || g.relocationRecovery.State != RelocationRecoveryCleanupReady {
		return errors.Join(fmt.Errorf("worktree has no cleanup-ready relocation identity"), ErrRelocateStateUnknown)
	}
	recovery := g.relocationRecovery
	identity, err := boundedRelocationPathIdentity(g.worktreePath)
	if err != nil {
		// A failed read is not evidence of a changed identity. Keep the exact
		// cleanup authorization retryable unless the path is conclusively absent.
		if errors.Is(err, os.ErrNotExist) {
			recovery.State = RelocationRecoveryClaimStale
		}
		return errors.Join(fmt.Errorf(
			"cannot validate worktree path %s for cleanup: %w",
			g.worktreePath, err,
		), ErrRelocateStateUnknown)
	}
	if !recovery.identity().same(identity) {
		recovery.State = RelocationRecoveryClaimStale
		return errors.Join(fmt.Errorf(
			"worktree path %s no longer identifies the directory authorized for cleanup",
			g.worktreePath,
		), ErrRelocateStateUnknown)
	}
	if g.repoPath == "" {
		return errors.Join(fmt.Errorf("origin repo path is empty"), ErrRelocateStateUnknown)
	}
	if err := boundedRepoGoneOriginProbe(g); err == nil {
		return fmt.Errorf(
			"origin repo %s exists again; restore the archived session before retrying kill",
			g.repoPath,
		)
	} else if !errors.Is(err, ErrRepoGone) && !os.IsNotExist(err) {
		return errors.Join(fmt.Errorf(
			"cannot establish that origin repo %s is still gone: %w",
			g.repoPath, err,
		), ErrRelocateStateUnknown)
	}
	return nil
}

func (g *GitWorktree) revalidateRelocationClaimLocked(claim RelocationClaim, settle bool) error {
	if g.relocationRecovery != nil {
		return errors.Join(fmt.Errorf(
			"worktree recovery state changed before claim for %s was consumed", claim.Path,
		), ErrRelocateStateUnknown)
	}
	if claim.recoveryOwned && !g.ownsRelocationClaimLocked(claim) {
		g.recordStaleClaimLocked(claim)
		return errors.Join(fmt.Errorf(
			"worktree relocation claim for %s is no longer the active owner", claim.Path,
		), ErrRelocateStateUnknown)
	}
	if claim.Path == "" || g.worktreePath != claim.Path {
		g.recordStaleClaimLocked(claim)
		return errors.Join(fmt.Errorf(
			"worktree path changed from claimed path %s to %s", claim.Path, g.worktreePath,
		), ErrRelocateStateUnknown)
	}
	identity, err := boundedRelocationPathIdentity(claim.Path)
	if err != nil {
		if claim.cleanupAuthorized && !errors.Is(err, os.ErrNotExist) {
			recovery := recoveryFromClaim(claim)
			if errors.Is(err, context.DeadlineExceeded) {
				recovery.State = RelocationRecoveryCleanupStalled
			}
			g.relocationRecovery = &recovery
			g.releaseRelocationClaimLocked(&claim)
		} else {
			g.recordStaleClaimLocked(claim)
		}
		return errors.Join(fmt.Errorf(
			"worktree path %s changed after identity resolution: %v", claim.Path, err,
		), ErrRelocateStateUnknown)
	}
	if !claim.identity.same(identity) {
		g.recordStaleClaimLocked(claim)
		return errors.Join(fmt.Errorf(
			"worktree path %s no longer identifies the directory selected during recovery", claim.Path,
		), ErrRelocateStateUnknown)
	}
	if settle {
		g.releaseRelocationClaimLocked(&claim)
	}
	return nil
}

func (g *GitWorktree) recordStaleClaimLocked(claim RelocationClaim) {
	alternate := claim.AlternatePath
	if alternate == g.worktreePath {
		alternate = ""
	}
	recovery := recoveryFromClaim(claim)
	recovery.State = RelocationRecoveryClaimStale
	recovery.AlternatePath = alternate
	g.relocationRecovery = &recovery
	g.releaseRelocationClaimLocked(&claim)
}

// finishRelocationClaim records the committed destination and releases a
// consumed recovery claim atomically. A checkpoint sees either the old path plus
// its active claim, or the new settled path; never the new path with stale
// ownership or an absent claim at the old path.
func (g *GitWorktree) finishRelocationClaim(claim RelocationClaim, dest string) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	g.setWorktreeLocationLocked(dest)
	g.relocationRecovery = nil
	g.releaseRelocationClaimLocked(&claim)
}

func (g *GitWorktree) setWorktreeLocationLocked(dest string) {
	g.worktreePath = dest
	g.worktreeDir = filepath.Dir(dest)
}
