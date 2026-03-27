package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// backendForPath returns the appropriate Backend for the given workspace path
// by checking whether a remote hooks config exists for the repo.
func backendForPath(absPath string) (Backend, error) {
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		// Not a git repo or can't resolve — default to local.
		return &LocalBackend{}, nil
	}
	return backendForRepoID(repo.ID)
}

// backendForRepoID returns the appropriate Backend for a given repo ID.
func backendForRepoID(repoID string) (Backend, error) {
	cfg, err := config.LoadRepoConfig(repoID)
	if err != nil {
		return nil, fmt.Errorf("failed to load repo config: %w", err)
	}
	if cfg.RemoteHooks != nil {
		return &HookBackend{Hooks: *cfg.RemoteHooks}, nil
	}
	return &LocalBackend{}, nil
}

// loadHookBackendForPath loads a HookBackend for the given workspace path.
// Returns an error if no remote hooks are configured.
func loadHookBackendForPath(absPath string) (*HookBackend, error) {
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve repo: %w", err)
	}
	cfg, err := config.LoadRepoConfig(repo.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to load repo config: %w", err)
	}
	if cfg.RemoteHooks == nil {
		return nil, fmt.Errorf("no remote hooks configured for repo %s", repo.ID)
	}
	return &HookBackend{Hooks: *cfg.RemoteHooks}, nil
}
