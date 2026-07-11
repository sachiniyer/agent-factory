package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// RemoteHooksConfiguredForPath reports whether absPath's repo has a validated
// remote hook backend configured. A repo with no remote hooks is a normal empty
// state, so it returns false, nil rather than an error.
func RemoteHooksConfiguredForPath(absPath string) (bool, error) {
	_, configured, err := loadHookBackendForPathIfConfigured(absPath)
	return configured, err
}

// loadHookBackendForPath loads a HookBackend for the given workspace path.
// Returns an error if no remote hooks are configured.
func loadHookBackendForPath(absPath string) (*HookBackend, error) {
	hook, configured, err := loadHookBackendForPathIfConfigured(absPath)
	if err != nil {
		return nil, err
	}
	if configured {
		return hook, nil
	}
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve repo: %w", err)
	}
	return nil, fmt.Errorf("no remote hooks configured for repo %s", repo.ID)
}

func loadHookBackendForPathIfConfigured(absPath string) (*HookBackend, bool, error) {
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return nil, false, fmt.Errorf("failed to resolve repo: %w", err)
	}
	cfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		return nil, false, fmt.Errorf("failed to resolve repo config: %w", err)
	}
	if cfg.RemoteHooks == nil {
		return nil, false, nil
	}
	if err := cfg.RemoteHooks.Validate(); err != nil {
		return nil, false, err
	}
	return &HookBackend{Hooks: *cfg.RemoteHooks}, true, nil
}
