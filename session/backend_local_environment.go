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

// refreshSessionEnvironment reapplies a session's environment declarations to
// its agent pane. The account's namespace is the one the selection was made in,
// never re-derived from the launch command: a program_overrides edit after the
// pin can resolve the recorded Program to another agent's command, and a
// namespace re-derived from that command would look the same account label up
// in a different agent's registry (#4430 review round 4). When the declaration
// and the launch disagree, prepareLaunchEnvironment's namespace check is what
// refuses — so the pin must be the durable one.
func refreshSessionEnvironment(i *Instance, tmuxSession *tmux.TmuxSession) error {
	if err := tmuxSession.SetEnvPassthrough(sessionEnvPassthroughForInstance(i)); err != nil {
		return fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	// Refreshed alongside the pass-through, on the same paths, so a restored or
	// re-provisioned session carries the account it was created with rather than
	// quietly reverting to the ambient identity (#3051).
	i.mu.RLock()
	account := i.Account
	accountAgent := i.accountNamespaceLocked()
	i.mu.RUnlock()
	tmuxSession.SetAccountForAgent(accountAgent, account)
	return nil
}

// refreshTabSessionEnvironment is the sibling-tab half: a shell or process tab
// carries the SESSION's account scope in the account's own selection namespace
// — the sibling's own program (a shell, a dev server) is not the namespace the
// account belongs to.
func refreshTabSessionEnvironment(i *Instance, tab *Tab) error {
	if tab == nil || tab.tmux == nil {
		return nil
	}
	if err := tab.tmux.SetEnvPassthrough(sessionEnvPassthroughForInstance(i)); err != nil {
		return fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	i.mu.RLock()
	account := i.Account
	agent := i.accountNamespaceLocked()
	i.mu.RUnlock()
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
