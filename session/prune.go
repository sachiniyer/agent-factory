package session

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// This file holds the session-domain half of `af sessions prune` (#5136): the
// archive timestamp, the record-level eligibility rules, and the filesystem
// helpers the daemon composes. Slice 1 is read-only, so everything here is a
// pure function of an InstanceData plus the filesystem — testable without a
// daemon, and incapable of drifting from what a future apply will refuse.

// ArchivedAt returns when this session's archive committed. Zero for a row
// that was never archived (or archived before the field existed, where the
// prune cutoff falls back to UpdatedAt — see InstanceData.ArchivedAt).
func (i *Instance) ArchivedAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.archivedAt
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
// checks the live claims (operation lock, killsInFlight, pending captures)
// and the deletion-boundary filesystem evidence when it lists candidates.
//
// The order is deliberate: structural disqualifiers come before the age test
// so a row that could never be reclaimed does not also report a confusing age
// verdict.
func PruneSkipReason(data InstanceData, archivedBefore time.Time) string {
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

// DirSizeBytes sums the ALLOCATED disk blocks under root (st_blocks × 512 —
// what `rm -rf` would actually free), not apparent file lengths: a sparse
// file's holes are never counted, and a hard-linked inode frees its blocks
// only when every link dies inside the deleted tree, so multiply-linked
// inodes are tracked and counted once — or not at all when a link survives
// outside root (#5136 Codex round 4). Missing roots contribute zero — a
// worktree deleted out-of-band is "already reclaimed", not an error — while
// every other failure is reported so the dry-run total cannot silently
// understate what a later --apply would reclaim.
func DirSizeBytes(root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	var total int64
	var firstErr error
	// inoKey → {blocks, nlink, links seen in-tree}. Only populated for
	// regular files with nlink>1; blocks are credited after the walk only
	// when no link to the inode survives outside root.
	type linkAcct struct {
		blocks int64
		nlink  uint64
		seen   uint64
	}
	linked := make(map[[2]uint64]*linkAcct)
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
		info, err := d.Info()
		if err != nil {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			// No stat_t (non-unix build): apparent size is the only measure
			// available — a coarse upper bound, still preferable to zero.
			total += info.Size()
			return nil
		}
		if info.Mode().IsRegular() && stat.Nlink > 1 {
			key := [2]uint64{uint64(stat.Dev), stat.Ino}
			acct := linked[key]
			if acct == nil {
				acct = &linkAcct{blocks: stat.Blocks, nlink: uint64(stat.Nlink)}
				linked[key] = acct
			}
			acct.seen++
			return nil
		}
		total += stat.Blocks * 512
		return nil
	})
	for _, acct := range linked {
		if acct.seen >= acct.nlink {
			total += acct.blocks * 512
		}
	}
	return total, firstErr
}
