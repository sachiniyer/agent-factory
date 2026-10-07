package session

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// This file holds the session-domain half of `af sessions prune` (#5136): the
// durable archive/prune timestamps, the record-level eligibility rules, and
// the small filesystem helpers the daemon composes under its own locks. The
// daemon owns concurrency (per-session operation locks, the killsInFlight
// claim, persistence ordering); everything here is deliberately a pure
// function of an InstanceData plus the filesystem so the rules are testable
// without a daemon and cannot drift between the dry-run and apply passes.

// ArchivedAt returns when this session's archive committed. Zero for a row
// that was never archived (or archived before the field existed, where the
// prune cutoff falls back to UpdatedAt — see InstanceData.ArchivedAt).
func (i *Instance) ArchivedAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.archivedAt
}

// PrunedAt returns when this session's files were deleted by
// `af sessions prune --apply`. Zero means unpruned.
func (i *Instance) PrunedAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.prunedAt
}

// IsPruned reports whether this row is a prune tombstone: still listed, but
// its archived worktree is gone and only its git branch remains. It stays
// LiveArchived on the liveness axis — the tombstone is a deletion marker
// layered on the archived state, not a new liveness.
func (i *Instance) IsPruned() bool {
	return !i.PrunedAt().IsZero()
}

// MarkPruned records that this session's archived worktree was deleted.
// Called only by the daemon's prune path, after the deletion commits and
// inside the same critical section the tombstone persist follows:
// the marker must never lead the physical deletion, or a crash could leave a
// tombstoned row whose files still exist — unrestorable AND undeletable by a
// later run, since prune skips already-marked rows. UnmarkPruned is the
// rollback for a tombstone persist that failed after the stamp was set, so
// the row stays eligible for the retry the error prescribes.
func (i *Instance) MarkPruned(at time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prunedAt = at
	i.touchLocked()
}

// UnmarkPruned rolls a tombstone write back after its persistence failed:
// clears the marker AND restores updated_at, which MarkPruned advanced.
// ArchiveTimeFor falls back to updated_at for pre-upgrade rows, so leaving
// the stamp's time would mis-date the archive on the retry the error
// prescribes — the row would report "archived too recently" and the
// tombstone could never be finished.
func (i *Instance) UnmarkPruned(restoreUpdatedAt time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prunedAt = time.Time{}
	i.UpdatedAt = restoreUpdatedAt
}

// ReconcilePrunedSnapshot adopts a snapshot's tombstone onto an already-open
// row — the prune can land between polls while the TUI holds the same
// Instance pointer. Monotonic like the kill tombstone's reconcile: a stale
// snapshot carrying zero never un-prunes a row the daemon already marked.
func (i *Instance) ReconcilePrunedSnapshot(prunedAt time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if prunedAt.IsZero() || !i.prunedAt.IsZero() {
		return false
	}
	i.prunedAt = prunedAt
	return true
}

// ArchiveTimeFor resolves the timestamp the prune cutoff measures from: the
// recorded archive commit, or for rows written before archived_at existed the
// record's UpdatedAt — the best surviving approximation of when the archive
// happened (see InstanceData.ArchivedAt for why the fallback can only
// under-delete). CreatedAt remains only as a degenerate floor for display —
// PruneSkipReason refuses rows with no provable archive time before this is
// ever consulted on a candidate.
func ArchiveTimeFor(data InstanceData) time.Time {
	switch {
	case !data.ArchivedAt.IsZero():
		return data.ArchivedAt
	case !data.UpdatedAt.IsZero():
		return data.UpdatedAt
	default:
		return data.CreatedAt
	}
}

// PruneSkipReason answers why a session record is NOT a prune candidate, or ""
// when it is eligible for `af sessions prune` against the given archive-time
// cutoff. It evaluates only what the record proves — the daemon additionally
// re-checks the live claims (operation lock, killsInFlight, pending captures)
// before applying.
//
// The order is deliberate: structural disqualifiers come before the age test
// so a row that could never be pruned does not also report a confusing age
// verdict, and the tombstone check comes first so a second --apply reports
// "already pruned" rather than re-evaluating a hollowed-out record.
func PruneSkipReason(data InstanceData, archivedBefore time.Time) string {
	if !data.PrunedAt.IsZero() {
		return "already pruned"
	}
	if data.UserKilled {
		return "kill tombstone — teardown is still owed"
	}
	if liveness := livenessFromData(data); liveness != LiveArchived {
		return fmt.Sprintf("not archived (liveness %s)", livenessLabel(liveness))
	}
	if data.InFlightOp != OpNone {
		return fmt.Sprintf("operation in flight (%s)", opLabel(data.InFlightOp))
	}
	if data.StartupStateUnknown {
		return "startup state unknown — af could not confirm which runtime owns its workspace"
	}
	if IsReservedTitle(data.Title) {
		return "reserved session title"
	}
	if !data.UsesLocalTmux() {
		return "remote session — no local worktree to reclaim"
	}
	if data.Worktree.ExternalWorktree {
		return "external (in-place) worktree is user-owned"
	}
	if data.Worktree.RelocationRecovery != nil {
		return "worktree relocation recovery is unresolved — the archive move did not provably finish"
	}
	if data.ArchiveReport != nil && !data.ArchiveReport.Empty() {
		return "incomplete archive — retained source trees are present and need manual review"
	}
	if data.Worktree.WorktreePath == "" {
		return "record carries no archived worktree path"
	}
	// A row with neither archived_at nor a genuine updated_at cannot prove
	// when it was archived. FromInstanceData synthesizes updated_at from
	// created_at on records that predate the field, and creation time is not
	// archive time: a session created long ago but archived recently would
	// pass a long cutoff on a fabricated age — exactly the rows a prune must
	// NOT delete early. Skip; the record needs an archived_at-bearing rewrite
	// (re-archive, or a daemon stamp) before it can be eligible.
	if data.ArchivedAt.IsZero() &&
		(data.UpdatedAt.IsZero() || data.UpdatedAt.Equal(data.CreatedAt)) {
		return "archive time cannot be proven — record predates archived_at and updated_at"
	}
	if at := ArchiveTimeFor(data); at.IsZero() || !at.Before(archivedBefore) {
		return fmt.Sprintf("archived too recently (%s)", ArchiveTimeFor(data).Format(time.RFC3339))
	}
	return ""
}

// DirSizeBytes sums the file sizes under root. Missing roots contribute zero —
// a worktree deleted out-of-band is "already reclaimed", not an error — while
// every other failure is reported so the dry-run total cannot silently
// understate what a later --apply would reclaim.
func DirSizeBytes(root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	var total int64
	var firstErr error
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				if path == root {
					return filepath.SkipDir
				}
				return nil // an entry deleted mid-walk is already reclaimed too
			}
			if firstErr == nil {
				firstErr = err
			}
			if path == root {
				return filepath.SkipDir
			}
			return nil
		}
		if d == nil {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, firstErr
}
