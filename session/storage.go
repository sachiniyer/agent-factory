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

	Program   string          `json:"program"`
	Worktree  GitWorktreeData `json:"worktree"`
	PRInfo    PRInfoData      `json:"pr_info,omitempty"`
}

// PRInfoData represents the serializable data of a PRInfo
type PRInfoData struct {
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// GitWorktreeData represents the serializable data of a GitWorktree
type GitWorktreeData struct {
	RepoPath         string `json:"repo_path"`
	WorktreePath     string `json:"worktree_path"`
	SessionName      string `json:"session_name"`
	BranchName       string `json:"branch_name"`
	BaseCommitSHA    string `json:"base_commit_sha"`
	ExternalWorktree bool   `json:"external_worktree,omitempty"`
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

// SaveInstances saves the list of instances to disk.
func (s *Storage) SaveInstances(instances []*Instance) error {
	// Convert instances to InstanceData
	data := make([]InstanceData, 0)
	for _, instance := range instances {
		if instance.Started() {
			data = append(data, instance.ToInstanceData())
		}
	}

	if s.repoID != "" {
		// TUI mode: the sidebar is the source of truth; external instances
		// are already pulled in by the periodic refreshExternalInstances tick.
		jsonData, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal instances: %w", err)
		}
		return s.state.SaveInstances(s.repoID, jsonData)
	}

	// Daemon mode: group by repo and save each group separately
	grouped := make(map[string][]InstanceData)
	for _, d := range data {
		rid := config.RepoIDFromRoot(d.Worktree.RepoPath)
		grouped[rid] = append(grouped[rid], d)
	}
	for rid, group := range grouped {
		jsonData, err := json.Marshal(group)
		if err != nil {
			return fmt.Errorf("failed to marshal instances for repo %s: %w", rid, err)
		}
		if err := s.state.SaveInstances(rid, jsonData); err != nil {
			return err
		}
	}
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
