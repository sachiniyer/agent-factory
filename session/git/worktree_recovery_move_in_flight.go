package git

import (
	"errors"
	"fmt"
	"os"
)

// This file holds the durable move_in_flight record machinery of #7819: a
// cross-device move names its `.af-source-*` quarantine and `.af-copy-*`
// staging paths in a recovery record persisted BEFORE either can exist, so a
// crash at any point afterwards leaves residue an af-owned durable record
// already names — never residue that can only be guessed at through git's
// pruned-and-recycled worktree registration leaf.

// SetRelocationRecordCheckpoint installs the durable writer the cross-device
// move invokes after installing or updating its move_in_flight record — once
// before the copy begins (both residue paths are then named before any can
// exist) and once after the copy lands (the destination identity joins the
// record) but still before the quarantine rename (#7819). The daemon arms it
// with the session row's targeted persist; the returned restore removes the
// process-local closure after the operation.
func (g *GitWorktree) SetRelocationRecordCheckpoint(checkpoint func() error) func() {
	g.relocationMu.Lock()
	previous := g.relocationRecordCheckpoint
	g.relocationRecordCheckpoint = checkpoint
	g.relocationMu.Unlock()
	return func() {
		g.relocationMu.Lock()
		g.relocationRecordCheckpoint = previous
		g.relocationMu.Unlock()
	}
}

// recordMoveInFlight installs the move_in_flight durable record — or refreshes
// its destination identity once the staged copy exists — while the cross-device
// move still owns this worktree's claim (#7819). The recorded worktree path
// deliberately stays the source: the quarantine rename is the commit boundary,
// and until it lands the source is still the worktree. After the state update
// the installed durable checkpoint publishes the row, so a crash at any later
// point leaves residue a persisted record already names. A failed publish
// fails the move before the next byte is touched: residue without its record
// is exactly the gap this state exists to close.
func (g *GitWorktree) recordMoveInFlight(
	claim RelocationClaim,
	dest, stagingPath, quarantinePath string,
	destIdentity *pathIdentity,
) error {
	g.relocationMu.Lock()
	// The claim carrying this move must still be live. For a recovery-owned
	// claim that means it is the active owner; a record-free claim's ownership
	// is implicit and proven by the recorded path still naming its directory —
	// exactly the check revalidateRelocationClaimLocked makes, minus its
	// record-free requirement (the V2 update runs over the V1 record itself).
	if claim.Path == "" || g.worktreePath != claim.Path ||
		(claim.recoveryOwned && !g.ownsRelocationClaimLocked(claim)) {
		g.relocationMu.Unlock()
		return errors.Join(fmt.Errorf(
			"cannot record an in-flight move for an unowned relocation claim on %s", g.worktreePath,
		), ErrRelocateStateUnknown)
	}
	if g.relocationRecovery != nil &&
		g.relocationRecovery.State != RelocationRecoveryMoveInFlight {
		state := g.relocationRecovery.State
		g.relocationMu.Unlock()
		return errors.Join(fmt.Errorf(
			"cannot record an in-flight move over relocation recovery state %s", state,
		), ErrRelocateStateUnknown)
	}
	if g.relocationRecovery == nil {
		g.relocationRecovery = &RelocationRecovery{
			State:             RelocationRecoveryMoveInFlight,
			AlternatePath:     dest,
			IdentityKnown:     true,
			Device:            claim.identity.device,
			Inode:             claim.identity.inode,
			FileType:          claim.identity.fileType,
			QuarantinePath:    quarantinePath,
			StagingPath:       stagingPath,
			DestIdentityKnown: false,
		}
	}
	if destIdentity != nil {
		g.relocationRecovery.DestIdentityKnown = true
		g.relocationRecovery.DestDevice = destIdentity.device
		g.relocationRecovery.DestInode = destIdentity.inode
		g.relocationRecovery.DestFileType = destIdentity.fileType
	}
	checkpoint := g.relocationRecordCheckpoint
	g.relocationMu.Unlock()
	if checkpoint == nil {
		return nil
	}
	return checkpoint()
}

// DiscardUnarmedMoveInFlightRecord clears a move_in_flight record whose
// residue provably does not exist. A cross-device move that failed before it
// could strand either private tree — or whose failure path already cleaned
// the staging it made — must not keep the record (R3): master's error class
// for that failure is the plain error, a latched record would flip it to
// Unknown, and a surviving record would misattribute the same-named tree a
// later operation creates. An unverifiable probe counts as armed — residue
// the record might still name keeps it, and the failure stays Unknown.
func (g *GitWorktree) DiscardUnarmedMoveInFlightRecord() {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	recovery := g.relocationRecovery
	if recovery == nil || recovery.State != RelocationRecoveryMoveInFlight {
		return
	}
	for _, residue := range []string{recovery.QuarantinePath, recovery.StagingPath} {
		if residue == "" {
			continue
		}
		if _, err := boundedRelocationPathIdentity(residue); err == nil || !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	g.relocationRecovery = nil
}

// resolveMoveInFlightRecordLocked resolves the durable record a cross-device
// move persisted before its quarantine rename (#7819). The record is never
// resolved past a surviving quarantine: `.af-source-*` can be the only complete
// copy of the session's work (it survives whole when the copy skipped files),
// so its presence always keeps the record and refuses with the operator exit —
// move it back to the recorded path when the destination never published, or
// merge or remove it when the destination did, then retry. Otherwise the
// source identity claims the recorded path (crash before the rename, or the
// operator restored the quarantine), the staged destination identity relocates
// the record to the publish path (the publish rename landed before the crash),
// and anything else keeps the record and refuses.
func (g *GitWorktree) resolveMoveInFlightRecordLocked(primary string, recovery RelocationRecovery) (RelocationClaim, error) {
	if recovery.QuarantinePath != "" {
		_, quarantineErr := boundedRelocationPathIdentity(recovery.QuarantinePath)
		if quarantineErr == nil {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"an interrupted cross-device move left this session's complete source at %s; "+
					"move it back to %s if the move never published to %s, or inspect it, merge what %s is missing, and remove it, then retry",
				recovery.QuarantinePath, primary, recovery.AlternatePath, recovery.AlternatePath,
			), ErrRelocateStateUnknown)
		}
		if !errors.Is(quarantineErr, os.ErrNotExist) {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cannot verify whether the interrupted move's quarantine %s is still present: %w",
				recovery.QuarantinePath, quarantineErr,
			), ErrRelocateStateUnknown)
		}
	}
	primaryIdentity, primaryErr := boundedRelocationPathIdentity(primary)
	if primaryErr == nil && recovery.identity().same(primaryIdentity) {
		g.relocationRecovery = nil
		return g.activateRelocationClaimLocked(RelocationClaim{
			Path: primary, AlternatePath: recovery.AlternatePath, identity: primaryIdentity,
			quarantinePath: recovery.QuarantinePath, stagingPath: recovery.StagingPath,
		}), nil
	}
	if recovery.AlternatePath != "" && recovery.DestIdentityKnown {
		destIdentity, destErr := boundedRelocationPathIdentity(recovery.AlternatePath)
		if destErr == nil && recovery.destIdentity().same(destIdentity) {
			selected := recovery.AlternatePath
			g.setWorktreeLocationLocked(selected)
			g.relocationRecovery = nil
			return g.activateRelocationClaimLocked(RelocationClaim{
				Path: selected, AlternatePath: primary, identity: destIdentity,
				quarantinePath: recovery.QuarantinePath, stagingPath: recovery.StagingPath,
			}), nil
		}
		if destErr != nil && !errors.Is(destErr, os.ErrNotExist) {
			return RelocationClaim{}, errors.Join(fmt.Errorf(
				"cannot inspect the interrupted move's destination %s: %w",
				recovery.AlternatePath, destErr,
			), ErrRelocateStateUnknown)
		}
	}
	if primaryErr != nil && !errors.Is(primaryErr, os.ErrNotExist) {
		return RelocationClaim{}, errors.Join(fmt.Errorf(
			"cannot resolve the interrupted move: its recorded path %s could not be verified: %w",
			primary, primaryErr,
		), ErrRelocateStateUnknown)
	}
	return RelocationClaim{}, errors.Join(fmt.Errorf(
		"cannot resolve the interrupted move: neither its recorded path %s nor its destination %s carries the recorded identity",
		primary, recovery.AlternatePath,
	), ErrRelocateStateUnknown)
}
