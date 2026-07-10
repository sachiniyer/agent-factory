package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
)

var localSessionPreflight = preflight.LocalSessionPrereqs

func (m *home) preflightSessionCreate(instance *session.Instance) error {
	// Local-session prerequisites (the agent binary, etc.) only apply to a
	// backend that runs the agent on a local worktree.
	if instance == nil || instance.Capabilities().Workspace != session.WorkspaceLocalWorktree {
		return nil
	}
	cfg, err := m.preflightConfig()
	if err != nil {
		return err
	}
	return localSessionPreflight(cfg, m.pendingProgram)
}

func (m *home) preflightConfig() (*config.Config, error) {
	if m.repoRoot != "" {
		resolved, err := config.ResolveConfig(m.repoRoot)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve config before starting session: %w", err)
		}
		return &resolved.Config, nil
	}
	if m.appConfig != nil {
		return m.appConfig, nil
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config before starting session: %w", err)
	}
	return cfg, nil
}

func SetLocalSessionPreflightForTest(f func(*config.Config, string) error) func() {
	prev := localSessionPreflight
	localSessionPreflight = f
	return func() { localSessionPreflight = prev }
}
