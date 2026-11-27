package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
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
		if err := retireMoveInFlightStaging(recovery); err != nil {
			return RelocationClaim{}, err
		}
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
			if err := retireMoveInFlightStaging(recovery); err != nil {
				return RelocationClaim{}, err
			}
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

// retireMoveInFlightStaging removes the `.af-copy-*` staged copy a retiring
// move_in_flight record still names (N1): the record may retire only once
// the residue it names is gone — a successful retry never re-names the old
// staging, and an unnamed `.af-copy-*` degrades to leaf inference that
// refuses this session's archives and restores once its leaf is pruned.
// Resolution therefore removes its own staging itself. The copy's bytes are
// never needed in a matching branch: it runs only after the source's
// identity claimed the recorded path (the worktree is intact at the source)
// or the published destination matched the recorded identity, so the staged
// tree is at most a duplicate of bytes that provably exist elsewhere.
//
// The ownership proof is the recorded name plus, when the second checkpoint
// had already landed, the recorded destination identity — it was captured
// from the staged tree before the publish rename, so a still-present staging
// directory that matches it is exactly what af staged. A crash before that
// checkpoint records no destination identity, and the unguessable af-private
// name the record carries is then the whole proof; either way the removal
// itself stays descriptor-anchored (removeOpenedDirectory rechecks the
// secured name against the opened tree) so a swap mid-removal refuses rather
// than deleting a replacement. A staging path that survives but mismatches a
// KNOWN destination identity is foreign and is never deleted — the record
// stays and the refusal directs the operator at it. Any removal failure also
// keeps the record: residue it names is never left anonymous.
func retireMoveInFlightStaging(recovery RelocationRecovery) error {
	staging := recovery.StagingPath
	if staging == "" {
		return nil
	}
	stagedIdentity, err := boundedRelocationPathIdentity(staging)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.Join(fmt.Errorf(
			"cannot verify whether the interrupted move's staged copy %s is still present: %w",
			staging, err,
		), ErrRelocateStateUnknown)
	}
	if recovery.DestIdentityKnown && !recovery.destIdentity().same(stagedIdentity) {
		return errors.Join(fmt.Errorf(
			"the interrupted move's staged-copy path %s now holds a directory af did not stage; "+
				"inspect it and remove it, then retry",
			staging,
		), ErrRelocateStateUnknown)
	}
	// The open and the recursive removal run under the caller's relocation
	// mutex, so they go through BoundedMutation: a staged copy on a stalled
	// mount cannot wedge every claim and snapshot behind the lock, and a
	// timed-out removal registers under the staging path so nothing retries
	// on it until the abandoned operation provably settles.
	if err := BoundedMutation(
		"retiring the interrupted move's staged copy "+staging,
		[]string{staging},
		func() error {
			parent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(staging), "staged copy parent")
			if err != nil {
				return fmt.Errorf(
					"the interrupted move's staged copy %s could not be reached for removal: %w",
					staging, err,
				)
			}
			defer parent.Close()
			stagedFd, err := unix.Open(
				staging,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
				0,
			)
			if err != nil {
				return fmt.Errorf(
					"the interrupted move's staged copy %s could not be opened without following links: %w",
					staging, err,
				)
			}
			stagedDir := os.NewFile(uintptr(stagedFd), staging)
			defer stagedDir.Close()
			// The identity check above ran before this open; a foreign
			// replacement could have swapped in between. When the second
			// checkpoint recorded the destination identity, the OPENED
			// tree must still carry it — removeOpenedDirectory alone only
			// compares the open tree with its current pathname.
			if recovery.DestIdentityKnown {
				opened, err := identityFromFile(stagedDir)
				if err != nil {
					return fmt.Errorf(
						"the interrupted move's staged copy %s could not be verified after opening: %w",
						staging, err,
					)
				}
				if !recovery.destIdentity().same(opened) {
					return fmt.Errorf(
						"the interrupted move's staged-copy path %s was replaced between verification and removal; inspect it and remove it, then retry",
						staging,
					)
				}
			}
			if err := removeOpenedDirectory(parent, filepath.Base(staging), staging, stagedDir, nil); err != nil {
				return fmt.Errorf(
					"the interrupted move left its staged copy at %s and it could not be removed: %w",
					staging, err,
				)
			}
			return nil
		},
	); err != nil {
		return errors.Join(err, ErrRelocateStateUnknown)
	}
	return nil
}
