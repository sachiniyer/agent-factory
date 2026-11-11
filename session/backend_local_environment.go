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

// SessionEnvPassthroughForInstance returns the session's merged exact-name
// pass-through list — the same names its agent pane's default-deny allowlist
// admits — so a daemon-owned child that must hold the pane's boundary can ask
// for the identical extension (the VS Code editor, #4311).
//
// The list configures what a launched process may inherit, so it is a
// launch-class resolve: when the project's config cannot be read it fails with
// a *ProjectConfigUnknownError instead of answering the global config's list,
// which would silently drop the names the project adds (#7077).
func SessionEnvPassthroughForInstance(i *Instance) ([]string, error) {
	if frozen := i.frozenEnvPassthrough.Load(); frozen != nil {
		return append([]string(nil), (*frozen)...), nil
	}
	i.mu.RLock()
	explicit := append([]string(nil), i.sessionEnvPassthrough...)
	i.mu.RUnlock()
	cfg, err := resolveConfigForInstance(i)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		explicit = append(explicit, cfg.SessionEnvPassthrough...)
	}
	normalized, _ := sessionenv.NormalizeExtraNames(explicit)
	return normalized, nil
}

// freezeEnvPassthrough makes SessionEnvPassthroughForInstance answer names — the
// list a prepared plan's admission already resolved — until the returned release
// runs. A swap executes its plan after stopping the outgoing agent; re-reading
// the project config there could only fail too late to refuse, so execution
// applies the answer admission proved, as it does the plan's frozen command.
//
// Releases nest: each restores the freeze it replaced, so an inner plan ending
// cannot clear an outer one that is still executing.
func (i *Instance) freezeEnvPassthrough(names []string) (release func()) {
	frozen := append([]string(nil), names...)
	prev := i.frozenEnvPassthrough.Swap(&frozen)
	return func() { i.frozenEnvPassthrough.Store(prev) }
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
	passthrough, err := SessionEnvPassthroughForInstance(i)
	if err != nil {
		return fmt.Errorf("session environment for %q: %w", i.Title, err)
	}
	if err := tmuxSession.SetEnvPassthrough(passthrough); err != nil {
		return fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	refreshSessionIdentity(i, tmuxSession)
	i.envResolvedTmux.Store(tmuxSession)
	return nil
}

// refreshSessionIdentity is refreshSessionEnvironment's config-free half: the
// account and instance id the session carries, which need no project config.
func refreshSessionIdentity(i *Instance, tmuxSession *tmux.TmuxSession) {
	// Refreshed alongside the pass-through, on the same paths, so a restored or
	// re-provisioned session carries the account it was created with rather than
	// quietly reverting to the ambient identity (#3051).
	i.mu.RLock()
	account := i.Account
	accountAgent := i.accountNamespaceLocked()
	i.mu.RUnlock()
	tmuxSession.SetAccountForAgent(accountAgent, account)
	// The agent session carries its instance id so a reconcile that finds it live
	// can prove which create attempt spawned it (#6258).
	tmuxSession.SetInstanceID(i.ID)
}

// ensureSpawnEnvironment is the property for the tab spawns that build a
// sibling from the agent's handle: a spawn reads its own environment
// (#7080, #7083). A handle counts as resolved only once a refresh SUCCEEDED on
// it — Provision, Recover, a respawn or a swap marks it (envResolvedTmux,
// envResolvedWorktree). Any other handle — one that skipped Provision, or one
// whose load could not read the config — is refreshed here: the agent handle's
// pass-through and account, which the sibling inherits, and the worktree's
// list. A refresh that fails refuses the spawn with its error (the actionable
// unknown-config one, for a config not read in time) rather than start the
// tab bare, on the daemon's own identity.
func (i *Instance) ensureSpawnEnvironment(agentTmux *tmux.TmuxSession, gw *git.GitWorktree) error {
	resolved := func() bool {
		return i.envResolvedTmux.Load() == agentTmux && (gw == nil || i.envResolvedWorktree.Load() == gw)
	}
	if resolved() {
		return nil
	}
	// One spawn resolves it; a concurrent one waits, then finds it resolved.
	i.spawnEnvMu.Lock()
	defer i.spawnEnvMu.Unlock()
	if i.envResolvedTmux.Load() != agentTmux {
		if err := refreshSessionEnvironment(i, agentTmux); err != nil {
			return fmt.Errorf("cannot add a tab to %q: %w", i.Title, err)
		}
	}
	if gw != nil && i.envResolvedWorktree.Load() != gw {
		if err := refreshWorktreeEnvironment(i, gw); err != nil {
			return fmt.Errorf("cannot add a tab to %q: %w", i.Title, err)
		}
	}
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
	passthrough, err := SessionEnvPassthroughForInstance(i)
	if err != nil {
		return fmt.Errorf("session environment for %q tab %q: %w", i.Title, tab.Name, err)
	}
	if err := tab.tmux.SetEnvPassthrough(passthrough); err != nil {
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
	passthrough, err := SessionEnvPassthroughForInstance(i)
	if err != nil {
		return fmt.Errorf("post-worktree environment for %q: %w", i.Title, err)
	}
	if err := worktree.SetHookEnvironment(passthrough); err != nil {
		return fmt.Errorf("invalid post-worktree environment pass-through: %w", err)
	}
	i.envResolvedWorktree.Store(worktree)
	return nil
}
