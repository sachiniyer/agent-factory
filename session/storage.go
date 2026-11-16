package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// Storage handles saving and loading instances using the state interface.
// When repoID is set (TUI mode), operations are scoped to that repo.
// When repoID is empty (daemon mode), operations span all repos.
type Storage struct {
	state  config.InstanceStorage
	repoID string
}

// NewStorage creates a new storage instance.
// Pass a non-empty repoID for TUI (repo-scoped) mode, or "" for daemon (all-repo) mode.
func NewStorage(state config.InstanceStorage, repoID string) (*Storage, error) {
	return &Storage{
		state:  state,
		repoID: repoID,
	}, nil
}

// dedupeInstanceData collapses records that share a title, keeping the one
// with the latest session mutation time, UpdatedAt (legacy records carry their
// last save time). Ties keep the earliest occurrence, so in-memory
// records — which both save paths place ahead of disk-only records — win.
// Titles are unique per repo (the daemon's findTitleConflictLocked enforces
// this on create), so two same-title records in one repo's list are always
// the same logical session written twice (#808). Deduping at the save/load
// chokepoints prevents new duplicates from persisting and collapses any
// existing on-disk duplicate on the next clean save.
func dedupeInstanceData(data []InstanceData) []InstanceData {
	if len(data) < 2 {
		return data
	}
	index := make(map[string]int, len(data))
	out := make([]InstanceData, 0, len(data))
	for _, d := range data {
		if i, ok := index[d.Title]; ok {
			if d.UpdatedAt.After(out[i].UpdatedAt) {
				out[i] = d
			}
			continue
		}
		index[d.Title] = len(out)
		out = append(out, d)
	}
	return out
}

// SaveInstances persists the daemon's authoritative in-memory instances to
// disk, grouped by repo. As of #960 PR 4 the daemon is the SOLE writer of
// instances.json. It normally writes the manager's per-repo state directly;
// the narrow archive reconciliation below is the exception because targeted
// archive persistence and this shutdown checkpoint are two paths in that same
// writer. The old mergeInstancesWithDisk rule-zoo
// (#551/#766/#808/#819/#844/#959) remains gone.
//
// Only repos with at least one persistable in-memory instance are rewritten;
// repos the daemon holds nothing for are left untouched — their records were
// already removed by the targeted DeleteInstanceByStableID on kill, or were never loaded.
// Generic Loading/Deleting/non-started instances are skipped: their worktree is
// not yet populated (Loading) or is mid-teardown (Deleting), so FromInstanceData
// cannot restore them. Explicit durable retention markers override that legacy
// projection; in particular, a pending handoff names a live replacement and a
// staged archive report is the only durable handle to retained source trees.
//
// The targeted writers (appendInstanceData / persistInstanceData /
// DeleteInstanceByStableID) keep the disk current on every mutation; this full save is the
// shutdown checkpoint. Records are deduped by title (#808) before marshaling.
// Because the manager's memory is the source of truth, ordinary rows do not read
// disk first. One on-disk read remains, under the repo file lock, serving two
// preservation purposes for rows the in-memory snapshot cannot see or has
// dropped:
//
//   - A mid-archive sandbox snapshot re-reads the repo so a newer committed
//     archive outcome for the same stable identity can displace the stale
//     pre-push in-memory snapshot.
//   - Every checkpoint re-reads the repo so on-disk rows the daemon did NOT
//     materialize (unloadable "ghost" rows the loader deliberately left on
//     disk for repair, #4162/#1892/#3310) survive the wholesale rewrite. The
//     carve-out for an older binary's disposable Loading/Deleting projection
//     (#551) is reaped; durable ghosts are preserved verbatim.
func (s *Storage) SaveInstances(instances []*Instance) error {
	return s.saveInstances(instances, config.WithFileLock)
}

// SaveInstancesWithin is SaveInstances with a DEADLINE on every instances-flock
// wait, shared across all repos in the call: a lock holder that never lets go
// surfaces as config.ErrLockTimeout once budget is spent instead of parking the
// caller. Nothing is written for a repo whose lock was not acquired — the
// checkpoint never proceeds unlocked. Repos checkpointed before the timeout
// stay written. Callers on a path with its own budget (the daemon's missing-file
// restore, bounded by the poll/shutdown grace) use this form (#6048 review).
func (s *Storage) SaveInstancesWithin(instances []*Instance, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	return s.saveInstances(instances, func(path string, fn func() error) error {
		// A spent budget (0) is a genuinely non-blocking attempt: a free lock is
		// never refused for being late, a held one times out at once without
		// sleeping, so a pass over many repos cannot overrun the shared deadline.
		return config.WithFileLockTimeout(path, max(time.Until(deadline), 0), fn)
	})
}

// saveInstances is the checkpoint body; withLock is how each repo's instances
// flock is taken.
func (s *Storage) saveInstances(instances []*Instance, withLock func(path string, fn func() error) error) error {
	// Keep the live instances grouped by their stable storage repo. Each repo is
	// projected once to decide whether it has anything to checkpoint, then again
	// inside its file lock so an archive that starts after collection cannot be
	// written back as the stale Running snapshot (#3966405841).
	grouped := make(map[string][]*Instance)
	for _, inst := range instances {
		rid := inst.repoIDForStorage()
		grouped[rid] = append(grouped[rid], inst)
	}

	for rid, repoInstances := range grouped {
		initial := snapshotInstancesForCheckpoint(repoInstances)
		if !initial.shouldWrite() {
			continue
		}
		// inMemoryTitles is the set of titles the daemon holds in memory for
		// THIS repo. Titles are unique per repo (the daemon's
		// findTitleConflictLocked enforces this on create), so a title match is
		// a same-logical-session match. preserveUnmaterializedGhostRows uses it
		// to recognize an on-disk row that is the durable record for a live
		// (even if checkpoint-dropped) instance and must not be re-persisted as
		// a ghost — including a killed instance the snapshot legitimately
		// drops (#819/#844). Scoped per repo: a same-titled session in a
		// DIFFERENT repo is a distinct logical session and must not silence
		// this repo's ghost.
		inMemoryTitles := make(map[string]struct{}, len(repoInstances))
		for _, inst := range repoInstances {
			if inst.Title != "" {
				inMemoryTitles[inst.Title] = struct{}{}
			}
		}
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := withLock(path, func() error {
			checkpoint := snapshotInstancesForCheckpoint(repoInstances)
			if !checkpoint.shouldWrite() {
				return nil
			}
			group := checkpoint.group
			// Read the on-disk rows for this repo under the file lock. The
			// checkpoint rewrites the repo's instances.json wholesale from the
			// in-memory group, so without preserving rows the daemon did not
			// materialize the rewrite would silently erase every unloadable
			// ("ghost") row in the file — the opposite of the
			// durable-across-restarts promise the loader makes to those rows
			// (#4162/#1892/#3310). The archive reconciliation below used to be
			// the only on-disk read; this single read generalizes it so a mixed
			// repo's ghosts survive the shutdown checkpoint too. A read failure
			// propagates: silently proceeding would let the wholesale rewrite
			// clobber ghost rows (or a committed archive row) with the stale
			// in-memory snapshot, the exact data loss (#938) the file lock
			// exists to prevent.
			raw, readErr := s.state.GetInstances(rid)
			if readErr != nil {
				// Classify the read failure: a newer-schema file or an
				// I/O failure propagates so the checkpoint does not clobber
				// rows it cannot read (#4783/#766); a corrupt-but-readable
				// file falls back to overwrite (#938). GetInstances maps a
				// missing file to "[]".
				if err := checkpointReadError(readErr, path); err != nil {
					return fmt.Errorf("checkpoint: read disk for repo %s: %w", rid, err)
				}
				raw = nil
			}
			// onDisk is the decoded form used for matching (title), the
			// disposable-ghost carve-out, and archive reconciliation. onDiskRaw
			// is the parallel raw form preserved verbatim so additive fields a
			// newer binary wrote under the same schema version — which this
			// binary's InstanceData does not know and would silently drop on a
			// decode/encode round trip — survive the checkpoint (#6).
			onDisk, onDiskRaw := decodeOnDiskRows(raw)
			if checkpoint.needsArchiveReconcile() {
				reconcilePendingArchiveRows(group, checkpoint.inFlightArchive, onDisk)
				group = mergeCommittedArchiveRows(group, onDisk, checkpoint.prePushArchive)
			}
			// A same-titled on-disk row whose stable ID differs from the
			// in-memory instance is a replacement the daemon has not observed:
			// refreshDaemonInstances keeps the old in-memory pointer by title
			// even when the disk row's ID differs, so the checkpoint would write
			// the stale instance and clobber the replacement's identity — the
			// data-loss class persistInstanceData's title+ID guard exists to
			// prevent. Drop the stale in-memory row and let the on-disk
			// replacement survive the wholesale rewrite verbatim (retained below
			// outside the ghost reaper). An empty ID on either side is a legacy record and still
			// matches, mirroring stableIDMatchesForDaemon (#6048 review).
			//
			// Duplicate disk titles are resolved FIRST, newest UpdatedAt wins (ties
			// keep the earliest), the same rule dedupeInstanceData applies: only
			// that authoritative row is compared with the live instance, so an older
			// stale duplicate cannot supersede a current row matching it.
			authoritative := make(map[string]int)
			for i, d := range onDisk {
				if prev, ok := authoritative[d.Title]; !ok || d.UpdatedAt.After(onDisk[prev].UpdatedAt) {
					authoritative[d.Title] = i
				}
			}
			replaced := make(map[string]struct{})
			replacedID := make(map[string]string)
			for i := range group {
				if idx, ok := authoritative[group[i].Title]; ok && !stableIDMatches(onDisk[idx].ID, group[i].ID) {
					replaced[group[i].Title] = struct{}{}
					replacedID[group[i].Title] = group[i].ID
				}
			}
			var replacements []json.RawMessage
			if len(replaced) > 0 {
				// The conflicting on-disk row is retained UNCONDITIONALLY and
				// verbatim: handing it to preserveUnmaterializedGhostRows would let
				// that helper reap it as a disposable legacy transient when it is a
				// Loading/Deleting row with no retention marker, and with the stale
				// in-memory row already dropped the checkpoint would then lose both
				// records and orphan the replacement session (#6048 review).
				best := make(map[string]int)
				restDisk := make([]InstanceData, 0, len(onDisk))
				restRaw := make([]json.RawMessage, 0, len(onDisk))
				for i, d := range onDisk {
					if _, isReplaced := replaced[d.Title]; !isReplaced {
						restDisk = append(restDisk, d)
						if i < len(onDiskRaw) {
							restRaw = append(restRaw, onDiskRaw[i])
						}
						continue
					}
					if i != authoritative[d.Title] {
						continue // an older duplicate of the replacement: superseded
					}
					best[d.Title] = i
				}
				for _, i := range best {
					raw := json.RawMessage(nil)
					if i < len(onDiskRaw) {
						raw = onDiskRaw[i]
					} else {
						raw, _ = json.Marshal(onDisk[i])
					}
					replacements = append(replacements, stripRemovedInstanceDataWireKeys(raw))
				}
				onDisk, onDiskRaw = restDisk, restRaw
				kept := make([]InstanceData, 0, len(group))
				for _, g := range group {
					if _, drop := replaced[g.Title]; drop {
						continue
					}
					kept = append(kept, g)
				}
				group = kept
				for title := range replaced {
					delete(inMemoryTitles, title)
				}
			}
			ghosts := append(preserveUnmaterializedGhostRows(onDisk, onDiskRaw, inMemoryTitles), replacements...)
			jsonData, err := marshalCheckpointInstances(group, ghosts)
			if err != nil {
				return fmt.Errorf("failed to marshal instances for repo %s: %w", rid, err)
			}
			return s.state.SaveInstances(rid, jsonData)
		}); err != nil {
			return err
		}
	}

	return nil
}

// SaveInstancesForShutdown writes the daemon's terminal checkpoint. Sealing
// every instance first closes the post-snapshot admission race: an archive that
// has not begun is refused, while an archive already behind its BeginArchive
// fence is joined through its settling transition. The final snapshot therefore
// cannot precede an archive that the terminating process allowed to commit.
func (s *Storage) SaveInstancesForShutdown(instances []*Instance) error {
	settled := make([]<-chan struct{}, 0, len(instances))
	for _, inst := range instances {
		if archiveSettled := inst.sealArchiveCheckpoint(); archiveSettled != nil {
			settled = append(settled, archiveSettled)
		}
	}
	for _, archiveSettled := range settled {
		<-archiveSettled
	}
	return s.SaveInstances(instances)
}

// InstanceDeleteLockTimeout bounds how long DeleteInstanceByStableID waits for
// the per-repo instances flock. A var so tests can shorten it; production never
// reassigns.
//
// The delete is the LAST step of a session kill, and the daemon runs it holding
// that session's kill guard, so an unbounded wait here does not just stall one
// write — it strands a session whose kill-intent tombstone is already on disk,
// leaving it undeletable for the daemon's whole lifetime (#1917). The budget is
// generous: this lock is held only across a read-modify-write of one small JSON
// file, so exceeding it means a peer is genuinely wedged, not merely slow.
var InstanceDeleteLockTimeout = 10 * time.Second

// LoadInstanceData reads and unmarshals instance data from disk without
// constructing live Instance objects (no tmux session restoration).
// Used for lightweight comparison against in-memory state.
func (s *Storage) LoadInstanceData() ([]InstanceData, error) {
	raw, err := s.state.GetInstances(s.repoID)
	if err != nil {
		return nil, err
	}
	if raw == nil || string(raw) == "[]" || string(raw) == "null" {
		return nil, nil
	}
	var data []InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
	}
	return dedupeInstanceData(data), nil
}

// DeleteAllInstances removes all stored instances
func (s *Storage) DeleteAllInstances() error {
	return s.state.DeleteAllInstances()
}
