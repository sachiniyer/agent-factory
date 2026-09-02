package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func configuredSessionEnvPassthrough(explicit []string) []string {
	names := append([]string(nil), explicit...)
	if cfg, err := config.LoadConfig(); err == nil && cfg != nil {
		names = append(names, cfg.SessionEnvPassthrough...)
	}
	normalized, _ := sessionenv.NormalizeExtraNames(names)
	return normalized
}

func sessionEnvPassthroughForInstance(i *Instance) []string {
	i.mu.RLock()
	explicit := append([]string(nil), i.sessionEnvPassthrough...)
	i.mu.RUnlock()
	if cfg := resolveConfigForInstance(i); cfg != nil {
		explicit = append(explicit, cfg.SessionEnvPassthrough...)
	}
	normalized, _ := sessionenv.NormalizeExtraNames(explicit)
	return normalized
}

func refreshSessionEnvironment(i *Instance, tmuxSession *tmux.TmuxSession) error {
	if err := tmuxSession.SetEnvPassthrough(sessionEnvPassthroughForInstance(i)); err != nil {
		return fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	// Refreshed alongside the pass-through, on the same paths, so a restored or
	// re-provisioned session carries the account it was created with rather than
	// quietly reverting to the ambient identity (#3051).
	i.mu.RLock()
	account := i.Account
	i.mu.RUnlock()
	tmuxSession.SetAccountForAgent(sessionenv.AgentForCommand(i.AgentProgram()), account)
	return nil
}

func refreshTabSessionEnvironment(i *Instance, tab *Tab) error {
	if tab == nil || tab.tmux == nil {
		return nil
	}
	if err := tab.tmux.SetEnvPassthrough(sessionEnvPassthroughForInstance(i)); err != nil {
		return fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	i.mu.RLock()
	account := i.Account
	i.mu.RUnlock()
	agent := sessionenv.AgentForCommand(i.AgentProgram())
	if tab.Kind == TabKindShell {
		if err := tab.tmux.SetAccountShellEnvironmentForAgent(agent, account); err != nil {
			return fmt.Errorf("prepare account-scoped shell: %w", err)
		}
		return nil
	}
	tab.tmux.SetAccountEnvironmentForAgent(agent, account)
	return nil
}

func refreshWorktreeEnvironment(i *Instance, worktree *git.GitWorktree) error {
	if worktree == nil {
		return nil
	}
	// The session's stable id, not its title: it names the transient scope a
	// daemon-spawned hook enters, and that name has to survive a rename and be
	// re-derivable by a later daemon generation (#3650). It changes nothing for a
	// TUI- or CLI-created worktree, where no scope is derived at all.
	worktree.SetHookScopeSessionID(i.ID)
	if err := worktree.SetHookEnvironment(sessionEnvPassthroughForInstance(i)); err != nil {
		return fmt.Errorf("invalid post-worktree environment pass-through: %w", err)
	}
	return nil
}
