package session

import (
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"path/filepath"
	"sync"

	"fmt"
	"strings"
	"time"
)

type Status int

const (
	// Running is the status when the instance is running and claude is working.
	Running Status = iota
	// Ready is if the claude instance is ready to be interacted with (waiting for user input).
	Ready
	// Loading is if the instance is loading (if we are setting it up).
	Loading
)

// Instance is a running instance of claude code.
type Instance struct {
	// mu protects fields that are accessed concurrently by async Start()
	// goroutines (writers) and the main bubbletea loop (readers):
	// started, Status, tmuxSession, gitWorktree, prInfo, diffStats.
	mu sync.RWMutex

	// Title is the title of the instance.
	Title string
	// Path is the path to the workspace.
	Path string
	// Branch is the branch of the instance.
	Branch string
	// Status is the status of the instance.
	Status Status
	// Program is the program to run in the instance.
	Program string
	// Height is the height of the instance.
	Height int
	// Width is the width of the instance.
	Width int
	// CreatedAt is the time the instance was created.
	CreatedAt time.Time
	// UpdatedAt is the time the instance was last updated.
	UpdatedAt time.Time
	// AutoYes is true if the instance should automatically press enter when prompted.
	AutoYes bool
	// Prompt is the initial prompt to pass to the instance on startup
	Prompt string

	// prInfo stores the associated GitHub PR info
	prInfo *git.PRInfo

	// The below fields are initialized upon calling Start().

	started bool
	// tmuxSession is the tmux session for the instance.
	tmuxSession *tmux.TmuxSession
	// gitWorktree is the git worktree for the instance.
	gitWorktree *git.GitWorktree
}

// ToInstanceData converts an Instance to its serializable form
func (i *Instance) ToInstanceData() InstanceData {
	i.mu.RLock()
	defer i.mu.RUnlock()

	data := InstanceData{
		Title:     i.Title,
		Path:      i.Path,
		Branch:    i.Branch,
		Status:    i.Status,
		Height:    i.Height,
		Width:     i.Width,
		CreatedAt: i.CreatedAt,
		UpdatedAt: time.Now(),
		Program:   i.Program,
		AutoYes:   i.AutoYes,
	}

	// Only include worktree data if gitWorktree is initialized
	if i.gitWorktree != nil {
		data.Worktree = GitWorktreeData{
			RepoPath:         i.gitWorktree.GetRepoPath(),
			WorktreePath:     i.gitWorktree.GetWorktreePath(),
			SessionName:      i.Title,
			BranchName:       i.gitWorktree.GetBranchName(),
			BaseCommitSHA:    i.gitWorktree.GetBaseCommitSHA(),
			ExternalWorktree: i.gitWorktree.IsExternalWorktree(),
		}
	}

	// Only include PR info if it exists
	if i.prInfo != nil {
		data.PRInfo = PRInfoData{
			Number: i.prInfo.Number,
			Title:  i.prInfo.Title,
			URL:    i.prInfo.URL,
			State:  i.prInfo.State,
		}
	}

	return data
}

// FromInstanceData creates a new Instance from serialized data
func FromInstanceData(data InstanceData) (*Instance, error) {
	instance := &Instance{
		Title:     data.Title,
		Path:      data.Path,
		Branch:    data.Branch,
		Status:    data.Status,
		Height:    data.Height,
		Width:     data.Width,
		CreatedAt: data.CreatedAt,
		UpdatedAt: data.UpdatedAt,
		Program:   data.Program,
		gitWorktree: git.NewGitWorktreeFromStorage(
			data.Worktree.RepoPath,
			data.Worktree.WorktreePath,
			data.Worktree.SessionName,
			data.Worktree.BranchName,
			data.Worktree.BaseCommitSHA,
			data.Worktree.ExternalWorktree,
		),
	}

	if data.PRInfo.Number != 0 {
		instance.prInfo = &git.PRInfo{
			Number: data.PRInfo.Number,
			Title:  data.PRInfo.Title,
			URL:    data.PRInfo.URL,
			State:  data.PRInfo.State,
		}
	}

	if err := instance.Start(false); err != nil {
		return nil, err
	}

	return instance, nil
}

// Options for creating a new instance
type InstanceOptions struct {
	// Title is the title of the instance.
	Title string
	// Path is the path to the workspace.
	Path string
	// Program is the program to run in the instance (e.g. "claude", "aider --model ollama_chat/gemma3:1b")
	Program string
	// If AutoYes is true, then
	AutoYes bool
}

func NewInstance(opts InstanceOptions) (*Instance, error) {
	t := time.Now()

	// Convert path to absolute
	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	return &Instance{
		Title:     opts.Title,
		Status:    Ready,
		Path:      absPath,
		Program:   opts.Program,
		Height:    0,
		Width:     0,
		CreatedAt: t,
		UpdatedAt: t,
		AutoYes:   false,
	}, nil
}

func (i *Instance) RepoName() (string, error) {
	if !i.started {
		return "", fmt.Errorf("cannot get repo name for instance that has not been started")
	}
	return i.gitWorktree.GetRepoName(), nil
}

func (i *Instance) SetStatus(status Status) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Status = status
}

// firstTimeSetup is true if this is a new instance. Otherwise, it's one loaded from storage.
func (i *Instance) Start(firstTimeSetup bool) error {
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
		// Create new tmux session
		tmuxSession = tmux.NewTmuxSession(i.Title, i.Program)
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

		// Inject Agent Factory instructions into the session. For CLI-based tools
		// (Claude Code) this modifies the program command. For file-based tools
		// (Codex, Amp, OpenCode) this writes an instruction file into the worktree.
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

	// SetStatus acquires its own lock.
	i.SetStatus(Running)

	return nil
}

// StartWithExistingWorktree starts the instance using an existing worktree
// instead of creating a new one. The worktree and branch are not deleted on kill.
func (i *Instance) StartWithExistingWorktree(worktreePath, branchName string) error {
	if i.Title == "" {
		return fmt.Errorf("instance title cannot be empty")
	}

	gitWorktree, err := git.NewGitWorktreeFromExistingWorktree(i.Path, worktreePath, branchName)
	if err != nil {
		return fmt.Errorf("failed to create git worktree reference: %w", err)
	}

	i.mu.Lock()
	i.gitWorktree = gitWorktree
	i.Branch = branchName
	i.mu.Unlock()

	program := injectSystemPrompt(i.Program, i.Title, worktreePath)
	tmuxSession := tmux.NewTmuxSession(i.Title, program)

	i.mu.Lock()
	i.tmuxSession = tmuxSession
	i.mu.Unlock()

	// Start is I/O; do not hold the lock.
	if err := tmuxSession.Start(worktreePath); err != nil {
		return fmt.Errorf("failed to start tmux session: %w", err)
	}

	i.mu.Lock()
	i.started = true
	i.Status = Running
	i.mu.Unlock()

	return nil
}

// Kill terminates the instance and cleans up all resources
func (i *Instance) Kill() error {
	i.mu.RLock()
	wasStarted := i.started
	ts := i.tmuxSession
	gw := i.gitWorktree
	i.mu.RUnlock()

	if !wasStarted {
		// If instance was never started, just return success
		return nil
	}

	var errs []error

	// Always try to cleanup both resources, even if one fails
	// Clean up tmux session first since it's using the git worktree
	if ts != nil {
		if err := ts.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close tmux session: %w", err))
		}
	}

	// Then clean up git worktree
	if gw != nil {
		if err := gw.Cleanup(); err != nil {
			errs = append(errs, fmt.Errorf("failed to cleanup git worktree: %w", err))
		}
	}

	return i.combineErrors(errs)
}

// combineErrors combines multiple errors into a single error
func (i *Instance) combineErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}

	errMsg := "multiple cleanup errors occurred:"
	for _, err := range errs {
		errMsg += "\n  - " + err.Error()
	}
	return fmt.Errorf("%s", errMsg)
}

func (i *Instance) Preview() (string, error) {
	if !i.started {
		return "", nil
	}
	return i.tmuxSession.CapturePaneContent()
}

func (i *Instance) HasUpdated() (updated bool, hasPrompt bool) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxSession
	i.mu.RUnlock()

	if !s {
		return false, false
	}
	return ts.HasUpdated()
}

// TapEnter sends an enter key press to the tmux session if AutoYes is enabled.
// CheckAndHandleTrustPrompt checks for and dismisses the trust prompt for supported programs.
func (i *Instance) CheckAndHandleTrustPrompt() bool {
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

func (i *Instance) TapEnter() {
	if !i.started || !i.AutoYes {
		return
	}
	if err := i.tmuxSession.TapEnter(); err != nil {
		log.ErrorLog.Printf("error tapping enter: %v", err)
	}
}

func (i *Instance) Attach() (chan struct{}, error) {
	if !i.started {
		return nil, fmt.Errorf("cannot attach instance that has not been started")
	}
	return i.tmuxSession.Attach()
}

func (i *Instance) SetPreviewSize(width, height int) error {
	if !i.started {
		return fmt.Errorf("cannot set preview size for instance that has not been started")
	}
	return i.tmuxSession.SetDetachedSize(width, height)
}

// GetGitWorktree returns the git worktree for the instance
func (i *Instance) GetGitWorktree() (*git.GitWorktree, error) {
	if !i.started {
		return nil, fmt.Errorf("cannot get git worktree for instance that has not been started")
	}
	return i.gitWorktree, nil
}

// GetWorktreePath returns the worktree path for the instance, or empty string if unavailable
func (i *Instance) GetWorktreePath() string {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()

	if gw == nil {
		return ""
	}
	return gw.GetWorktreePath()
}

func (i *Instance) Started() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.started
}

// SetTitle sets the title of the instance. Returns an error if the instance has started.
// We cant change the title once it's been used for a tmux session etc.
func (i *Instance) SetTitle(title string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.started {
		return fmt.Errorf("cannot change title of a started instance")
	}
	i.Title = title
	return nil
}

// TmuxAlive returns true if the tmux session is alive. This is a sanity check before attaching.
func (i *Instance) TmuxAlive() bool {
	i.mu.RLock()
	ts := i.tmuxSession
	i.mu.RUnlock()

	if ts == nil {
		return false
	}
	return ts.DoesSessionExist()
}

// GetPRInfo returns the associated GitHub PR info, or nil if none.
func (i *Instance) GetPRInfo() *git.PRInfo {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.prInfo
}

// SetPRInfo sets the associated GitHub PR info.
func (i *Instance) SetPRInfo(info *git.PRInfo) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfo = info
}

// UpdatePRInfo fetches the latest PR info from GitHub for this instance's branch.
func (i *Instance) UpdatePRInfo() error {
	i.mu.RLock()
	s := i.started
	gw := i.gitWorktree
	i.mu.RUnlock()

	if !s || gw == nil {
		return nil
	}
	info, err := git.FetchPRInfo(gw.GetRepoPath(), i.Branch)
	if err != nil {
		return err
	}

	i.mu.Lock()
	i.prInfo = info
	i.mu.Unlock()
	return nil
}

// SendPrompt sends a prompt to the tmux session
func (i *Instance) SendPrompt(prompt string) error {
	if !i.started {
		return fmt.Errorf("instance not started")
	}
	if i.tmuxSession == nil {
		return fmt.Errorf("tmux session not initialized")
	}
	if err := i.tmuxSession.SendKeys(prompt); err != nil {
		return fmt.Errorf("error sending keys to tmux session: %w", err)
	}

	// Brief pause to prevent carriage return from being interpreted as newline
	time.Sleep(100 * time.Millisecond)
	if err := i.tmuxSession.TapEnter(); err != nil {
		return fmt.Errorf("error tapping enter: %w", err)
	}

	return nil
}

// SendPromptCommand sends a prompt using tmux send-keys command instead of PTY writes.
// This is more reliable for headless/scheduled runs where the PTY may not persist.
func (i *Instance) SendPromptCommand(prompt string) error {
	if !i.started {
		return fmt.Errorf("instance not started")
	}
	if i.tmuxSession == nil {
		return fmt.Errorf("tmux session not initialized")
	}
	return i.tmuxSession.SendKeysCommand(prompt)
}

// PreviewFullHistory captures the entire tmux pane output including full scrollback history
func (i *Instance) PreviewFullHistory() (string, error) {
	if !i.started {
		return "", nil
	}
	return i.tmuxSession.CapturePaneContentWithOptions("-", "-")
}

// SetTmuxSession sets the tmux session for testing purposes
func (i *Instance) SetTmuxSession(session *tmux.TmuxSession) {
	i.tmuxSession = session
}

// SendKeys sends keys to the tmux session
func (i *Instance) SendKeys(keys string) error {
	if !i.started {
		return fmt.Errorf("cannot send keys to instance that has not been started")
	}
	return i.tmuxSession.SendKeys(keys)
}
