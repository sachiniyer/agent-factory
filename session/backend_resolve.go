package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// backendForPath returns the appropriate Backend for the given workspace path
// by resolving the repo's effective config (in-repo .agent-factory/config.json
// over the legacy per-repo location) and checking for remote hooks.
func backendForPath(absPath string) (Backend, error) {
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		// Not a git repo or can't resolve — default to local.
		return &LocalBackend{}, nil
	}
	cfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve repo config: %w", err)
	}
	if cfg.RemoteHooks != nil {
		if err := cfg.RemoteHooks.Validate(); err != nil {
			return nil, err
		}
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
	cfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve repo config: %w", err)
	}
	if cfg.RemoteHooks == nil {
		return nil, fmt.Errorf("no remote hooks configured for repo %s", repo.ID)
	}
	if err := cfg.RemoteHooks.Validate(); err != nil {
		return nil, err
	}
	return &HookBackend{Hooks: *cfg.RemoteHooks}, nil
}
