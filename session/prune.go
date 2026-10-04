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
// its archived worktree and capture files are gone and only its git branch
// remains. It stays LiveArchived on the liveness axis — the tombstone is a
// deletion marker layered on the archived state, not a new liveness.
func (i *Instance) IsPruned() bool {
	return !i.PrunedAt().IsZero()
}

// MarkPruned records that this session's archived worktree and capture files
// were deleted. Called only by the daemon's prune path, after the deletions
// commit and inside the same critical section the tombstone persist follows:
// the marker must never lead the physical deletion, or a crash could leave a
// tombstoned row whose files still exist — unrestorable AND undeletable by a
// later run, since prune skips already-marked rows. Passing the zero time
// clears the marker — the rollback for a tombstone persist that failed after
// the stamp was set, so the row stays eligible for the retry the error
// prescribes.
func (i *Instance) MarkPruned(at time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prunedAt = at
	i.touchLocked()
}

// ArchiveTimeFor resolves the timestamp the prune cutoff measures from: the
// recorded archive commit, or for rows written before archived_at existed the
// record's UpdatedAt — the best surviving approximation of when the archive
// happened (see InstanceData.ArchivedAt for why the fallback can only
// under-delete). CreatedAt is the floor for a record with neither.
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
		return "remote session — no local worktree or captures to reclaim"
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

// RecordedConversations collects every provider conversation identity a record
// carries: the Agent tab's current conversation, every tab's recorded
// conversation, and every completed handoff's outgoing conversation. Those ids
// are what prune uses to find the session's transcript files — the ids are
// uuids embedded in provider file names, so a conversation copied across
// account homes by an account swap is still found by name rather than by a
// path af would have to re-derive (#5136).
func RecordedConversations(data InstanceData) []AgentConversationData {
	seen := make(map[string]struct{})
	var out []AgentConversationData
	add := func(conv AgentConversationData) {
		if !conv.HasID() {
			return
		}
		key := conv.Agent + "\x00" + conv.ID
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, conv)
	}
	if data.AgentConversation != nil {
		add(*data.AgentConversation)
	}
	for _, tab := range data.Tabs {
		if tab.Conversation != nil {
			add(*tab.Conversation)
		}
		for _, h := range tab.Handoffs {
			add(h.From)
		}
	}
	return out
}
