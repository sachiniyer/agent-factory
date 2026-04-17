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

// SaveInstances saves the list of instances to disk under file locks.
// Includes both started instances and Loading instances (which are in the
// process of being started asynchronously). This ensures preSaveInstances
// persists Loading instances to disk so refreshExternalInstances won't
// remove them during the Loading→Running transition.
func (s *Storage) SaveInstances(instances []*Instance) error {
	// Convert instances to InstanceData
	data := make([]InstanceData, 0)
	for _, instance := range instances {
		if instance.Started() || instance.Status == Loading {
			data = append(data, instance.ToInstanceData())
		}
	}

	if s.repoID != "" {
		// TUI mode: the sidebar is the source of truth; external instances
		// are already pulled in by the periodic refreshExternalInstances tick.
		path, pathErr := config.RepoInstancesPath(s.repoID)
		if pathErr != nil {
			return pathErr
		}
		return config.WithFileLock(path, func() error {
			jsonData, err := json.Marshal(data)
			if err != nil {
				return fmt.Errorf("failed to marshal instances: %w", err)
			}
			return s.state.SaveInstances(s.repoID, jsonData)
		})
	}

	// Daemon mode: group in-memory instances by repo.
	// Use d.Path (always set) rather than d.Worktree.RepoPath (empty for
	// remote backends) so the grouping key is consistent with knownRepoIDs.
	grouped := make(map[string][]InstanceData)
	for _, d := range data {
		rid := config.RepoIDFromRoot(d.Path)
		grouped[rid] = append(grouped[rid], d)
	}

	// Collect repo IDs that the daemon knows about so we visit repos
	// that had all their in-memory instances killed (group would be empty).
	knownRepoIDs := make(map[string]bool)
	for _, inst := range instances {
		rid := config.RepoIDFromRoot(inst.Path)
		knownRepoIDs[rid] = true
	}

	// Merge each repo's in-memory state with disk state.
	for rid := range knownRepoIDs {
		group := grouped[rid] // may be nil if all instances for this repo were killed

		// Build a per-repo set of ALL in-memory instance titles (including
		// non-started/non-loading ones) belonging to THIS repo. This is used
		// to distinguish "killed in daemon" from "added externally on disk".
		// IMPORTANT: this must be scoped per-repo. Using a global set across
		// all repos causes cross-repo title collisions to drop legitimate
		// externally-added instances from other repos (issue #198).
		repoMemTitlesAll := make(map[string]bool)
		for _, inst := range instances {
			if config.RepoIDFromRoot(inst.Path) == rid {
				repoMemTitlesAll[inst.Title] = true
			}
		}

		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := config.WithFileLock(path, func() error {
			// Read existing disk state inside the lock.
			diskJSON := s.state.GetInstances(rid)
			var diskData []InstanceData
			if diskJSON != nil && string(diskJSON) != "[]" && string(diskJSON) != "null" {
				if err := json.Unmarshal(diskJSON, &diskData); err != nil {
					log.WarningLog.Printf("failed to parse disk instances for repo %s, overwriting: %v", rid, err)
					diskData = nil
				}
			}

			// Build set of in-memory titles for this repo's group for
			// quick lookup when replacing disk entries.
			memTitles := make(map[string]bool, len(group))
			for _, d := range group {
				memTitles[d.Title] = true
			}

			// Start with the in-memory instances (they take precedence).
			merged := make([]InstanceData, 0, len(group)+len(diskData))
			merged = append(merged, group...)

			// Preserve disk-only instances that were NOT known to the
			// daemon (i.e. added externally while the daemon was running).
			for _, dd := range diskData {
				if memTitles[dd.Title] {
					// Already covered by the in-memory version.
					continue
				}
				if repoMemTitlesAll[dd.Title] {
					// The daemon knew about this instance in THIS repo but
					// it is no longer started/loading (e.g. killed). Don't
					// preserve it.
					continue
				}
				// Externally added instance — keep it.
				merged = append(merged, dd)
			}

			jsonData, err := json.Marshal(merged)
			if err != nil {
				return fmt.Errorf("failed to marshal instances for repo %s: %w", rid, err)
			}
			return s.state.SaveInstances(rid, jsonData)
		}); err != nil {
			return err
		}
	}

	// Handle repos that exist ONLY on disk (no in-memory instances at all).
	// These repos were never loaded by the daemon, so we must not touch them.
	// Since we only iterate knownRepoIDs above, disk-only repos are
	// naturally preserved (we never write to them).

	return nil
}

// LoadInstances loads the list of instances from disk.
func (s *Storage) LoadInstances() ([]*Instance, error) {
	var allJSON map[string]json.RawMessage
	if s.repoID != "" {
		// TUI mode: load just this repo
		raw := s.state.GetInstances(s.repoID)
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
		raw := s.state.GetInstances(s.repoID)
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
	raw := s.state.GetInstances(s.repoID)
	if raw == nil || string(raw) == "[]" || string(raw) == "null" {
		return nil, nil
	}
	var data []InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
	}
	return data, nil
}

// DeleteAllInstances removes all stored instances
func (s *Storage) DeleteAllInstances() error {
	return s.state.DeleteAllInstances()
}
