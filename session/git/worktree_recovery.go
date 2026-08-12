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
	RelocationRecoveryMoveUnknown       RelocationRecoveryState = "move_unknown"
	RelocationRecoveryStalled           RelocationRecoveryState = "relocation_stalled"
	RelocationRecoveryClaimStale        RelocationRecoveryState = "claim_stale"
	RelocationRecoveryCleanupStalled    RelocationRecoveryState = "cleanup_stalled"
	RelocationRecoveryCleanupReady      RelocationRecoveryState = "cleanup_ready"
	RelocationRecoveryCleanupFinalizing RelocationRecoveryState = "cleanup_finalizing"
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
	// cleanupAuthorized preserves a cleanup-ready claim across an answered
	// transient error. Only a failed identity or generation validation may
	// downgrade it to claim_stale.
	cleanupAuthorized bool
	cleanupGeneration string
	// cleanupFinalizing means the descriptor walk already removed every entry
	// beneath the claimed root and durably checkpointed that fact. A retry may
	// remove only that exact empty root; absence or replacement is completion,
	// never authority to touch the pathname now there.
	cleanupFinalizing bool
	cleanupRootGone   bool
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

// SetCleanupGenerationInstallErrorForTest makes cleanup-generation installation
// fail and returns a restore function. It lets daemon tests prove that the
// already-persisted claim_stale fence survives a late authorization failure.
func SetCleanupGenerationInstallErrorForTest(err error) func() {
	previous := cleanupGenerationInstall
	cleanupGenerationInstall = func(string, pathIdentity) (string, error) {
		return "", err
	}
	return func() { cleanupGenerationInstall = previous }
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
	return g.relocationSnapshotLocked()
}

func (g *GitWorktree) relocationSnapshotLocked() (string, RelocationRecovery, bool) {
	if g.relocationRecovery != nil {
		return g.worktreePath, *g.relocationRecovery, true
	}
	if g.activeRelocationClaim != nil {
		return g.worktreePath, recoveryFromClaim(*g.activeRelocationClaim), true
	}
	return g.worktreePath, RelocationRecovery{}, false
}

// PersistenceSnapshot returns relocation ownership and archive completeness as
// one state. A checkpoint cannot observe the committed archive path without the
// report that explains its known-absent entries, or vice versa.
func (g *GitWorktree) PersistenceSnapshot() (string, RelocationRecovery, bool, ArchiveReport) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	path, recovery, hasRecovery := g.relocationSnapshotLocked()
	return path, recovery, hasRecovery, g.archiveReport.Clone()
}

// ProjectionSnapshot returns the live, bounded view of relocation and archive
// state. The full report is deliberately absent: status/UI snapshots happen
// continuously, while the lossless clone belongs only to PersistenceSnapshot.
func (g *GitWorktree) ProjectionSnapshot(
	operation string,
) (string, RelocationRecovery, bool, bool, string) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	path, recovery, hasRecovery := g.relocationSnapshotLocked()
	hasArchiveReport := !g.archiveReport.Empty()
	if !hasArchiveReport {
		return path, recovery, hasRecovery, false, ""
	}
	// Production writers refresh the cache with every report mutation. The lazy
	// fallback keeps zero-value/direct-literal GitWorktree fixtures correct too.
	if g.archiveWarningSuffix == "" {
		g.refreshArchiveWarningLocked()
	}
	return path, recovery, hasRecovery, true, renderArchiveWarning(operation, g.archiveWarningSuffix)
}

func (g *GitWorktree) replaceArchiveReportLocked(report ArchiveReport) {
	g.archiveReport = report.Clone()
	g.refreshArchiveWarningLocked()
}

func (g *GitWorktree) refreshArchiveWarningLocked() {
	g.archiveWarningSuffix = g.archiveReport.warningSuffix()
}

// RestoreArchiveReport reinstates the durable report when a session record is
// loaded. The caller has not exposed the worktree to runtime use yet.
func (g *GitWorktree) RestoreArchiveReport(report ArchiveReport) {
	g.relocationMu.Lock()
	g.replaceArchiveReportLocked(report)
	g.relocationMu.Unlock()
}

// GetArchiveReport returns a detached copy for user-facing archive/restore
// reporting.
func (g *GitWorktree) GetArchiveReport() ArchiveReport {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.archiveReport.Clone()
}

func (g *GitWorktree) setArchiveReport(report ArchiveReport) {
	g.relocationMu.Lock()
	g.replaceArchiveReportLocked(report)
	g.relocationMu.Unlock()
}

// SetRepoGoneFinalizationCheckpoint installs the durable writer used by an
// explicit kill while it owns this worktree's operation lock. The returned
// restore function removes the process-local closure after teardown.
func (g *GitWorktree) SetRepoGoneFinalizationCheckpoint(checkpoint func() error) func() {
	g.relocationMu.Lock()
	previous := g.repoGoneFinalizationCheckpoint
	g.repoGoneFinalizationCheckpoint = checkpoint
	g.relocationMu.Unlock()
	return func() {
		g.relocationMu.Lock()
		g.repoGoneFinalizationCheckpoint = previous
		g.relocationMu.Unlock()
	}
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
	case RelocationRecoveryCleanupReady, RelocationRecoveryCleanupFinalizing:
		if !recovery.IdentityKnown {
			return fmt.Errorf("cleanup relocation identity is missing for state %s", recovery.State)
		}
		if recovery.CleanupGeneration == "" {
			return fmt.Errorf("cleanup relocation generation is missing for state %s", recovery.State)
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
		if recovery.CleanupGeneration == "" {
			return fmt.Errorf("identity-qualified cleanup stall generation is missing")
		}
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
		recovery.CleanupGeneration = claim.cleanupGeneration
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
	case RelocationRecoveryCleanupFinalizing:
		expected := normalized.identity()
		primaryIdentity, primaryErr := boundedRelocationPathIdentity(primary)
		primaryMatches := primaryErr == nil && expected.same(primaryIdentity)
		var primaryGenerationErr error
		if primaryMatches {
			primaryGenerationErr = requireCleanupPathIdentity(primary, expected, normalized.CleanupGeneration)
			if primaryGenerationErr == nil {
				g.relocationRecovery = nil
				return g.activateRelocationClaimLocked(RelocationClaim{
					Path: primary, AlternatePath: normalized.AlternatePath, identity: expected,
					cleanupAuthorized: true, cleanupGeneration: normalized.CleanupGeneration,
					cleanupFinalizing: true,
				}), nil
			}
		}
		if normalized.AlternatePath != "" {
			alternateIdentity, alternateErr := boundedRelocationPathIdentity(normalized.AlternatePath)
			if alternateErr == nil && expected.same(alternateIdentity) {
				if generationErr := requireCleanupPathIdentity(
					normalized.AlternatePath, expected, normalized.CleanupGeneration,
				); generationErr != nil {
					return RelocationClaim{}, errors.Join(fmt.Errorf(
						"cannot verify secured finalizing cleanup generation at %s: %w",
						normalized.AlternatePath, generationErr,
					), ErrRelocateStateUnknown)
				}
				selected := normalized.AlternatePath
				g.setWorktreeLocationLocked(selected)
				g.relocationRecovery = nil
				return g.activateRelocationClaimLocked(RelocationClaim{
					Path: selected, AlternatePath: primary, identity: expected,
					cleanupAuthorized: true, cleanupGeneration: normalized.CleanupGeneration,
					cleanupFinalizing: true,
				}), nil
			}
			if alternateErr != nil && !errors.Is(alternateErr, os.ErrNotExist) {
				return RelocationClaim{}, errors.Join(fmt.Errorf(
					"cannot inspect secured finalizing cleanup root %s: %w", normalized.AlternatePath, alternateErr,
				), ErrRelocateStateUnknown)
			}
		}
		if primaryGenerationErr != nil {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cannot verify finalizing cleanup generation at %s: %w", primary, primaryGenerationErr,
			), ErrRelocateStateUnknown)
		}
		if primaryErr != nil && !errors.Is(primaryErr, os.ErrNotExist) {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cannot inspect finalizing cleanup root %s: %w", primary, primaryErr,
			), ErrRelocateStateUnknown)
		}
		g.relocationRecovery = nil
		return g.activateRelocationClaimLocked(RelocationClaim{
			Path: primary, AlternatePath: normalized.AlternatePath, identity: expected,
			cleanupAuthorized: true, cleanupGeneration: normalized.CleanupGeneration,
			cleanupFinalizing: true, cleanupRootGone: true,
		}), nil
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
		if normalized.State == RelocationRecoveryCleanupReady {
			if err := requireCleanupPathIdentity(primary, normalized.identity(), normalized.CleanupGeneration); err != nil {
				g.relocationRecovery.State = RelocationRecoveryClaimStale
				return RelocationClaim{}, errors.Join(fmt.Errorf(
					"worktree path %s has no matching durable cleanup generation: %w", primary, err,
				), ErrRelocateStateUnknown)
			}
		}
		g.relocationRecovery = nil
		return g.activateRelocationClaimLocked(RelocationClaim{
			Path: primary, AlternatePath: normalized.AlternatePath,
			identity: identity, cleanupAuthorized: normalized.State == RelocationRecoveryCleanupReady,
			cleanupGeneration: normalized.CleanupGeneration,
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
		if recovery.State == RelocationRecoveryCleanupReady {
			if err := requireCleanupPathIdentity(primary, expected, recovery.CleanupGeneration); err != nil {
				g.relocationRecovery.State = RelocationRecoveryClaimStale
				return RelocationClaim{}, errors.Join(fmt.Errorf(
					"cleanup candidate %s has no matching durable generation: %w", primary, err,
				), ErrRelocateStateUnknown)
			}
		}
		g.relocationRecovery = nil
		return g.activateRelocationClaimLocked(RelocationClaim{
			Path: primary, AlternatePath: recovery.AlternatePath,
			identity: expected, cleanupAuthorized: recovery.State == RelocationRecoveryCleanupReady,
			cleanupGeneration: recovery.CleanupGeneration,
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
	if recovery.State == RelocationRecoveryCleanupReady {
		if err := requireCleanupPathIdentity(selected, expected, recovery.CleanupGeneration); err != nil {
			g.relocationRecovery.State = RelocationRecoveryClaimStale
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cleanup candidate %s has no matching durable generation: %w", selected, err,
			), ErrRelocateStateUnknown)
		}
	}
	g.setWorktreeLocationLocked(selected)
	g.relocationRecovery = nil
	return g.activateRelocationClaimLocked(RelocationClaim{
		Path: selected, AlternatePath: primary,
		identity: expected, cleanupAuthorized: recovery.State == RelocationRecoveryCleanupReady,
		cleanupGeneration: recovery.CleanupGeneration,
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
	if claim.cleanupFinalizing {
		state = RelocationRecoveryCleanupFinalizing
	} else if claim.cleanupAuthorized {
		state = RelocationRecoveryCleanupReady
	}
	return RelocationRecovery{
		State:             state,
		AlternatePath:     alternate,
		IdentityKnown:     true,
		Device:            claim.identity.device,
		Inode:             claim.identity.inode,
		FileType:          claim.identity.fileType,
		CleanupGeneration: claim.cleanupGeneration,
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

// PreserveRelocationClaimAsUnresolved materializes a non-destructive recovery
// record even for a claim obtained from record-free state. Restore uses this
// when an origin probe gives no answer: the directory identity is known, but
// neither relocation nor cleanup is authorized until a later retry resolves it.
func (g *GitWorktree) PreserveRelocationClaimAsUnresolved(claim RelocationClaim) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil {
		g.releaseRelocationClaimLocked(&claim)
		return
	}
	if claim.recoveryOwned && !g.ownsRelocationClaimLocked(claim) {
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

// repoGoneFinalizationCheckpointSnapshot captures the daemon's durable writer
// before the descriptor worker starts. The callback itself must run without the
// relocation lock because persistence takes a snapshot under that lock.
func (g *GitWorktree) repoGoneFinalizationCheckpointSnapshot() func() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	return g.repoGoneFinalizationCheckpoint
}

// checkpointRepoGoneFinalization publishes the only state in which pathname
// absence can mean cleanup completion. The descriptor walk has already removed
// every child and verified the root empty; the checkpoint lands while that exact
// root still occupies its claimed name. A failed writer restores cleanup_ready,
// leaving the empty root as an ordinary identity-qualified retry handle.
func (g *GitWorktree) checkpointRepoGoneFinalization(
	claim RelocationClaim,
	securedPath string,
	checkpoint func() error,
) error {
	g.relocationMu.Lock()
	if !g.ownsRelocationClaimLocked(claim) || g.relocationRecovery != nil {
		g.relocationMu.Unlock()
		return errors.Join(fmt.Errorf(
			"cannot checkpoint finalization for unowned cleanup claim %s", claim.Path,
		), ErrRelocateStateUnknown)
	}
	recovery := recoveryFromClaim(claim)
	recovery.State = RelocationRecoveryCleanupFinalizing
	recovery.AlternatePath = securedPath
	g.relocationRecovery = &recovery
	g.relocationMu.Unlock()

	if checkpoint == nil {
		return nil
	}
	if err := checkpoint(); err != nil {
		g.relocationMu.Lock()
		if g.relocationRecovery != nil &&
			g.relocationRecovery.State == RelocationRecoveryCleanupFinalizing &&
			g.relocationRecovery.identity().same(claim.identity) &&
			g.relocationRecovery.CleanupGeneration == claim.cleanupGeneration {
			recovery := recoveryFromClaim(claim)
			recovery.State = RelocationRecoveryCleanupReady
			g.relocationRecovery = &recovery
			g.releaseRelocationClaimLocked(&claim)
		}
		g.relocationMu.Unlock()
		return fmt.Errorf("persist repo-gone cleanup finalization fence: %w", err)
	}
	return nil
}

func (g *GitWorktree) completeRepoGoneFinalization(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if !claim.cleanupFinalizing || g.worktreePath != claim.Path ||
		g.relocationRecovery != nil || !g.ownsRelocationClaimLocked(claim) {
		return errors.Join(fmt.Errorf(
			"worktree finalization claim for %s is no longer applicable", claim.Path,
		), ErrRelocateStateUnknown)
	}
	g.relocationRecovery = nil
	g.releaseRelocationClaimLocked(&claim)
	return nil
}

// completeRemovedRelocationClaim releases ownership after a descriptor-anchored
// delete has removed the claimed directory. Revalidating the pathname here would
// necessarily fail because successful deletion made it absent; ownership, not a
// fresh path lookup, is the completion proof.
func (g *GitWorktree) completeRemovedRelocationClaim(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.worktreePath == claim.Path && g.relocationRecovery != nil &&
		g.relocationRecovery.State == RelocationRecoveryCleanupFinalizing &&
		g.relocationRecovery.IdentityKnown && g.relocationRecovery.identity().same(claim.identity) &&
		g.relocationRecovery.CleanupGeneration == claim.cleanupGeneration {
		if g.ownsRelocationClaimLocked(claim) {
			g.relocationRecovery = nil
			g.releaseRelocationClaimLocked(&claim)
		}
		// If the caller deadline already released ownership, keep the durable
		// finalization fence. A later live retry can consume the now-absent root
		// safely; clearing here would route it through ordinary pathname cleanup.
		return nil
	}
	if g.worktreePath == claim.Path && g.ownsRelocationClaimLocked(claim) && g.relocationRecovery == nil {
		g.releaseRelocationClaimLocked(&claim)
		return nil
	}
	if g.worktreePath == claim.Path && g.relocationRecovery != nil &&
		g.relocationRecovery.State == RelocationRecoveryCleanupStalled &&
		g.relocationRecovery.IdentityKnown && g.relocationRecovery.identity().same(claim.identity) &&
		g.relocationRecovery.CleanupGeneration == claim.cleanupGeneration {
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
// durable obligation which only the kill cleanup path may consume. The old
// recovery record was already cleared when the claim was resolved; this record
// represents the later destructive decision and keeps its identity across the
// time between a repo-gone restore and an explicit kill.
func (g *GitWorktree) PrepareRelocationClaimForCleanup(claim RelocationClaim) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if err := g.revalidateRelocationClaimLocked(claim, false); err != nil {
		return err
	}
	generation, err := boundedCleanupGenerationInstall(claim.Path, claim.identity)
	if err != nil {
		g.recordStaleClaimLocked(claim)
		return errors.Join(fmt.Errorf(
			"cannot establish durable cleanup generation at %s: %w", claim.Path, err,
		), ErrRelocateStateUnknown)
	}
	claim.cleanupAuthorized = true
	claim.cleanupGeneration = generation
	recovery := recoveryFromClaim(claim)
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
	if g.relocationRecovery == nil ||
		(g.relocationRecovery.State != RelocationRecoveryCleanupReady &&
			g.relocationRecovery.State != RelocationRecoveryCleanupFinalizing) {
		return errors.Join(fmt.Errorf("worktree has no cleanup-ready relocation identity"), ErrRelocateStateUnknown)
	}
	recovery := g.relocationRecovery
	identity, err := boundedRelocationPathIdentity(g.worktreePath)
	if recovery.State == RelocationRecoveryCleanupFinalizing {
		// This state was persisted only after the descriptor walk verified the
		// claimed root empty. Exact identity means the empty marker remains;
		// absence or replacement means there is nothing left that this row owns.
		// Operational non-answers still fail closed because they cannot distinguish
		// those cases from an unreadable exact marker.
		if errors.Is(err, os.ErrNotExist) || (err == nil && !recovery.identity().same(identity)) {
			return nil
		}
		if err == nil {
			inspection, inspectionErr := boundedFinalizingCleanupIdentity(g.worktreePath)
			if inspectionErr != nil {
				return errors.Join(fmt.Errorf(
					"cannot inspect exact finalizing cleanup root at %s: %w", g.worktreePath, inspectionErr,
				), ErrRelocateStateUnknown)
			}
			if !recovery.identity().same(inspection.identity) ||
				inspection.generation != recovery.CleanupGeneration {
				return errors.Join(fmt.Errorf(
					"exact finalizing cleanup root at %s changed during admission", g.worktreePath,
				), ErrRelocateStateUnknown)
			}
			if !inspection.empty {
				return errors.Join(fmt.Errorf(
					"finalizing cleanup root %s was repopulated before the kill commit", g.worktreePath,
				), ErrRelocateStateUnknown)
			}
			return nil
		}
		return errors.Join(fmt.Errorf(
			"cannot inspect finalizing cleanup root %s: %w", g.worktreePath, err,
		), ErrRelocateStateUnknown)
	}
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
	if generationErr := requireCleanupPathIdentity(
		g.worktreePath, recovery.identity(), recovery.CleanupGeneration,
	); generationErr != nil {
		recovery.State = RelocationRecoveryClaimStale
		return errors.Join(fmt.Errorf(
			"worktree path %s no longer carries its durable cleanup generation: %w",
			g.worktreePath, generationErr,
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
	if claim.cleanupAuthorized {
		if err := requireCleanupPathIdentity(claim.Path, claim.identity, claim.cleanupGeneration); err != nil {
			g.recordStaleClaimLocked(claim)
			return errors.Join(fmt.Errorf(
				"worktree path %s no longer carries its durable cleanup generation: %w", claim.Path, err,
			), ErrRelocateStateUnknown)
		}
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
func (g *GitWorktree) finishRelocationClaim(claim RelocationClaim, dest string, archiveReport *ArchiveReport) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	g.setWorktreeLocationLocked(dest)
	if archiveReport != nil {
		g.replaceArchiveReportLocked(*archiveReport)
	}
	g.relocationRecovery = nil
	g.releaseRelocationClaimLocked(&claim)
}

// checkpointRelocationPublication makes the filesystem publication and its
// durable in-memory ownership one snapshot boundary. Persistence takes the same
// lock, so it can observe either the source claim before publish or the committed
// destination, recovery identity, and archive report after publish — never the
// published bytes with their retained-tree handle still caller-local.
func (g *GitWorktree) checkpointRelocationPublication(
	claim RelocationClaim,
	dest, source string,
	identity pathIdentity,
	archiveReport *ArchiveReport,
	publish func() error,
) error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if err := publish(); err != nil {
		return err
	}
	g.setWorktreeLocationLocked(dest)
	if archiveReport != nil {
		g.replaceArchiveReportLocked(*archiveReport)
	}
	g.relocationRecovery = &RelocationRecovery{
		State:         RelocationRecoveryMoveUnknown,
		AlternatePath: source,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
	}
	g.releaseRelocationClaimLocked(&claim)
	return nil
}

func (g *GitWorktree) setWorktreeLocationLocked(dest string) {
	g.worktreePath = dest
	g.worktreeDir = filepath.Dir(dest)
}
