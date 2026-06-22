package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// resolveProgramForInstance returns the actual tmux command for an instance.
// Resolution chain: agent enum -> cfg.ProgramOverrides[agent] (if set) ->
// bare agent name. The overrides come from the repo-resolved config (global
// program_overrides merged with the repo's .agent-factory/config.json) when
// the instance path belongs to a git repo; outside a repo, or when repo
// resolution fails, the global config alone applies. When AutoYes is set on a
// claude instance, the --permission-mode bypassPermissions flag is appended
// to the resolved command — claude needs the flag at exec time, and
// Instance.Program now holds only the bare enum so the append can no longer
// happen in main.go. A nil cfg (e.g. tests that don't materialize a config)
// falls back to the raw Program string so legacy free-form values still
// reach tmux verbatim.
func resolveProgramForInstance(i *Instance) string {
	var cfg *config.Config
	if repo, err := config.RepoFromPath(i.Path); err == nil {
		if resolved, rerr := config.ResolveConfig(repo.Root); rerr == nil {
			cfg = &resolved.Config
		} else {
			log.WarningLog.Printf("failed to resolve repo config when resolving program for %q: %v", i.Title, rerr)
		}
	}
	if cfg == nil {
		loaded, err := config.LoadConfig()
		if err != nil {
			log.WarningLog.Printf("failed to load config when resolving program for %q: %v", i.Title, err)
			loaded = nil
		}
		cfg = loaded
	}
	resolved := config.ResolveProgram(cfg, i.Program)
	if i.AutoYes && DetectAgentFromProgram(i.Program) == tmux.ProgramClaude &&
		// Sessions persisted by pre-#659 binaries got the flag appended at
		// create-time in main.go (19c0dd9), so legacy Instance.Program values
		// can already carry it; appending again duplicates the flag on every
		// restore (#818). A substring check suffices: claude exposes no short
		// form of --permission-mode, and the check also matches the
		// =-attached spelling.
		!strings.Contains(resolved, "--permission-mode") {
		resolved = resolved + " --permission-mode bypassPermissions"
	}
	return resolved
}

// LocalBackend implements Backend using local tmux sessions and git worktrees.
type LocalBackend struct{}

func (b *LocalBackend) Type() string { return "local" }

func (b *LocalBackend) Start(i *Instance, firstTimeSetup bool) error {
	if i.Title == "" {
		return fmt.Errorf("instance title cannot be empty")
	}

	var tmuxSession *tmux.TmuxSession
	i.mu.RLock()
	existingSession := i.tmuxLocked()
	i.mu.RUnlock()

	if existingSession != nil {
		// Use existing tmux session (useful for testing)
		tmuxSession = existingSession
	} else {
		// Create new tmux session with repo-scoped name. The program
		// passed here is a placeholder — SetProgram below replaces it
		// with the override-resolved + system-prompt-injected form
		// before Start/Restore.
		tmuxSession = tmux.NewTmuxSessionForRepo(i.Title, i.Path, i.Program)
	}

	i.mu.Lock()
	i.setTmuxLocked(tmuxSession)
	i.mu.Unlock()

	if firstTimeSetup {
		gitWorktree, branchName, err := git.NewGitWorktree(i.Path, i.Title)
		if err != nil {
			return fmt.Errorf("failed to create git worktree: %w", err)
		}
		i.mu.Lock()
		i.gitWorktree = gitWorktree
		i.Branch = branchName
		i.mu.Unlock()
	}

	// Setup error handler to cleanup resources on any error.
	// Kill() acquires its own lock, so we must not hold i.mu here.
	var setupErr error
	defer func() {
		if setupErr != nil {
			if firstTimeSetup {
				// New session: full cleanup (tmux + worktree) is safe.
				if cleanupErr := i.Kill(); cleanupErr != nil {
					setupErr = fmt.Errorf("%v (cleanup error: %v)", setupErr, cleanupErr)
				}
			} else {
				// Restore: the server-side tmux session may already be live
				// (has-session passed) and we only failed to allocate the
				// local attach PTY — e.g. EMFILE/ENOMEM in Restore (#895).
				// Use CloseAttachOnly, NOT Close: Close runs `tmux
				// kill-session` and would destroy a recoverable live session
				// (scrollback + running processes), turning a transient attach
				// failure into data loss. CloseAttachOnly releases only the
				// local attach resources this object opened and leaves the
				// server session and its worktree intact for a later retry.
				i.mu.Lock()
				ts := i.tmuxLocked()
				i.setTmuxLocked(nil)
				i.started = false
				i.mu.Unlock()
				if ts != nil {
					if cleanupErr := ts.CloseAttachOnly(); cleanupErr != nil {
						setupErr = fmt.Errorf("%v (cleanup error: %v)", setupErr, cleanupErr)
					}
				}
			}
		} else {
			i.mu.Lock()
			i.started = true
			i.mu.Unlock()
		}
	}()

	if !firstTimeSetup {
		// Reuse existing session. Pass the worktree path so Restore can
		// re-spawn the tmux session if the server died across a reboot
		// (see #386). When the worktree is unavailable (e.g. tests inject
		// a tmux session without a gitWorktree), pass empty string.
		i.mu.RLock()
		gw := i.gitWorktree
		i.mu.RUnlock()
		var workDir string
		if gw != nil {
			workDir = gw.GetWorktreePath()
		}
		// Re-inject the system prompt so a lazy re-spawn (tmux server died
		// across a reboot, see #386/#444) starts the agent with the same
		// program string as the original first-time launch — most
		// importantly, claude-code's --plugin-dir flag, without which
		// /af-* slash commands silently vanish post-reboot (#511).
		// Setting the program on the existing attach path is harmless:
		// attach-session does not re-exec the program.
		if workDir != "" {
			tmuxSession.SetProgram(injectSystemPrompt(i.Program, resolveProgramForInstance(i), i.Title, workDir))
		}
		if err := tmuxSession.Restore(workDir); err != nil {
			setupErr = fmt.Errorf("failed to restore existing session: %w", err)
			return setupErr
		}
	} else {
		i.mu.RLock()
		gw := i.gitWorktree
		i.mu.RUnlock()

		// Setup git worktree first
		if err := gw.Setup(); err != nil {
			setupErr = fmt.Errorf("failed to setup git worktree: %w", err)
			return setupErr
		}

		// Inject Agent Factory instructions into the session.
		tmuxSession.SetProgram(
			injectSystemPrompt(i.Program, resolveProgramForInstance(i), i.Title, gw.GetWorktreePath()),
		)

		// Create new session
		if err := tmuxSession.Start(gw.GetWorktreePath()); err != nil {
			// Cleanup git worktree if tmux session creation fails
			if cleanupErr := gw.Cleanup(); cleanupErr != nil {
				err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
			}
			setupErr = fmt.Errorf("failed to start new session: %w", err)
			return setupErr
		}
	}

	// Promote the per-instance terminal into a real Shell tab (#930 PR 2). This
	// is best-effort: a shell-tab failure leaves the instance fully usable with
	// just the agent tab (the terminal tab renders a fallback), so it must not
	// fail the whole start. Runs after the agent session is up so the shell tab
	// can be a sibling of it (sharing tmux deps).
	b.setupShellTab(i)

	return nil
}

// setupShellTab ensures the instance has a live Shell tab. On a fresh start (or
// a legacy restore that predates persisted tabs) it creates a $SHELL session as
// a sibling of the agent session; on restore of a persisted shell tab it
// reconnects to the exact tmux session by name so the terminal survives an
// af/daemon restart.
func (b *LocalBackend) setupShellTab(i *Instance) {
	i.mu.RLock()
	agentTmux := i.tmuxLocked()
	shell := i.shellTabLocked()
	gw := i.gitWorktree
	i.mu.RUnlock()

	if agentTmux == nil || gw == nil {
		return
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return
	}

	// Restore a persisted shell session: reconnect, re-spawning in the worktree
	// only if the tmux server died across a reboot (Restore handles both).
	if shell != nil && shell.tmux != nil {
		if err := shell.tmux.Restore(worktreePath); err != nil {
			log.WarningLog.Printf("restore shell tab for %q failed: %v", i.Title, err)
		}
		return
	}

	// Create a fresh shell session as a sibling of the agent session so it
	// inherits the agent's PTY factory / executor — real in production, mock in
	// tests — keeping the create path hermetic. The name extends the agent
	// session's name deterministically so it is collision-free and restorable
	// by exact name.
	shellTmux := agentTmux.NewSiblingSession(agentTmux.SanitizedName()+shellTmuxSuffix, defaultShell())
	if err := shellTmux.Start(worktreePath); err != nil {
		log.WarningLog.Printf("start shell tab for %q failed: %v", i.Title, err)
		return
	}

	i.mu.Lock()
	if existing := i.shellTabLocked(); existing != nil {
		existing.tmux = shellTmux
	} else {
		i.Tabs = append(i.Tabs, newShellTab(shellTmux))
	}
	i.mu.Unlock()
}

// Kill is best-effort: each cleanup step runs independently and a failure in
// one (e.g. a broken git worktree) only logs a warning rather than aborting
// the rest. The in-memory pointers are cleared regardless so the daemon
// caller can always proceed to remove the persisted record. See issue #478.
func (b *LocalBackend) Kill(i *Instance) error {
	i.mu.Lock()
	// Snapshot every tab's tmux session under the lock. PR 2 of #930 gives an
	// instance N tabs (agent + shell today), so Kill tears down each tab's
	// session, not just the agent's.
	type tabSession struct {
		name string
		ts   *tmux.TmuxSession
	}
	sessions := make([]tabSession, 0, len(i.Tabs))
	for _, tab := range i.Tabs {
		if tab.tmux != nil {
			sessions = append(sessions, tabSession{name: tab.Name, ts: tab.tmux})
		}
	}
	gw := i.gitWorktree
	title := i.Title
	i.started = false
	i.mu.Unlock()

	// Tear down every tab's tmux session BEFORE the worktree cleanup below, and
	// wait for each pane's process to actually exit: kill-session only SIGHUPs
	// it, and a process still flushing state mid-shutdown races git's recursive
	// delete of the worktree, leaking a half-deleted directory ("Directory not
	// empty", #802). The ordering — all panes exit, then remove the worktree
	// once — is preserved across all tabs.
	for _, s := range sessions {
		if err := s.ts.CloseAndWaitForPaneExit(); err != nil {
			log.WarningLog.Printf("kill %q: tmux cleanup for tab %q failed: %v", title, s.name, err)
		}
	}

	if gw != nil {
		if err := gw.Cleanup(); err != nil {
			log.WarningLog.Printf("kill %q: git worktree cleanup failed: %v", title, err)
		}
	}

	i.mu.Lock()
	for _, tab := range i.Tabs {
		tab.tmux = nil
	}
	if i.gitWorktree == gw {
		i.gitWorktree = nil
	}
	i.mu.Unlock()

	return nil
}

// CloseAttachOnly releases this instance's hold on the tmux session — the
// attach PTY and the `tmux attach-session` child process — WITHOUT running
// `tmux kill-session`. The server-side tmux session and the git worktree
// behind it are left untouched. The daemon uses this to discard a duplicate
// Instance built from disk that turned out to already be tracked in memory
// (#867): the duplicate must surrender the PTY it opened during restore
// without tearing down the live session the canonical Instance shares.
func (b *LocalBackend) CloseAttachOnly(i *Instance) error {
	i.mu.Lock()
	ts := i.tmuxLocked()
	i.started = false
	i.mu.Unlock()

	if ts == nil {
		return nil
	}

	err := ts.CloseAttachOnly()

	i.mu.Lock()
	if i.tmuxLocked() == ts {
		i.setTmuxLocked(nil)
	}
	i.mu.Unlock()
	return err
}

func (b *LocalBackend) Preview(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContent()
}

func (b *LocalBackend) PreviewFullHistory(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentWithOptions("-", "-")
}

func (b *LocalBackend) Attach(i *Instance) (chan struct{}, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return nil, fmt.Errorf("cannot attach instance that has not been started")
	}
	return ts.Attach()
}

func (b *LocalBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return false, false
	}
	return ts.HasUpdated()
}

func (b *LocalBackend) SendPrompt(i *Instance, prompt string) error {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s {
		return fmt.Errorf("instance not started")
	}
	if ts == nil {
		return fmt.Errorf("tmux session not initialized")
	}
	if err := ts.SendKeys(prompt); err != nil {
		return fmt.Errorf("error sending keys to tmux session: %w", err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := ts.TapEnter(); err != nil {
		return fmt.Errorf("error tapping enter: %w", err)
	}

	return nil
}

func (b *LocalBackend) SendPromptCommand(i *Instance, prompt string) error {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s {
		return fmt.Errorf("instance not started")
	}
	if ts == nil {
		return fmt.Errorf("tmux session not initialized")
	}
	return ts.SendKeysCommand(prompt)
}

func (b *LocalBackend) SendKeys(i *Instance, keys string) error {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s {
		return fmt.Errorf("cannot send keys to instance that has not been started")
	}
	if ts == nil {
		return fmt.Errorf("tmux session not initialized")
	}
	return ts.SendKeys(keys)
}

func (b *LocalBackend) SetPreviewSize(i *Instance, width, height int) error {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return fmt.Errorf("cannot set preview size for instance that has not been started")
	}
	return ts.SetDetachedSize(width, height)
}

func (b *LocalBackend) IsAlive(i *Instance) bool {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if ts == nil {
		return false
	}
	return ts.DoesSessionExist()
}

func (b *LocalBackend) CheckAndHandleTrustPrompt(i *Instance) bool {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return false
	}
	// Normalize so restored sessions with legacy free-form Program values
	// (e.g. "/home/foo/bin/claude") still get trust-prompt auto-handling —
	// same persisted-state class of regression as #677. Codex was added in
	// #729: it was previously excluded here, so a codex trust/confirmation
	// dialog was never dismissed even though isReadyContent could surface it.
	switch DetectAgentFromProgram(i.Program) {
	case tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramGemini:
		return ts.CheckAndHandleTrustPrompt()
	}
	return false
}

func (b *LocalBackend) TapEnter(i *Instance) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	autoYes := i.AutoYes
	i.mu.RUnlock()

	if !s || !autoYes || ts == nil {
		return
	}
	if err := ts.TapEnter(); err != nil {
		log.ErrorLog.Printf("error tapping enter: %v", err)
	}
}
