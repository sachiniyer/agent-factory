package session

import (
	"encoding/json"
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"time"
)

// InstanceData represents the serializable data of an Instance
type InstanceData struct {
	Title     string    `json:"title"`
	Path      string    `json:"path"`
	Branch    string    `json:"branch"`
	Status    Status    `json:"status"`
	Height    int       `json:"height"`
	Width     int       `json:"width"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	AutoYes   bool      `json:"auto_yes"`

	Program     string                 `json:"program"`
	TmuxName    string                 `json:"tmux_name,omitempty"`
	Worktree    GitWorktreeData        `json:"worktree"`
	PRInfo      PRInfoData             `json:"pr_info,omitempty"`
	BackendType string                 `json:"backend_type,omitempty"`
	RemoteMeta  map[string]interface{} `json:"remote_meta,omitempty"`
}

// PRInfoData represents the serializable data of a PRInfo
type PRInfoData struct {
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// GitWorktreeData represents the serializable data of a GitWorktree.
//
// BranchCreatedByUs indicates whether the session created the underlying
// branch itself (vs. reused a pre-existing one). It is serialized via a
// pointer so that "missing" (nil, for data written before this field was
// added) can be distinguished from an explicit false. Missing values are
// treated as true to preserve the prior behavior for sessions that existed
// before this flag was introduced.
type GitWorktreeData struct {
	RepoPath          string `json:"repo_path"`
	WorktreePath      string `json:"worktree_path"`
	SessionName       string `json:"session_name"`
	BranchName        string `json:"branch_name"`
	BaseCommitSHA     string `json:"base_commit_sha"`
	ExternalWorktree  bool   `json:"external_worktree,omitempty"`
	BranchCreatedByUs *bool  `json:"branch_created_by_us,omitempty"`
}

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
// with the newest UpdatedAt (ties keep the earliest occurrence, so in-memory
// records — which both save paths place ahead of disk-only records — win).
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

// mergeInstancesWithDisk applies the save-merge rules shared by the TUI and
// daemon save paths to one repo's worth of state. It returns the records to
// persist; callers still dedupe (#808), marshal, and write under the lock.
//
//   - In-memory instances take precedence over their disk records.
//   - Loading instances are never persisted — their worktree is not yet
//     populated, so FromInstanceData cannot restore them, and an orphaned
//     record would block title reuse via the daemon's collision check (#551).
//   - Non-started instances are dropped.
//   - An in-memory instance whose disk record was removed by another process
//     AND whose backing session is dead is dropped instead of being
//     resurrected from stale memory (#819). If the session is still alive,
//     the record is rewritten — an externally wiped or truncated file must
//     not take live sessions down with it.
//   - Disk-only records are preserved as externally-added, except legacy
//     Loading ghosts (#551) and titles in knownTitles.
//
// knownTitles is the set of ALL in-memory titles for this repo, including
// non-started ones. It distinguishes "killed in this process" (the stale
// disk record must not be preserved) from "added externally on disk". The
// daemon passes its per-repo title set; the TUI passes nil because it
// deletes killed sessions from disk explicitly rather than via save-merge.
func mergeInstancesWithDisk(instances []*Instance, diskData []InstanceData, knownTitles map[string]bool) []InstanceData {
	diskTitles := make(map[string]bool, len(diskData))
	for _, disk := range diskData {
		diskTitles[disk.Title] = true
	}

	merged := make([]InstanceData, 0, len(instances)+len(diskData))
	memTitles := make(map[string]bool, len(instances))
	for _, instance := range instances {
		if instance.GetStatus() == Loading {
			continue
		}
		if !instance.Started() {
			continue
		}
		// If another process deleted the disk record and the backing
		// session is gone, don't resurrect it from stale memory.
		if !diskTitles[instance.Title] && !instance.TmuxAlive() {
			continue
		}
		merged = append(merged, instance.ToInstanceData())
		memTitles[instance.Title] = true
	}

	for _, disk := range diskData {
		if memTitles[disk.Title] {
			// Already covered by the in-memory version.
			continue
		}
		if knownTitles[disk.Title] {
			// Known in memory but filtered out above (e.g. killed).
			// Don't preserve the stale disk record.
			continue
		}
		// Disk-only Loading entries are stale pre-save records from a
		// start that never completed. External CLI/task creates are only
		// persisted after startup succeeds, so they arrive as non-Loading.
		if disk.Status == Loading {
			continue
		}
		// Externally added instance — keep it.
		merged = append(merged, disk)
	}
	return merged
}

// SaveInstances saves the list of instances to disk under file locks.
// Loading instances are excluded — they represent in-flight TUI session
// creations whose worktree is not yet populated, so they cannot be
// restored via FromInstanceData. refreshExternalInstances already
// protects in-memory Loading entries from being reaped, so they don't
// need a disk presence. Persisting them risked orphaned records that
// the daemon's title-collision check would treat as live (#551).
func (s *Storage) SaveInstances(instances []*Instance) error {
	if s.repoID != "" {
		return s.saveRepoInstances(instances)
	}

	// Daemon mode: group in-memory instances by repo root.
	// Prefer the worktree's resolved repo path so we share a repo ID with
	// the TUI even when the instance was created from a symlinked path;
	// fall back to Path for remote backends where Worktree.RepoPath is
	// empty. This mirrors CollectRepoRoots (#667). All instances are
	// grouped — including non-started ones — so repos whose sessions were
	// all killed are still visited and their records removed.
	//
	// The per-repo title sets must be scoped per-repo, not global: a
	// global set across all repos causes cross-repo title collisions to
	// drop legitimate externally-added instances from other repos (#198).
	grouped := make(map[string][]*Instance)
	repoTitles := make(map[string]map[string]bool)
	for _, inst := range instances {
		root := inst.GetRepoPath()
		if root == "" {
			root = inst.Path
		}
		rid := config.RepoIDFromRoot(root)
		grouped[rid] = append(grouped[rid], inst)
		if repoTitles[rid] == nil {
			repoTitles[rid] = make(map[string]bool)
		}
		repoTitles[rid][inst.Title] = true
	}

	// Merge each repo's in-memory state with disk state. Repos that exist
	// ONLY on disk (no in-memory instances at all) were never loaded by
	// the daemon; we never write to them, so they are naturally preserved.
	for rid, group := range grouped {
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := config.WithFileLock(path, func() error {
			// Read existing disk state inside the lock. A genuine read
			// failure (permission denied, I/O error) must abort this repo's
			// save: merging against the empty list GetInstances used to
			// return would overwrite instances.json and permanently drop
			// sessions that are present on disk but momentarily unreadable
			// (#766). Missing files still come back as "[]" with a nil error.
			diskJSON, err := s.state.GetInstances(rid)
			if err != nil {
				return fmt.Errorf("refusing to overwrite repo %s: failed to read existing instances: %w", rid, err)
			}
			var diskData []InstanceData
			if diskJSON != nil && string(diskJSON) != "[]" && string(diskJSON) != "null" {
				if err := json.Unmarshal(diskJSON, &diskData); err != nil {
					log.WarningLog.Printf("failed to parse disk instances for repo %s, overwriting: %v", rid, err)
					diskData = nil
				}
			}

			merged := mergeInstancesWithDisk(group, diskData, repoTitles[rid])

			jsonData, err := json.Marshal(dedupeInstanceData(merged))
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

func (s *Storage) saveRepoInstances(instances []*Instance) error {
	path, pathErr := config.RepoInstancesPath(s.repoID)
	if pathErr != nil {
		return pathErr
	}
	return config.WithFileLock(path, func() error {
		// A transient read failure must not be mistaken for "no sessions":
		// merging against an empty disk state and writing the result back
		// would clobber the unreadable-but-present instances.json (#766).
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return fmt.Errorf("refusing to overwrite repo %s: failed to read existing instances: %w", s.repoID, err)
		}
		var diskData []InstanceData
		if raw != nil && string(raw) != "[]" && string(raw) != "null" {
			if err := json.Unmarshal(raw, &diskData); err != nil {
				return fmt.Errorf("failed to parse existing instances for repo %s: %w", s.repoID, err)
			}
		}

		merged := mergeInstancesWithDisk(instances, diskData, nil)

		jsonData, err := json.Marshal(dedupeInstanceData(merged))
		if err != nil {
			return fmt.Errorf("failed to marshal instances: %w", err)
		}
		return s.state.SaveInstances(s.repoID, jsonData)
	})
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
		// Daemon mode: load all repos
		allJSON = s.state.GetAllInstances()
	}

	var instances []*Instance
	for _, jsonData := range allJSON {
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
			instance, err := FromInstanceData(data)
			if err != nil {
				// Instance's tmux session or worktree may have been
				// destroyed externally. Log and skip rather than
				// failing the entire load.
				log.WarningLog.Printf("skipping instance %q: %v", data.Title, err)
				continue
			}
			instances = append(instances, instance)
		}
	}

	return instances, nil
}

// DeleteInstance removes an instance from storage by filtering raw JSON
// directly, avoiding the need to reconstruct live Instance objects (which
// may fail if tmux/worktree has already been destroyed).
func (s *Storage) DeleteInstance(title string) error {
	path, pathErr := config.RepoInstancesPath(s.repoID)
	if pathErr != nil {
		return pathErr
	}
	return config.WithFileLock(path, func() error {
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
				found = true
				continue
			}
			filtered = append(filtered, d)
		}

		if !found {
			return fmt.Errorf("instance not found: %s", title)
		}

		out, err := json.Marshal(filtered)
		if err != nil {
			return fmt.Errorf("failed to marshal instances: %w", err)
		}
		return s.state.SaveInstances(s.repoID, out)
	})
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

// CollectRepoRoots returns the set of unique repo root paths referenced by
// stored instances across all repos. This is used by operations whose scope
// must span every repo with persisted state (e.g. `af reset` cleaning
// worktrees in all repos before deleting global instance storage).
//
// Instances without a usable repo path (e.g. certain remote backends where
// Worktree.RepoPath is empty and Path is not a local filesystem path) are
// skipped. Callers should treat the result as best-effort.
func (s *Storage) CollectRepoRoots() (map[string]struct{}, error) {
	roots := make(map[string]struct{})
	allJSON := s.state.GetAllInstances()
	for _, jsonData := range allJSON {
		if jsonData == nil || string(jsonData) == "[]" || string(jsonData) == "null" {
			continue
		}
		var instancesData []InstanceData
		if err := json.Unmarshal(jsonData, &instancesData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
		}
		for _, data := range instancesData {
			// Prefer the worktree's repo path; fall back to the
			// instance path (the repo the instance was created in).
			root := data.Worktree.RepoPath
			if root == "" {
				root = data.Path
			}
			if root == "" {
				continue
			}
			roots[root] = struct{}{}
		}
	}
	return roots, nil
}
