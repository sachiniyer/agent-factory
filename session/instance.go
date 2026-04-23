package session

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
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
	// prInfoLastFetched is the wall-clock time of the most recent PR info
	// fetch. Not persisted — restored instances start with a zero value so
	// the first lazy fetch on selection always runs. Used to debounce
	// repeated fetches when the user cycles the sidebar.
	prInfoLastFetched time.Time

	// backend abstracts session lifecycle (local tmux+git vs remote hooks).
	backend Backend
	// remoteMeta stores additional metadata returned by remote hook scripts.
	remoteMeta map[string]interface{}

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

	if i.backend != nil {
		data.BackendType = i.backend.Type()
	}
	if i.remoteMeta != nil {
		data.RemoteMeta = i.remoteMeta
	}

	// Persist the tmux session name so we can restore it exactly
	if i.tmuxSession != nil {
		data.TmuxName = i.tmuxSession.SanitizedName()
	}

	// Only include worktree data if gitWorktree is initialized
	if i.gitWorktree != nil {
		branchCreatedByUs := i.gitWorktree.BranchCreatedByUs()
		data.Worktree = GitWorktreeData{
			RepoPath:          i.gitWorktree.GetRepoPath(),
			WorktreePath:      i.gitWorktree.GetWorktreePath(),
			SessionName:       i.Title,
			BranchName:        i.gitWorktree.GetBranchName(),
			BaseCommitSHA:     i.gitWorktree.GetBaseCommitSHA(),
			ExternalWorktree:  i.gitWorktree.IsExternalWorktree(),
			BranchCreatedByUs: &branchCreatedByUs,
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
		Title:      data.Title,
		Path:       data.Path,
		Branch:     data.Branch,
		Status:     data.Status,
		Height:     data.Height,
		Width:      data.Width,
		CreatedAt:  data.CreatedAt,
		UpdatedAt:  data.UpdatedAt,
		Program:    data.Program,
		AutoYes:    data.AutoYes,
		remoteMeta: data.RemoteMeta,
	}

	// Pick backend based on persisted BackendType.
	if data.BackendType == "remote" {
		hook, err := loadHookBackendForPath(data.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to load remote hooks config: %w", err)
		}
		instance.backend = hook
	} else {
		instance.backend = &LocalBackend{}

		// Preserve backward compatibility: when the branch_created_by_us
		// field is missing from persisted data (written before this field
		// was added), default to true. Old saved sessions were created
		// under the assumption that the session owned the branch, so
		// keeping that behavior avoids surprising changes on restore.
		branchCreatedByUs := true
		if data.Worktree.BranchCreatedByUs != nil {
			branchCreatedByUs = *data.Worktree.BranchCreatedByUs
		}

		gw, err := git.NewGitWorktreeFromStorage(
			data.Worktree.RepoPath,
			data.Worktree.WorktreePath,
			data.Worktree.SessionName,
			data.Worktree.BranchName,
			data.Worktree.BaseCommitSHA,
			data.Worktree.ExternalWorktree,
			branchCreatedByUs,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to restore git worktree: %w", err)
		}
		instance.gitWorktree = gw

		// Pre-set the tmux session with the correct name for backward compat.
		// If TmuxName was persisted, use it exactly; otherwise fall back to
		// the legacy naming scheme (no repo hash) so old sessions still restore.
		if data.TmuxName != "" {
			instance.tmuxSession = tmux.NewTmuxSessionFromSanitizedName(data.TmuxName, data.Program)
		} else {
			instance.tmuxSession = tmux.NewTmuxSession(data.Title, data.Program)
		}
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
	// ForceRemote forces the instance to use the remote hook backend,
	// even if the repo config would default to local.
	ForceRemote bool
}

// backendFactory constructs the Backend used by a new Instance. It is a
// package-level variable (not a hard-coded branch) so tests can inject a
// FakeBackend through SetBackendFactoryForTest without touching production
// code paths. Defaults to the real local/remote branching.
var backendFactory = defaultBackendFactory

func defaultBackendFactory(opts InstanceOptions, absPath string) (Backend, error) {
	if opts.ForceRemote {
		hook, err := loadHookBackendForPath(absPath)
		if err != nil {
			return nil, fmt.Errorf("remote hooks not configured for this repo: %w", err)
		}
		return hook, nil
	}
	return &LocalBackend{}, nil
}

// SetBackendFactoryForTest replaces the backend factory with f and returns a
// restore function. Intended for use in tests that need to swap in a
// FakeBackend so NewInstance-driven creation flows stay on the hot path.
func SetBackendFactoryForTest(f func(opts InstanceOptions, absPath string) (Backend, error)) func() {
	prev := backendFactory
	backendFactory = f
	return func() { backendFactory = prev }
}

func NewInstance(opts InstanceOptions) (*Instance, error) {
	t := time.Now()

	// Convert path to absolute
	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	backend, err := backendFactory(opts, absPath)
	if err != nil {
		return nil, err
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
		AutoYes:   opts.AutoYes,
		backend:   backend,
	}, nil
}

func (i *Instance) RepoName() (string, error) {
	if i.IsRemote() {
		return "", fmt.Errorf("remote instances do not have a local repo")
	}
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

// GetBranch returns the current worktree branch name under the Instance's
// mutex. Readers that run from goroutines other than the one mutating the
// instance (notably the bubbletea renderer) must use this accessor rather
// than reading i.Branch directly, or the race detector flags a write in
// LocalBackend.Start vs a read in InstanceRenderer.Render.
func (i *Instance) GetBranch() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Branch
}

// GetStatus returns the current status under the Instance's mutex, so
// cross-goroutine readers don't race with SetStatus.
func (i *Instance) GetStatus() Status {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Status
}

// GetTitle returns the instance title under the Instance's mutex.
func (i *Instance) GetTitle() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Title
}

// firstTimeSetup is true if this is a new instance. Otherwise, it's one loaded from storage.
func (i *Instance) Start(firstTimeSetup bool) error {
	return i.backend.Start(i, firstTimeSetup)
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
	tmuxSession := tmux.NewTmuxSessionForRepo(i.Title, i.Path, program)

	i.mu.Lock()
	i.tmuxSession = tmuxSession
	i.mu.Unlock()

	// Start is I/O; do not hold the lock.
	if err := tmuxSession.Start(worktreePath); err != nil {
		return fmt.Errorf("failed to start tmux session: %w", err)
	}

	i.mu.Lock()
	i.started = true
	i.mu.Unlock()

	return nil
}

// Kill terminates the instance and cleans up all resources
func (i *Instance) Kill() error {
	return i.backend.Kill(i)
}

func (i *Instance) Preview() (string, error) {
	return i.backend.Preview(i)
}

func (i *Instance) HasUpdated() (updated bool, hasPrompt bool) {
	return i.backend.HasUpdated(i)
}

// CheckAndHandleTrustPrompt checks for and dismisses the trust prompt for supported programs.
func (i *Instance) CheckAndHandleTrustPrompt() bool {
	return i.backend.CheckAndHandleTrustPrompt(i)
}

// TapEnter sends an enter key press to the tmux session if AutoYes is enabled.
func (i *Instance) TapEnter() {
	i.backend.TapEnter(i)
}

func (i *Instance) Attach() (chan struct{}, error) {
	return i.backend.Attach(i)
}

func (i *Instance) SetPreviewSize(width, height int) error {
	return i.backend.SetPreviewSize(i, width, height)
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

// TmuxAlive returns true if the underlying session is alive.
// For remote backends this delegates to IsAlive.
func (i *Instance) TmuxAlive() bool {
	return i.backend.IsAlive(i)
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
	i.prInfoLastFetched = time.Now()
}

// PRInfoAge returns how long ago PR info was last fetched. Returns a very
// large duration if PR info has never been fetched in this process.
func (i *Instance) PRInfoAge() time.Duration {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.prInfoLastFetched.IsZero() {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(i.prInfoLastFetched)
}

// MarkPRInfoFetched bumps the fetch timestamp without touching the cached
// value. Used after a transient fetch error so we don't re-try on every
// subsequent selection change.
func (i *Instance) MarkPRInfoFetched() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prInfoLastFetched = time.Now()
}

// FetchPRInfoSnapshot returns the data needed to fetch PR info for this
// instance off the main event loop. The returned repoPath is empty when the
// instance is not ready for fetching (not started, no worktree, or remote).
func (i *Instance) FetchPRInfoSnapshot() (repoPath, branch string) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started || i.gitWorktree == nil {
		return "", ""
	}
	return i.gitWorktree.GetRepoPath(), i.Branch
}

// SendPrompt sends a prompt to the session
func (i *Instance) SendPrompt(prompt string) error {
	return i.backend.SendPrompt(i, prompt)
}

// SendPromptCommand sends a prompt using a more reliable command-based approach.
// This is more reliable for headless/scheduled runs where the PTY may not persist.
func (i *Instance) SendPromptCommand(prompt string) error {
	return i.backend.SendPromptCommand(i, prompt)
}

// PreviewFullHistory captures the entire session output including full scrollback history
func (i *Instance) PreviewFullHistory() (string, error) {
	return i.backend.PreviewFullHistory(i)
}

// SetTmuxSession sets the tmux session for testing purposes
func (i *Instance) SetTmuxSession(session *tmux.TmuxSession) {
	i.tmuxSession = session
}

// SetStartedForTest toggles the started flag for testing purposes. Prefer
// Start() in non-test code; this exists so unit tests can exercise flows
// gated on Started() without spinning up a real tmux session.
func (i *Instance) SetStartedForTest(started bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.started = started
}

// SetGitWorktreeForTest assigns a git worktree to this instance. Test-only:
// the real flow sets this inside LocalBackend.Start, which isn't available
// in unit tests that use FakeBackend.
func (i *Instance) SetGitWorktreeForTest(gw *git.GitWorktree) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.gitWorktree = gw
}

// SendKeys sends keys to the underlying session. For remote backends this
// returns an explicit error since raw key injection is not supported.
func (i *Instance) SendKeys(keys string) error {
	return i.backend.SendKeys(i, keys)
}

// IsRemote returns true if this instance uses the remote hook backend.
func (i *Instance) IsRemote() bool {
	if i.backend == nil {
		return false
	}
	return i.backend.Type() == "remote"
}

// GetBackend returns the backend for the instance (mainly for testing).
func (i *Instance) GetBackend() Backend {
	return i.backend
}

// SetBackend sets the backend for the instance (mainly for testing).
func (i *Instance) SetBackend(b Backend) {
	i.backend = b
}
