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
		// Recording the obligation precedes submission, so this is positive
		// mission-scoped evidence that an automatic attempt is initially safe.
		i.handoffDeliveryStatus = PromptNotDelivered
		i.touchLocked()
	}
}

// PendingHandoffMission returns the takeover brief awaiting confirmed delivery.
func (i *Instance) PendingHandoffMission() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingHandoffMission
}

// BeginPendingHandoffMissionDelivery fails closed before submission. The caller
// persists this marker before touching the composer, closing the crash window in
// which an attempt may land without its verdict becoming durable.
func (i *Instance) BeginPendingHandoffMissionDelivery(mission string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission != mission || mission == "" {
		return fmt.Errorf("pending handoff mission changed before delivery")
	}
	if i.handoffDeliveryStatus != PromptCouldNotConfirm {
		i.handoffDeliveryStatus = PromptCouldNotConfirm
		i.touchLocked()
	}
	return nil
}

// RecordPendingHandoffMissionDelivery binds the runtime verdict to this exact
// mission. Empty and future verdicts are ambiguity, never retry authorization.
func (i *Instance) RecordPendingHandoffMissionDelivery(mission string, status PromptDeliveryStatus) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission != mission || mission == "" {
		return fmt.Errorf("pending handoff mission changed during delivery")
	}
	if !status.Valid() {
		status = PromptCouldNotConfirm
	}
	if i.handoffDeliveryStatus != status {
		i.handoffDeliveryStatus = status
		i.touchLocked()
	}
	return nil
}

// PendingHandoffDeliveryStatus returns the verdict recorded for the pending
// handoff mission, or "" when no mission is pending. A caller that raises the
// attempt marker reads it first, so a failed marker write can put back the
// verdict it replaced instead of assuming one.
func (i *Instance) PendingHandoffDeliveryStatus() PromptDeliveryStatus {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.handoffDeliveryStatus
}

// PendingHandoffMissionAutoRetryable permits automatic redelivery only after a
// mission-scoped observation proved that the exact pending mission did not land.
func (i *Instance) PendingHandoffMissionAutoRetryable() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingHandoffMission != "" && i.handoffDeliveryStatus == PromptNotDelivered
}

// pendingHandoffMissionNeedsFence reports whether a durable pending mission
// must reconstruct the OpReplacing fence on load (#4429). The fence protects
// the two verdicts that still own an in-flight obligation the daemon resolves
// itself — positive non-delivery (automatic replay owns the resend; the row
// stays inert until it lands) and the delivered crash window (the recovery
// settle owns the bookkeeping). Ambiguous verdicts deliberately load WITHOUT
// the fence: the remaining confirm-or-retry decision is the operator's, both
// exits prove the runtime before acting, and rebuilding the fence there
// manufactures the wedge. FromInstanceData spells out how an ambiguous verdict
// can become durable.
func pendingHandoffMissionNeedsFence(status PromptDeliveryStatus) bool {
	return status == PromptNotDelivered || status == PromptDelivered
}

// PendingHandoffMissionSettleable reports whether the pending mission's
// recorded verdict already proves delivery, so recovery can retire it without
// a resend or an operator attestation — the crash window between a delivered
// record and its clearing settle.
func (i *Instance) PendingHandoffMissionSettleable() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingHandoffMission != "" && i.handoffDeliveryStatus == PromptDelivered &&
		!i.userKilled
}

// CanRetryPendingHandoffMissionDelivery reports whether an operator can inspect
// the known incoming pane and explicitly override an ambiguous mission verdict.
// Positive non-delivery belongs to automatic recovery; delivered evidence and
// an unknown/missing runtime never authorize another submission.
//
// A startup-unknown row DOES admit the explicit retry (#4429). Its `started`
// bit is down, and every local send and pane capture refuses a row in that
// state, so the daemon first probes the pane under the op lock: only a runtime
// that answers restores `started` (ResolveStartupState) before the readiness
// wait and the send run. A probe that does not answer refuses the retry and
// leaves the row as it was.
func (i *Instance) CanRetryPendingHandoffMissionDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	ambiguous := ambiguousHandoffDelivery(i.handoffDeliveryStatus)
	// Dead rows keep their restore/kill handles; neither a resend nor an
	// attestation is meaningful against a runtime that no longer exists.
	dead := i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived
	return i.pendingHandoffMission != "" && ambiguous && !i.userKilled && !dead &&
		(i.inFlightOp == OpNone || i.inFlightOp == OpReplacing) &&
		(i.liveness == LiveRunning || i.liveness == LiveReady || i.startupStateUnknown)
}

// CanConfirmPendingHandoffDelivery reports whether an operator can retire the
// pending mission without resending it — the "it already landed" exit (#4429).
// The verdict must already be recorded and ambiguous-or-positive: an
// unrecorded or positively-absent delivery belongs to the send path, and
// not-delivered belongs to automatic recovery. Startup-unknown rows are the
// ones this verb exists FOR, so the flag is not a refusal here — the daemon
// probes the pane before honoring the attestation.
func (i *Instance) CanConfirmPendingHandoffDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.pendingHandoffMission == "" || i.userKilled ||
		(i.inFlightOp != OpNone && i.inFlightOp != OpReplacing) ||
		!confirmableHandoffDelivery(i.handoffDeliveryStatus) {
		return false
	}
	if i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived {
		return false
	}
	return i.startupStateUnknown ||
		i.liveness == LiveRunning || i.liveness == LiveReady || i.liveness == LiveLimitReached
}

// ConfirmPendingHandoffDelivery retires the pending handoff mission on the
// operator's attestation that it already landed (#4429): no resend, no new
// observation — the pane inspection happened at the terminal, not here. The
// daemon probes the runtime before calling; this method re-checks only the
// durable facts the attestation discharges.
//
// It resolves the whole wedge in one critical section: an OpReplacing fence
// settles through the CommitHandoff edge (the runtime was proven to reach this
// point — it accepted the paste), a startup-unknown flag lifts with started
// restored (the probe that admitted this call is the identity proof that flag
// was waiting for), and the mission plus its verdict clear together so no
// later reader reconstructs the fence. Refusing not-delivered keeps automatic
// recovery's ownership unambiguous.
func (i *Instance) ConfirmPendingHandoffDelivery(mission string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission == "" || i.pendingHandoffMission != mission {
		return fmt.Errorf("session %q has no pending handoff mission matching this confirmation", i.Title)
	}
	if i.userKilled {
		return fmt.Errorf("session %q has a pending kill", i.Title)
	}
	if i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived {
		return fmt.Errorf("session %q has no live runtime to confirm against (liveness %v); restore owns this row", i.Title, i.liveness)
	}
	if !confirmableHandoffDelivery(i.handoffDeliveryStatus) {
		return fmt.Errorf("session %q has no ambiguous handoff delivery to confirm (status %q); automatic recovery owns not-delivered missions", i.Title, i.handoffDeliveryStatus)
	}
	if i.inFlightOp != OpNone && i.inFlightOp != OpReplacing {
		return fmt.Errorf("session %q is busy (%v)", i.Title, i.inFlightOp)
	}
	lv, op, resetAt := i.lifecycleStateLocked()
	i.resolveStartupStateLocked()
	if i.inFlightOp == OpReplacing {
		if err := i.transitionLocked(CommitHandoff()); err != nil {
			return err
		}
	}
	i.pendingHandoffMission = ""
	i.handoffDeliveryStatus = ""
	i.touchLocked()
	i.noteStateChangeLocked(lv, op, resetAt)
	return nil
}

// resolveStartupStateLocked clears the startup-unknown fence once a fresh proof
// — a live-pane probe or an operator attestation accepted under it — has
// re-established the runtime binding. MarkStartupStateUnknown lifted `started`
// to keep attach/probe paths from trusting the unconfirmed name; restoring it
// here is the other half of the same fact. Caller holds i.mu. The task-run
// marker is deliberately untouched: runs only ever go true→false.
func (i *Instance) resolveStartupStateLocked() {
	if i.startupStateUnknown {
		i.startupStateUnknown = false
		i.touchLocked()
	}
	if !i.started {
		i.started = true
		i.touchLocked()
	}
}

// ResolveStartupState is the locking form of resolveStartupStateLocked. The
// daemon calls it once a liveness probe has answered: before an explicit retry
// sends to a startup-unknown row, and when recovery settles a delivered
// mission on a live row.
func (i *Instance) ResolveStartupState() {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, resetAt := i.lifecycleStateLocked()
	i.resolveStartupStateLocked()
	i.noteStateChangeLocked(lv, op, resetAt)
}

// ReconcilePendingHandoffSnapshot mirrors the daemon-owned agent handoff
// obligation onto an existing client projection. Open TUIs update rows in place,
// so copying only OpReplacing would leave the explicit retry predicate blind to
// the mission and its mission-scoped verdict until the client restarted.
func (i *Instance) ReconcilePendingHandoffSnapshot(mission string, status PromptDeliveryStatus) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission == mission && i.handoffDeliveryStatus == status {
		return false
	}
	i.pendingHandoffMission = mission
	i.handoffDeliveryStatus = status
	i.touchLocked()
	return true
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
		i.handoffDeliveryStatus = ""
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
		// RuntimeProgram stops being proof of a current runtime once teardown is
		// committed. Invalidate lock-free drift observers before publishing the
		// tombstone, including when OpNone means noteStateChangeLocked is a no-op.
		i.runtimeEvidenceGeneration.Add(1)
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
		i.runtimeEvidenceGeneration.Add(1)
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
		// RuntimeProgram stops being proof of a current runtime at this edge.
		// Advance the lock-free evidence generation before publishing the fence so
		// an asynchronous drift check cannot latch the previously known command.
		i.runtimeEvidenceGeneration.Add(1)
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

// RuntimeProgram returns durable evidence of the override-resolved base command
// used by the last positively established agent runtime. It is intentionally
// empty for legacy or uncertain records; Program is intent, not runtime proof.
func (i *Instance) RuntimeProgram() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runtimeProgram
}

// RuntimeProgramEvidence binds one runtime command to the lifecycle generation
// in which it was observed. Its generation is deliberately opaque outside the
// session package; callers can only validate it through Instance.
type RuntimeProgramEvidence struct {
	program    string
	generation uint64
}

// Program returns the resolved command captured by this evidence.
func (e RuntimeProgramEvidence) Program() string { return e.program }

// ObserveRuntimeProgram captures the command and its invalidation generation
// under the same instance lock.
func (i *Instance) ObserveRuntimeProgram() RuntimeProgramEvidence {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return RuntimeProgramEvidence{
		program:    i.runtimeProgram,
		generation: i.runtimeEvidenceGeneration.Load(),
	}
}

// RuntimeProgramEvidenceCurrent reports whether no runtime-command or
// lifecycle transition has superseded evidence since it was observed. It is
// lock-free so daemon callers can validate inside Manager.mu without reversing
// the manager/instance lock order.
func (i *Instance) RuntimeProgramEvidenceCurrent(evidence RuntimeProgramEvidence) bool {
	return evidence.generation == i.runtimeEvidenceGeneration.Load()
}

// CommitRuntimeProgramEvidence runs commit only while evidence still describes
// this instance's current runtime generation. The read lock is the commit
// boundary: every lifecycle or runtime replacement that invalidates evidence
// owns i.mu for writing, so either that invalidation lands first and commit is
// refused, or it waits until commit returns.
//
// commit must not call methods that acquire i.mu. It is intended for a small
// external side effect whose truth depends on this evidence, such as emitting a
// diagnostic about the runtime command.
func (i *Instance) CommitRuntimeProgramEvidence(evidence RuntimeProgramEvidence, commit func()) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if evidence.generation != i.runtimeEvidenceGeneration.Load() {
		return false
	}
	commit()
	return true
}

// setRuntimeProgram records a command only after a launch boundary positively
// established the replacement runtime. The surrounding lifecycle transition
// owns UpdatedAt and the durable checkpoint; touching here would count one
// runtime replacement twice.
func (i *Instance) setRuntimeProgram(program string) {
	if strings.TrimSpace(program) == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setRuntimeProgramLocked(program)
}

func (i *Instance) setRuntimeProgramLocked(program string) {
	if i.runtimeProgram == program {
		return
	}
	// Invalidate lock-free consumers before publishing the replacement value.
	// A consumer may validate while holding a different owner lock and therefore
	// cannot take i.mu to close this ordering edge.
	i.runtimeEvidenceGeneration.Add(1)
	i.runtimeProgram = program
}

// clearRuntimeProgramForUnverifiedReattach retires a persisted launch-command
// claim when load can establish only that a tmux name exists, not that it still
// names the process AF launched. It reports whether durable state changed so a
// load caller can checkpoint the clear before publishing the restored row.
func (i *Instance) clearRuntimeProgramForUnverifiedReattach() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.runtimeProgram == "" {
		return false
	}
	i.runtimeEvidenceGeneration.Add(1)
	i.runtimeProgram = ""
	return true
}

// SetTmuxSession sets the agent tab's tmux session for testing purposes,
// materializing the single Agent tab if needed.
func (i *Instance) SetTmuxSession(session *tmux.TmuxSession) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setTmuxLocked(session)
	// This method exists only for tests. Installing a concrete test pane is their
	// positive launch boundary, so carry the same runtime evidence production
	// launch paths record after Start/Restore succeeds.
	if session != nil && strings.TrimSpace(session.Program()) != "" {
		i.setRuntimeProgramLocked(session.Program())
	}
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
	i.setGitWorktreeLocked(gw)
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
