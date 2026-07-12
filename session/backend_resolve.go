package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// RemoteHooksConfiguredForPath reports whether absPath's repo has a validated
// remote hook backend configured. A repo with no remote hooks is a normal empty
// state, so it returns false, nil rather than an error.
func RemoteHooksConfiguredForPath(absPath string) (bool, error) {
	_, configured, err := loadRemoteHooksForPathIfConfigured(absPath)
	return configured, err
}

// loadRemoteHooksForPath loads the validated RemoteHooks for the given workspace
// path (#1592 Phase 4 PR7). Returns an error if no remote hooks are configured
// or if the config still carries the removed pre-PR7 keys (RemoteHooks.Validate
// surfaces the migration message). The hookRuntime provisions from this config.
func loadRemoteHooksForPath(absPath string) (config.RemoteHooks, error) {
	hooks, configured, err := loadRemoteHooksForPathIfConfigured(absPath)
	if err != nil {
		return config.RemoteHooks{}, err
	}
	if configured {
		return hooks, nil
	}
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return config.RemoteHooks{}, fmt.Errorf("failed to resolve repo: %w", err)
	}
	return config.RemoteHooks{}, fmt.Errorf("no remote hooks configured for repo %s", repo.ID)
}

func loadRemoteHooksForPathIfConfigured(absPath string) (config.RemoteHooks, bool, error) {
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return config.RemoteHooks{}, false, fmt.Errorf("failed to resolve repo: %w", err)
	}
	cfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		return config.RemoteHooks{}, false, fmt.Errorf("failed to resolve repo config: %w", err)
	}
	if cfg.RemoteHooks == nil {
		return config.RemoteHooks{}, false, nil
	}
	if err := cfg.RemoteHooks.Validate(); err != nil {
		return config.RemoteHooks{}, false, err
	}
	return *cfg.RemoteHooks, true, nil
}
