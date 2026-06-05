package session

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// resolveProgramForInstance returns the actual tmux command for an instance.
// Resolution chain: agent enum -> cfg.ProgramOverrides[agent] (if set) ->
// bare agent name. When AutoYes is set on a claude instance, the
// --permission-mode bypassPermissions flag is appended to the resolved
// command — claude needs the flag at exec time, and Instance.Program now
// holds only the bare enum so the append can no longer happen in main.go.
// A nil cfg (e.g. tests that don't materialize a config) falls back to the
// raw Program string so legacy free-form values still reach tmux verbatim.
func resolveProgramForInstance(i *Instance) string {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.WarningLog.Printf("failed to load config when resolving program for %q: %v", i.Title, err)
		cfg = nil
	}
	resolved := config.ResolveProgram(cfg, i.Program)
	if i.AutoYes && DetectAgentFromProgram(i.Program) == tmux.ProgramClaude {
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
	existingSession := i.tmuxSession
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
	i.tmuxSession = tmuxSession
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
				// Restore: only clean up tmux session, preserve the worktree
				// to avoid data loss.
				i.mu.Lock()
				ts := i.tmuxSession
				i.tmuxSession = nil
				i.started = false
				i.mu.Unlock()
				if ts != nil {
					if cleanupErr := ts.Close(); cleanupErr != nil {
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
		i.tmuxSession.SetProgram(
			injectSystemPrompt(i.Program, resolveProgramForInstance(i), i.Title, gw.GetWorktreePath()),
		)

		// Create new session
		if err := i.tmuxSession.Start(gw.GetWorktreePath()); err != nil {
			// Cleanup git worktree if tmux session creation fails
			if cleanupErr := gw.Cleanup(); cleanupErr != nil {
				err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
			}
			setupErr = fmt.Errorf("failed to start new session: %w", err)
			return setupErr
		}
	}

	return nil
}

// Kill is best-effort: each cleanup step runs independently and a failure in
// one (e.g. a broken git worktree) only logs a warning rather than aborting
// the rest. The in-memory pointers are cleared regardless so the daemon
// caller can always proceed to remove the persisted record. See issue #478.
func (b *LocalBackend) Kill(i *Instance) error {
	i.mu.Lock()
	ts := i.tmuxSession
	gw := i.gitWorktree
	title := i.Title
	i.started = false
	i.mu.Unlock()

	if ts != nil {
		if err := ts.Close(); err != nil {
			log.WarningLog.Printf("kill %q: tmux cleanup failed: %v", title, err)
		}
	}

	if gw != nil {
		if err := gw.Cleanup(); err != nil {
			log.WarningLog.Printf("kill %q: git worktree cleanup failed: %v", title, err)
		}
	}

	i.mu.Lock()
	if i.tmuxSession == ts {
		i.tmuxSession = nil
	}
	if i.gitWorktree == gw {
		i.gitWorktree = nil
	}
	i.mu.Unlock()

	return nil
}

func (b *LocalBackend) Preview(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContent()
}

func (b *LocalBackend) PreviewFullHistory(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentWithOptions("-", "-")
}

func (b *LocalBackend) Attach(i *Instance) (chan struct{}, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
	i.mu.RUnlock()

	if !s || ts == nil {
		return nil, fmt.Errorf("cannot attach instance that has not been started")
	}
	return ts.Attach()
}

func (b *LocalBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
	i.mu.RUnlock()

	if !s || ts == nil {
		return false, false
	}
	return ts.HasUpdated()
}

func (b *LocalBackend) SendPrompt(i *Instance, prompt string) error {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
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
	ts := i.tmuxSession
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
	ts := i.tmuxSession
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
	ts := i.tmuxSession
	i.mu.RUnlock()

	if !s || ts == nil {
		return fmt.Errorf("cannot set preview size for instance that has not been started")
	}
	return ts.SetDetachedSize(width, height)
}

func (b *LocalBackend) IsAlive(i *Instance) bool {
	i.mu.RLock()
	ts := i.tmuxSession
	i.mu.RUnlock()

	if ts == nil {
		return false
	}
	return ts.DoesSessionExist()
}

func (b *LocalBackend) CheckAndHandleTrustPrompt(i *Instance) bool {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
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
	ts := i.tmuxSession
	autoYes := i.AutoYes
	i.mu.RUnlock()

	if !s || !autoYes || ts == nil {
		return
	}
	if err := ts.TapEnter(); err != nil {
		log.ErrorLog.Printf("error tapping enter: %v", err)
	}
}
