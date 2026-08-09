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
)

// RelocationClaim is an ephemeral assertion that Path named one exact directory
// when recovery was resolved. It is deliberately not persisted: every consumer
// must revalidate it immediately before using the pathname.
type RelocationClaim struct {
	Path          string
	AlternatePath string
	identity      pathIdentity
	// recoveryOwned is true when producing this claim consumed a durable record.
	// The owner must either reach a revalidated use boundary or rematerialize the
	// claim if an earlier gate aborts the operation.
	recoveryOwned bool
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

// GetRelocationRecovery snapshots the recovery record under the same lock as
// worktreePath. Absence means no unresolved operation is known; a deadline path
// always materializes a record before returning.
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
	if g.relocationRecovery == nil {
		return g.worktreePath, RelocationRecovery{}, false
	}
	return g.worktreePath, *g.relocationRecovery, true
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
	case RelocationRecoveryStalled:
		// A read-only probe may time out before any identity can be captured.
	case RelocationRecoveryCleanupStalled:
		// A cleanup stall is deliberately process-epoch state: within this daemon
		// no later fast probe can prove an unbounded delete will not wedge again.
		// Loading in a fresh daemon is the explicit retry boundary, matching the
		// original cleanup contract, so consume this persisted latch on restore.
		return nil
	default:
		return fmt.Errorf("unknown relocation recovery state %q", recovery.State)
	}

	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if recovery.AlternatePath != "" && recovery.AlternatePath == g.worktreePath {
		return fmt.Errorf("relocation recovery paths are identical: %s", g.worktreePath)
	}
	g.relocationRecovery = &recovery
	return nil
}

func (g *GitWorktree) HasUnresolvedRelocation() bool {
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
	g.relocationRecovery = &RelocationRecovery{
		State:         RelocationRecoveryMoveUnknown,
		AlternatePath: source,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
	}
}

func (g *GitWorktree) clearRelocationRecovery() {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	g.relocationRecovery = nil
}

func (g *GitWorktree) recordStall(state RelocationRecoveryState, claim *RelocationClaim) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	// An ambiguous move or stale claim contains stronger evidence than a later
	// generic stall. Never replace it with a less precise record.
	if g.relocationRecovery != nil &&
		g.relocationRecovery.State != RelocationRecoveryCleanupStalled &&
		g.relocationRecovery.State != RelocationRecoveryStalled {
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
	case RelocationRecoveryStalled, RelocationRecoveryCleanupStalled:
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
		return RelocationClaim{
			Path: primary, AlternatePath: normalized.AlternatePath,
			identity: identity, recoveryOwned: true,
		}, nil
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
		return RelocationClaim{
			Path: primary, AlternatePath: recovery.AlternatePath,
			identity: expected, recoveryOwned: true,
		}, nil
	}
	if primaryErr != nil && !errors.Is(primaryErr, os.ErrNotExist) {
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"cannot resolve interrupted relocation: candidate %s could not be verified: %w",
			primary, primaryErr,
		), ErrRelocateStateUnknown)
	}
	if recovery.AlternatePath == "" {
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"cannot resolve stale relocation claim: %s no longer identifies the original worktree", primary,
		), ErrRelocateStateUnknown)
	}
	alternateIdentity, alternateErr := boundedRelocationPathIdentity(recovery.AlternatePath)
	if alternateErr != nil || !expected.same(alternateIdentity) {
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"cannot resolve interrupted relocation: neither candidate was established (%s: %v; %s: %v)",
			primary, primaryErr, recovery.AlternatePath, alternateErr,
		), ErrRelocateStateUnknown)
	}
	selected := recovery.AlternatePath
	g.setWorktreeLocationLocked(selected)
	g.relocationRecovery = nil
	return RelocationClaim{
		Path: selected, AlternatePath: primary,
		identity: expected, recoveryOwned: true,
	}, nil
}

// PreserveRelocationClaim rematerializes a durable record when an operation
// which consumed one aborts before reaching any path use boundary. A claim made
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
		return
	}
	g.recordStaleClaimLocked(claim)
}

// RevalidateRelocationClaim checks a point-in-time claim immediately before a
// pathname is consumed. Failure recreates durable state before returning, so a
// stale claim can never degrade into an absent record.
func (g *GitWorktree) RevalidateRelocationClaim(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil {
		return errors.Join(fmt.Errorf(
			"worktree recovery state changed before claim for %s was consumed", claim.Path,
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
		g.recordStaleClaimLocked(claim)
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
	return nil
}

func (g *GitWorktree) recordStaleClaimLocked(claim RelocationClaim) {
	alternate := claim.AlternatePath
	if alternate == g.worktreePath {
		alternate = ""
	}
	g.relocationRecovery = &RelocationRecovery{
		State:         RelocationRecoveryClaimStale,
		AlternatePath: alternate,
		IdentityKnown: true,
		Device:        claim.identity.device,
		Inode:         claim.identity.inode,
		FileType:      claim.identity.fileType,
	}
}

func (g *GitWorktree) setWorktreeLocationLocked(dest string) {
	g.worktreePath = dest
	g.worktreeDir = filepath.Dir(dest)
}
