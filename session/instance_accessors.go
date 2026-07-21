package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func (i *Instance) RepoName() (string, error) {
	if i.Capabilities().Workspace != WorkspaceLocalWorktree {
		return "", fmt.Errorf("remote instances do not have a local repo")
	}
	i.mu.RLock()
	started := i.started
	gw := i.gitWorktree
	i.mu.RUnlock()
	if !started {
		return "", fmt.Errorf("cannot get repo name for instance that has not been started")
	}
	if gw == nil {
		return "", fmt.Errorf("cannot get repo name for instance without a git worktree")
	}
	return gw.GetRepoName(), nil
}

// SetAutoYes sets the AutoYes flag under the instance mutex. Writers must use
// this rather than assigning i.AutoYes directly: TapEnter runs from the
// metadata-tick background goroutine and reads AutoYes under i.mu.RLock, so
// any unsynchronized write produces a data race (issue #563, regression from
// PR #560 which moved the tick off the bubbletea event loop).
func (i *Instance) SetAutoYes(autoYes bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.AutoYes = autoYes
}

// SetPrompt replaces the durable goal used by later limit resumes and handoffs.
// Prompt became mutable when handoff gained an operator-supplied brief, so the
// write and every concurrent reader must use the instance lock.
func (i *Instance) SetPrompt(prompt string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Prompt = prompt
}

// GetPrompt returns the session's current durable goal.
func (i *Instance) GetPrompt() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Prompt
}

// SetPendingHandoffMission records the rendered takeover brief before the
// irreversible runtime-swap checkpoint. A daemon restart can then recover the
// exact context that still needs delivery instead of guessing from Prompt.
func (i *Instance) SetPendingHandoffMission(mission string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pendingHandoffMission = mission
}

// PendingHandoffMission returns the takeover brief awaiting confirmed delivery.
func (i *Instance) PendingHandoffMission() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingHandoffMission
}

// ClearPendingHandoffMission clears the marker only if it still names mission.
// The compare makes a delayed recovery attempt unable to erase a newer handoff's
// brief after the same session has moved on.
func (i *Instance) ClearPendingHandoffMission(mission string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission != mission {
		return false
	}
	i.pendingHandoffMission = ""
	return true
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

// GetWorktreeBranch returns the canonical branch recorded by the GitWorktree,
// or empty when the instance has no worktree. Unlike GetGitWorktree, this is not
// gated on started: kill/archive cleanup still acts on a restore-failed row's
// recorded worktree and branch, so safety checks must be able to inspect the
// exact ref cleanup would delete (#2209 review).
func (i *Instance) GetWorktreeBranch() string {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return ""
	}
	return gw.GetBranchName()
}

// MarkUserKilled records kill intent on the instance (#1108). Callers persist
// the instance afterwards so the tombstone survives a daemon crash mid-kill.
func (i *Instance) MarkUserKilled() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.userKilled = true
}

// UserKilled reports whether an explicit kill was recorded for this instance.
func (i *Instance) UserKilled() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.userKilled
}

// MarkStartupStateUnknown retains a failed create as an inert record. Clearing
// started prevents attach/probe paths from treating the requested runtime name
// as confirmed; StartupStateUnknown keeps storage checkpoints from dropping the
// record merely because it is not started.
func (i *Instance) MarkStartupStateUnknown() {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, resetAt := i.lifecycleStateLocked()
	i.startupStateUnknown = true
	i.started = false
	// Startup-unknown is a terminal delivery outcome, not a run still consuming
	// the task's concurrency budget. Store that fact on the same transition that
	// stores the terminal marker so projections, persistence, and unloadable-row
	// accounting cannot disagree about whether the slot was released.
	i.taskRunActive = false
	// The create attempt has settled into an explicit blocked outcome. Leaving
	// OpCreating set makes projections report an operation that no goroutine owns
	// and can keep old clients polling forever.
	i.inFlightOp = OpNone
	i.noteStateChangeLocked(lv, op, resetAt)
}

// StartupStateUnknown reports whether a create may have launched a runtime but
// could not confirm its identity or liveness.
func (i *Instance) StartupStateUnknown() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.startupStateUnknown
}

// TaskRunActive reports whether this session's task run is still in flight
// (#1892). Prefer LifecycleView when the answer is combined with any other piece
// of state: a verdict assembled from separate accessor calls can straddle a
// concurrent transition.
func (i *Instance) TaskRunActive() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.taskRunActive
}

// GetGitWorktree returns the git worktree for the instance
func (i *Instance) GetGitWorktree() (*git.GitWorktree, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
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

// GetBaseCommitSHA returns the recorded base commit SHA of the instance's
// worktree, or "" when there is no worktree. Deliberately NOT gated on started
// (unlike GetGitWorktree): the kill-confirmation's unmerged-work check must run
// for a session that has a worktree even if it was never started — a restore-
// failed session's branch still gets force-deleted by the kill (#2029). Mirrors
// GetWorktreePath, which is likewise ungated so both loss checks cover the same
// session states.
func (i *Instance) GetBaseCommitSHA() string {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()

	if gw == nil {
		return ""
	}
	return gw.GetBaseCommitSHA()
}

// GetRepoPath returns the resolved git repo path stored in the instance's
// worktree, or empty string when no worktree is attached (e.g. a remote-
// backend instance). Callers using the result to derive a repo ID must
// fall back to Instance.Path when this is empty (#667).
func (i *Instance) GetRepoPath() string {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()

	if gw == nil {
		return ""
	}
	return gw.GetRepoPath()
}

// PostWorktreeHooksDone returns a channel that is closed once the instance's
// post-worktree hooks (post_worktree_commands) have finished running, or nil
// when no hook run is in flight — no worktree yet, an external worktree that
// skips hooks, or a repo with no hooks configured. The readiness wait uses it
// so a slow build hook running concurrently with the agent is not charged
// against the agent's startup budget (see task.WaitForReady).
func (i *Instance) PostWorktreeHooksDone() <-chan struct{} {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return nil
	}
	return gw.HooksDone()
}

func (i *Instance) Started() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.started
}

// IsExternalWorktree reports whether the instance's worktree is external/in-place
// (`af sessions create --here`, or a legacy external record) — the same flag
// MoveWorktree checks. Such a worktree is the user's own working tree and must
// never be relocated, so the daemon rejects archiving it (#1028). Returns false
// when the instance has no worktree yet.
func (i *Instance) IsExternalWorktree() bool {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	return gw != nil && gw.IsExternalWorktree()
}

// WorktreeCleanupImpact snapshots exactly what GitWorktree.Cleanup will remove.
// Destructive confirmation code consumes this instead of reconstructing cleanup
// ownership from capability flags, which do not distinguish AF-owned linked
// worktrees from in-place or user-branch worktrees.
type WorktreeCleanupImpact struct {
	Path           string
	Branch         string
	BaseCommitSHA  string
	RemoveWorktree bool
	DeleteBranch   bool
}

// GetWorktreeCleanupImpact returns a coherent description of Cleanup's targets.
// The GitWorktree ownership fields are immutable after construction.
func (i *Instance) GetWorktreeCleanupImpact() (WorktreeCleanupImpact, bool) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return WorktreeCleanupImpact{}, false
	}
	external := gw.IsExternalWorktree()
	return WorktreeCleanupImpact{
		Path:           gw.GetWorktreePath(),
		Branch:         gw.GetBranchName(),
		BaseCommitSHA:  gw.GetBaseCommitSHA(),
		RemoveWorktree: !external,
		DeleteBranch:   !external && gw.BranchCreatedByUs(),
	}, true
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
//
// It collapses IsAlive's tri-state to a bool, treating "could not ask" as NOT
// alive. That is safe for its callers — the TUI's attach/pane guards, which only
// refuse to attach — but it must never be used as evidence of liveness: take
// IsAlive directly for that (#1917 round 8).
func (i *Instance) TmuxAlive() bool {
	alive, err := i.currentBackend().IsAlive(i)
	return err == nil && alive
}

// ResolvedAgent returns the canonical agent (one of tmux.SupportedPrograms)
// this instance's pane will actually run, or "" when the resolved command
// runs no known agent — e.g. a program_overrides entry pointing an agent name
// at a plain shell (#1131). Agent-specific behavior (readiness heuristics,
// trust-prompt handling, flag injection) must key off this, never off
// Instance.Program: Program is the config-name enum the instance was created
// with, and an override may point it at a different program entirely (#1116).
//
// Once the tmux session exists, its program string (override-resolved and
// flag-injected by Start) is the ground truth. Before Start — or in tests
// that never attach a tmux session — detection falls back to the raw Program
// value, which also covers legacy free-form persisted values like
// "/home/foo/bin/claude --plugin-dir x" (#677).
func (i *Instance) ResolvedAgent() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.resolvedAgentLocked()
}

// ResolvedPaneAgent returns the canonical agent proven by this instance's
// concrete local tmux binding, or "" when there is no such binding or its
// command names no known agent. Unlike ResolvedAgent it deliberately never
// falls back to Instance.Program: callers describing an already-attached pane
// must not invent agent-specific behavior for remote tabs, whose real command
// was resolved inside the sandbox and is not represented by a local tmux
// session (#2210).
func (i *Instance) ResolvedPaneAgent() string {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil || strings.TrimSpace(ts.Program()) == "" {
		return ""
	}
	return tmux.DetectAgentFromCommand(ts.Program())
}

// SetTmuxSession sets the agent tab's tmux session for testing purposes,
// materializing the single Agent tab if needed.
func (i *Instance) SetTmuxSession(session *tmux.TmuxSession) {
	i.setTmuxLocked(session)
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

// AddTabForTest appends a tmux-less tab record. Test-only: UI tests (the
// sidebar tree, tab labels) need instances with a populated tab LIST without
// spinning up real tmux sessions; the tab is never attachable or previewable.
func (i *Instance) AddTabForTest(name string, kind TabKind) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Tabs = append(i.Tabs, &Tab{Name: name, Kind: kind})
}

// AddWebTabForTest appends a web tab carrying url. Test-only: the URL is the
// whole payload of a web tab, so tests that assert it survives a lifecycle step
// (archive → restore, #1809) need to seed one. It bypasses AddWebTab's started /
// tmux-bound preconditions, which a fake-backend instance cannot satisfy.
func (i *Instance) AddWebTabForTest(name, url string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Tabs = append(i.Tabs, &Tab{ID: newTabID(), Name: name, Kind: TabKindWeb, URL: url})
}
