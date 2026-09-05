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

// SetPrompt replaces the durable goal used by later limit resumes and handoffs.
// Prompt became mutable when handoff gained an operator-supplied brief, so the
// write and every concurrent reader must use the instance lock.
func (i *Instance) SetPrompt(prompt string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Prompt != prompt {
		i.Prompt = prompt
		i.touchLocked()
	}
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
	if i.pendingHandoffMission != mission {
		i.pendingHandoffMission = mission
		i.touchLocked()
	}
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
	if i.pendingHandoffMission != "" {
		i.pendingHandoffMission = ""
		i.touchLocked()
	}
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

// ArchiveWarning returns the bounded live notice for an incomplete archive.
// It is projection-only: the complete durable ownership report stays on the
// GitWorktree and storage projections scrub this string before writing disk.
func (i *Instance) ArchiveWarning() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.archiveWarning
}

// SetRuntimeTeardownForTest installs the physical reap a sandbox runtime would
// normally supply through ProvisionResult.Teardown.
//
// It exists because the daemon's remote fixtures could not construct one: the
// field is unexported, so a test in package daemon could only observe the
// /v1/agent/kill REST call and not the reap it is supposed to trigger. That let a
// regression which emitted the right message while leaving the container running
// pass — and a sandbox left alive is a VM still billing with no session record
// pointing at it, so nothing ever cleans it up (#3042).
//
// Deliberately narrow: it installs the callback and nothing else, so a test asserts
// the EFFECT through the same field production populates rather than through a
// better-chosen proxy. A better proxy is still a proxy.
//
// It clears the derived agentSrv cache in the SAME i.mu section, which every
// production writer of this field already does (bindProvisionResult,
// retainProvisionResultCleanup, resetRemoteRuntime) and which #1729 is about.
// remoteAgentServer captures teardown BY VALUE at build time, so without this a
// reap installed while the cache is warm — after any poll, preview or probe — is
// never invoked: the fixture observes zero reaps whatever production does, and a
// test asserting "nothing was reaped" passes unconditionally. That is #3042's own
// blind spot reproduced inside the helper meant to close it, and leaving it to
// call order across thirty-odd fixture call sites is not a guarantee.
func SetRuntimeTeardownForTest(i *Instance, teardown func() error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.runtimeTeardown != nil || teardown != nil {
		i.touchLocked()
	}
	i.agentSrv = nil
	i.runtimeTeardown = teardown
}

// SetSandboxBranch records the branch a SANDBOX session's own runtime reports,
// under the same mutex GetBranch reads it with.
//
// It exists because a sandbox session's daemon-side Branch has no other honest
// source. The in-sandbox provision creates the branch with the SANDBOX's config
// and never mutates this Instance, so the name reaches the daemon only as an
// Archive() return — from ArchiveSandbox, and now from the push recovery performs
// before replacing a reachable sandbox (#2923/#2925). The daemon must not derive
// it instead: the sandbox's branch_prefix may differ, and BranchForTitle appends a
// random suffix for titles that sanitize away, so a derived name would be
// confidently wrong — worse than the empty one it replaced.
func (i *Instance) SetSandboxBranch(branch string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Branch != branch {
		i.Branch = branch
		i.touchLocked()
	}
}

// MarkUserKilled records kill intent on the instance (#1108). Callers persist
// the instance afterwards so the tombstone survives a daemon crash mid-kill.
// Daemon callers reach this commit at serialized points: an explicit kill owns
// the per-session operation lock, while failed-create retention still owns the
// repo start lock and has not exposed the instance to another operation. Any
// carried process-local operation therefore has no live owner and must not
// outrank the durable tombstone or hide its retry action. A TUI retry owns its
// OpKilling on a separate projection instance and is preserved by snapshot
// reconciliation.
func (i *Instance) MarkUserKilled() {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, resetAt := i.lifecycleStateLocked()
	if !i.userKilled {
		i.userKilled = true
		i.touchLocked()
	}
	i.inFlightOp = OpNone
	i.noteStateChangeLocked(lv, op, resetAt)
}

// ReconcileUserKilledSnapshot applies the durable tombstone carried by a
// daemon snapshot to an already-materialized projection row. Tombstones are
// monotonic: an older snapshot cannot make a killed row live again. When the
// tombstone is first adopted, stale daemon operation markers must be cleared
// with it. OpKilling is different: snapshot reconciliation runs on the TUI's
// projection instance, so that marker belongs to the user's current teardown
// request and must survive even the first tombstone snapshot.
func (i *Instance) ReconcileUserKilledSnapshot(userKilled bool) bool {
	i.mu.Lock()
	defer i.mu.Unlock()

	lv, op, resetAt := i.lifecycleStateLocked()
	changed := false
	if userKilled && !i.userKilled {
		i.userKilled = true
		i.touchLocked()
		if i.inFlightOp != OpKilling {
			i.inFlightOp = OpNone
		}
		changed = true
	}
	i.noteStateChangeLocked(lv, op, resetAt)
	return changed
}

// UserKilled reports whether an explicit kill was recorded for this instance.
func (i *Instance) UserKilled() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.userKilled
}

// MarkStartupStateUnknown retains an uncertain create or restore as an inert
// record. Clearing started prevents attach/probe paths from treating the
// requested runtime name as confirmed; StartupStateUnknown keeps storage
// checkpoints from dropping the record merely because it is not started.
func (i *Instance) MarkStartupStateUnknown() {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, resetAt := i.lifecycleStateLocked()
	if !i.startupStateUnknown {
		i.startupStateUnknown = true
		i.touchLocked()
	}
	if i.started {
		i.started = false
		i.touchLocked()
	}
	// Startup-unknown is a terminal delivery outcome, not a run still consuming
	// the task's concurrency budget. Store that fact on the same transition that
	// stores the terminal marker so projections, persistence, and unloadable-row
	// accounting cannot disagree about whether the slot was released.
	// Same section, same reason as the completion transition (#3865): this is the
	// other edge that clears the run marker, so it is the other place the adoption
	// baseline has to be pinned. On the EDGE, not beside the assignment below,
	// which is unconditional — a second call once the run has already ended must
	// not pin a fresh baseline over a teardown's, since that would fold a delivery
	// made in between into the baseline and read as though nothing had happened.
	if i.taskRunActive {
		i.captureAdoptionBaselineLocked()
		i.taskRunActive = false
		i.touchLocked()
	}
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

// GetWorktreeRelocationCandidates returns both durable pathnames retained after
// a bounded worktree move ended without an answer. Neither path is authoritative
// while ok is true; lifecycle retry resolves their captured identity.
func (i *Instance) GetWorktreeRelocationCandidates() (primary, alternate string, ok bool) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return "", "", false
	}
	primary, recovery, ok := gw.RelocationSnapshot()
	if !ok {
		return "", "", false
	}
	return primary, recovery.AlternatePath, true
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
	if i.Title != title {
		i.Title = title
		i.touchLocked()
	}
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
	program := i.ResolvedPaneProgram()
	if strings.TrimSpace(program) == "" {
		return ""
	}
	return tmux.DetectAgentFromCommand(program)
}

// ResolvedPaneProgram returns the concrete command frozen onto the local agent
// pane at launch. It is empty when the instance has no local tmux binding.
func (i *Instance) ResolvedPaneProgram() string {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return ""
	}
	return ts.Program()
}

// SetTmuxSession sets the agent tab's tmux session for testing purposes,
// materializing the single Agent tab if needed.
func (i *Instance) SetTmuxSession(session *tmux.TmuxSession) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setTmuxLocked(session)
}

// SetStartedForTest toggles the started flag for testing purposes. Prefer
// Start() in non-test code; this exists so unit tests can exercise flows
// gated on Started() without spinning up a real tmux session.
func (i *Instance) SetStartedForTest(started bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.started != started {
		i.started = started
		i.touchLocked()
	}
}

// MarkLoadRuntimeReplacedForTest seeds the loader settlement owed by a
// confirmed Start(false) respawn. Production sets it only from LocalBackend.
func (i *Instance) MarkLoadRuntimeReplacedForTest() {
	i.markLoadRuntimeReplaced()
}

// SetPendingTabCleanupForTest seeds the unconfirmed tab-teardown handles a
// previous daemon would have left behind (#2669). Test-only: the real flow
// writes them from CloseTab's commit and reads them back through
// FromInstanceData, neither of which a daemon-package test can reach without
// staging a whole crashed close.
func (i *Instance) SetPendingTabCleanupForTest(pending []TabCleanupData) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pendingTabCleanup = append([]TabCleanupData(nil), pending...)
	i.touchLocked()
}

// SetGitWorktreeForTest assigns a git worktree to this instance. Test-only:
// the real flow sets this inside LocalBackend.Start, which isn't available
// in unit tests that use FakeBackend.
func (i *Instance) SetGitWorktreeForTest(gw *git.GitWorktree) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.gitWorktree != gw {
		i.gitWorktree = gw
		i.touchLocked()
	}
}

// AddTabForTest appends a tmux-less tab record. Test-only: UI tests (the
// sidebar tree, tab labels) need instances with a populated tab LIST without
// spinning up real tmux sessions; the tab is never attachable or previewable.
func (i *Instance) AddTabForTest(name string, kind TabKind) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Tabs = append(i.Tabs, &Tab{Name: name, Kind: kind})
	i.touchLocked()
}

// AddWebTabForTest appends a web tab carrying url. Test-only: the URL is the
// whole payload of a web tab, so tests that assert it survives a lifecycle step
// (archive → restore, #1809) need to seed one. It bypasses AddWebTab's started /
// tmux-bound preconditions, which a fake-backend instance cannot satisfy.
func (i *Instance) AddWebTabForTest(name, url string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Tabs = append(i.Tabs, &Tab{ID: newTabID(), Name: name, Kind: TabKindWeb, URL: url})
	i.touchLocked()
}
