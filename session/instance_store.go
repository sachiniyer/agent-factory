package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The record STORE: reading, writing and deleting the persisted instance rows.
// storage.go next door owns the record SHAPE — InstanceData, the tab data types
// and the projections that narrow them for storage or for a client read. Split
// from that file when the two halves together crossed the 1000-line limit
// (#1145); nothing else moved.

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
// disk first. Only a sandbox archive snapshot re-reads the exact repo under its
// file lock, and only a newer committed outcome for the same stable identity can
// displace memory.
func (s *Storage) SaveInstances(instances []*Instance) error {
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
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := config.WithFileLock(path, func() error {
			checkpoint := snapshotInstancesForCheckpoint(repoInstances)
			if !checkpoint.shouldWrite() {
				return nil
			}
			group := checkpoint.group
			if checkpoint.needsArchiveReconcile() {
				raw, readErr := s.state.GetInstances(rid)
				if readErr != nil {
					return fmt.Errorf("reconcile: read disk for repo %s: %w", rid, readErr)
				}
				if len(raw) > 0 {
					var onDisk []InstanceData
					if jsonErr := json.Unmarshal(raw, &onDisk); jsonErr == nil {
						reconcilePendingArchiveRows(group, checkpoint.inFlightArchive, onDisk)
						group = mergeCommittedArchiveRows(group, onDisk, checkpoint.prePushArchive)
					}
				}
			}
			jsonData, err := json.Marshal(dedupeInstanceData(group))
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

// LoadInstances loads the list of instances from disk.
func (s *Storage) LoadInstances() ([]*Instance, error) {
	var allJSON map[string]json.RawMessage
	if s.repoID != "" {
		// TUI mode: load just this repo. Surface read errors so startup can
		// report "couldn't read your sessions" instead of silently showing
		// an empty list that looks like a fresh install (#766).
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return nil, err
		}
		allJSON = map[string]json.RawMessage{s.repoID: raw}
	} else {
		// Daemon mode: load all repos. Surface a directory-level read error so
		// the daemon reports "couldn't read your sessions" instead of silently
		// presenting an empty list that looks like a fresh install while live
		// sessions sit unreadable on disk (#868).
		all, err := s.state.GetAllInstances()
		if err != nil {
			return nil, err
		}
		allJSON = all
	}

	var instances []*Instance
	for repoID, jsonData := range allJSON {
		if jsonData == nil || string(jsonData) == "[]" || string(jsonData) == "null" {
			continue
		}
		var instancesData []InstanceData
		if err := json.Unmarshal(jsonData, &instancesData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
		}
		// Collapse duplicate records written before the dedup-on-save fix
		// (#808) so a dup-containing file yields one sidebar row per session
		// immediately, not just after the next save rewrites the file.
		instancesData = dedupeInstanceData(instancesData)
		for _, data := range instancesData {
			data = data.ForStorage()
			instance, err := FromInstanceData(data)
			if err != nil {
				// Instance's tmux session or worktree may have been
				// destroyed externally. Log and skip rather than
				// failing the entire load.
				log.WarningLog.Printf("skipping instance %q: %v", data.Title, err)
				continue
			}
			instance.PinStorageRepoID(repoID)
			instances = append(instances, instance)
		}
	}

	return instances, nil
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

// DeleteInstanceByStableID removes an instance from storage only when the
// record still matches the stable session identity captured by the caller. A
// false nil result means a same-titled record exists but belongs to a different
// instance, so the caller must treat the delete as stale and leave it alone.
// Empty IDs are legacy-compatible and fall back to title matching.
//
// It takes the instances flock with a DEADLINE (config.WithFileLockTimeout), not
// the blocking WithFileLock every other Storage writer uses: a contended lock
// surfaces as a retryable config.ErrLockTimeout instead of parking the caller
// forever. See InstanceDeleteLockTimeout for why this writer in particular
// cannot afford an unbounded wait.
func (s *Storage) DeleteInstanceByStableID(title, id string) (bool, error) {
	path, pathErr := config.RepoInstancesPath(s.repoID)
	if pathErr != nil {
		return false, pathErr
	}
	deleted := false
	sameTitleDifferentID := false
	if err := config.WithFileLockTimeout(path, InstanceDeleteLockTimeout, func() error {
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return err
		}
		if raw == nil || string(raw) == "[]" || string(raw) == "null" {
			return fmt.Errorf("instance not found: %s", title)
		}

		var data []InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("failed to parse instances: %w", err)
		}

		filtered := make([]InstanceData, 0, len(data))
		found := false
		for _, d := range data {
			if d.Title == title {
				if stableIDMatches(d.ID, id) {
					found = true
					deleted = true
					continue
				}
				sameTitleDifferentID = true
			}
			filtered = append(filtered, d)
		}

		if !found {
			if sameTitleDifferentID {
				return nil
			}
			return fmt.Errorf("instance not found: %s", title)
		}

		out, err := json.Marshal(filtered)
		if err != nil {
			return fmt.Errorf("failed to marshal instances: %w", err)
		}
		return s.state.SaveInstances(s.repoID, out)
	}); err != nil {
		return false, err
	}
	return deleted, nil
}

func stableIDMatches(recordID, expectedID string) bool {
	return expectedID == "" || recordID == "" || recordID == expectedID
}

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
