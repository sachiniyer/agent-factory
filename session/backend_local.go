package session

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

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
		// Create new tmux session with repo-scoped name
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
			if cleanupErr := i.Kill(); cleanupErr != nil {
				setupErr = fmt.Errorf("%v (cleanup error: %v)", setupErr, cleanupErr)
			}
		} else {
			i.mu.Lock()
			i.started = true
			i.mu.Unlock()
		}
	}()

	if !firstTimeSetup {
		// Reuse existing session
		if err := tmuxSession.Restore(); err != nil {
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
			injectSystemPrompt(i.Program, i.Title, gw.GetWorktreePath()),
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

func (b *LocalBackend) Kill(i *Instance) error {
	i.mu.Lock()
	ts := i.tmuxSession
	gw := i.gitWorktree
	i.started = false
	i.tmuxSession = nil
	i.gitWorktree = nil
	i.mu.Unlock()

	var errs []error

	if ts != nil {
		if err := ts.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close tmux session: %w", err))
		}
	}

	if gw != nil {
		if err := gw.Cleanup(); err != nil {
			errs = append(errs, fmt.Errorf("failed to cleanup git worktree: %w", err))
		}
	}

	return errors.Join(errs...)
}

func (b *LocalBackend) Preview(i *Instance) (string, error) {
	if !i.started {
		return "", nil
	}
	return i.tmuxSession.CapturePaneContent()
}

func (b *LocalBackend) PreviewFullHistory(i *Instance) (string, error) {
	if !i.started {
		return "", nil
	}
	return i.tmuxSession.CapturePaneContentWithOptions("-", "-")
}

func (b *LocalBackend) Attach(i *Instance) (chan struct{}, error) {
	if !i.started {
		return nil, fmt.Errorf("cannot attach instance that has not been started")
	}
	return i.tmuxSession.Attach()
}

func (b *LocalBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
	i.mu.RUnlock()

	if !s {
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

func (b *LocalBackend) SetPreviewSize(i *Instance, width, height int) error {
	if !i.started {
		return fmt.Errorf("cannot set preview size for instance that has not been started")
	}
	return i.tmuxSession.SetDetachedSize(width, height)
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
	program := i.Program
	if !strings.Contains(program, tmux.ProgramClaude) &&
		!strings.Contains(program, tmux.ProgramAider) &&
		!strings.Contains(program, tmux.ProgramGemini) {
		return false
	}
	return ts.CheckAndHandleTrustPrompt()
}

func (b *LocalBackend) TapEnter(i *Instance) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
	autoYes := i.AutoYes
	i.mu.RUnlock()

	if !s || !autoYes {
		return
	}
	if err := ts.TapEnter(); err != nil {
		log.ErrorLog.Printf("error tapping enter: %v", err)
	}
}
