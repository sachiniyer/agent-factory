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

func loadRemoteHooksForPathIfConfigured(absPath string) (config.RemoteHooks, bool, error) {
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return config.RemoteHooks{}, false, fmt.Errorf("failed to resolve repo: %w", err)
	}
	cfg, err := config.ResolveConfigForRepo(repo)
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
