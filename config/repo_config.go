package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const RepoConfigFileName = "config.json"

// RemoteHooks configures user-provided shell scripts for managing remote
// sessions. When present in a repo config, sessions for that repo use the
// remote hook backend instead of local tmux+git worktrees.
type RemoteHooks struct {
	LaunchCmd string `json:"launch_cmd"`
	ListCmd   string `json:"list_cmd"`
	AttachCmd string `json:"attach_cmd"`
	DeleteCmd string `json:"delete_cmd"`
}

// RepoConfig holds per-repository configuration.
type RepoConfig struct {
	// PostWorktreeCommands are shell commands run asynchronously in the worktree
	// directory after a new worktree is created.
	PostWorktreeCommands []string `json:"post_worktree_commands,omitempty"`
	// RemoteHooks, when set, causes all sessions for this repo to use the
	// remote hook backend.
	RemoteHooks *RemoteHooks `json:"remote_hooks,omitempty"`
}

// repoConfigPath validates repoID and returns the per-repo config file path.
// Mirrors the validation + containment guard from repoInstancesPath so the
// "repos/" tree is held to the same boundary as "instances/".
func repoConfigPath(repoID string) (string, string, error) {
	if err := ValidateRepoID(repoID); err != nil {
		return "", "", err
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return "", "", fmt.Errorf("failed to get config dir: %w", err)
	}
	parent := filepath.Join(configDir, "repos")
	dir := filepath.Join(parent, repoID)
	path := filepath.Join(dir, RepoConfigFileName)
	cleanParent := filepath.Clean(parent) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(path), cleanParent) {
		return "", "", fmt.Errorf("invalid repo id: resolved path escapes repos directory")
	}
	return dir, path, nil
}

// LoadRepoConfig loads the per-repo config for the given repo ID.
// Returns an empty config (not an error) if none exists.
func LoadRepoConfig(repoID string) (*RepoConfig, error) {
	_, path, err := repoConfigPath(repoID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &RepoConfig{}, nil
		}
		return nil, fmt.Errorf("failed to read repo config: %w", err)
	}
	var cfg RepoConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse repo config: %w", err)
	}
	return &cfg, nil
}

// SaveRepoConfig saves the per-repo config for the given repo ID.
func SaveRepoConfig(repoID string, cfg *RepoConfig) error {
	dir, path, err := repoConfigPath(repoID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create repo config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal repo config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
