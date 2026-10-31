package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func (i *Instance) RepoName() (string, error) {
	if i.Capabilities().Workspace != WorkspaceLocalWorktree {
		// An off-box row owns no local worktree, but the daemon records which
		// project it belongs to; that identity is what a multi-repo rail
		// disambiguates by (W8.8).
		i.mu.RLock()
		repoPath := i.repoIdentityPath
		if repoPath == "" {
			repoPath = i.Path
		}
		i.mu.RUnlock()
		if repoPath == "" {
			return "", fmt.Errorf("remote instance has no recorded repo")
		}
		return filepath.Base(filepath.Clean(repoPath)), nil
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

// CallbackUnavailable returns the reason this session has no af callback, or ""
// when one was delivered (or the session's backend never injects one). Surfaces
// render it prefixed by CallbackUnavailablePrefix.
func (i *Instance) CallbackUnavailable() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.callbackUnavailable
}

// AgentConfigSkipped returns the "agent:file: <reason>" entries for the declared
// sandbox_agent_config files the account-scope placement refused, or nil when
// every declared file landed (or none applied). Surfaces render the joined
// entries prefixed by AgentConfigSkippedPrefix. The slice is a copy: a renderer
// must never mutate the row's own list.
func (i *Instance) AgentConfigSkipped() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]string(nil), i.agentConfigSkipped...)
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

// owesNoRuntimeTeardownLocked reports whether a sandbox-backed instance
// positively owes no off-box reap — i.e. its backend carries no durable cleanup
// identity at all (the cleanup field is nil). It is the EARNED-EVIDENCE half of
// the KilledWithoutLiveRuntime marker staging in toInstanceDataLocked.
//
// Caller holds i.mu (read or write) and hands over the backend pointer it
// already read. It must NOT acquire i.mu itself: sync.RWMutex is not reentrant,
// so a nested RLock from a caller holding the read lock deadlocks the moment a
// writer queues between the two acquisitions, and from a caller holding the
// write lock it deadlocks unconditionally. The snapshot path (toInstanceDataLocked
// — whose contract is that the caller already holds i.mu) calls this, so any lock
// acquisition here would wedge snapshot, persistence, and kill behind a session
// mutex that never releases.
//
// The necessity of the second condition besides `runtimeTeardown == nil` is the
// f58ca97f wedge-fix gone wrong: a sandbox row reloaded INERT after a daemon
// restart ALWAYS has `runtimeTeardown == nil` (the inert backend binds no live
// teardown), regardless of whether its off-box runtime survived — so gating the
// "nothing owed" marker on that nil alone laundered a still-live ssh workspace or
// hook-provisioned sandbox into a no-op kill and deleted the only record that
// pointed at it. Persisting RuntimeCleanup for ordinary sandbox rows (ForStorage)
// and rebuilding it at load (FromInstanceData) puts a real teardown handle back on
// a reloaded LIVE row, so this `runtimeTeardown == nil` arm stops firing for it;
// the marker is staged only when the backend itself positively carries no
// cleanup identity, which is the always-inert shape the original wedge fix meant
// to admit.
func owesNoRuntimeTeardownLocked(backend Backend) bool {
	if backend == nil {
		return true
	}
	provider, ok := backend.(runtimeCleanupProvider)
	if !ok {
		return true
	}
	return provider.runtimeCleanupData() == nil
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
	// The caller learned this name from a push that just landed, so it is a
	// restore checkpoint now, not just a chosen name (#5972).
	if branch != "" && i.branchUnpushed {
		i.branchUnpushed = false
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
		// The tombstone permanently disables automatic restore, so an armed
		// environmental wait becomes a promise nothing can keep — every client
		// would report "waiting on the environment" while only the tombstone's
		// cleanup loop runs. It commits clear with the tombstone (#5295 review).
		i.lostRestoreWait = LostRestoreWait{}
		i.attention = nil
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
		// Same retirement as the daemon's own commit: a tombstoned row can
		// never be auto-restored, so a mirrored armed-wait is a lie here too
		// (#5295 review).
		i.lostRestoreWait = LostRestoreWait{}
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

// TaskRunActive reports whether this session's task run is still in flight
// (#1892). Prefer LifecycleView when the answer is combined with any other piece
// of state: a verdict assembled from separate accessor calls can straddle a
// concurrent transition.
func (i *Instance) TaskRunActive() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.taskRunActive
}

// TaskRunPromptAttemptAt is the run's own prompt boundary (#5219). The daemon's
// pane-chrome watcher keys its state to it: a redelivery must discard primed
// rows that predate the new boundary, or a turn already in flight releases the
// fresh window as though it were the redelivered prompt's (#5221 review).
func (i *Instance) TaskRunPromptAttemptAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.taskRunPromptAttemptAt
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
// flag-injected by Start) is the ground truth. A remote session has no local
// tmux binding; the command its runtime launched is still on record, because
// the remote launch boundary writes the same runtime_program evidence the
// local backend writes (#5067, #5108). Only when neither exists — before
// Start, or for a runtime that recorded nothing — does detection fall back to
// the raw Program value, which also covers legacy free-form persisted values
// like "/home/foo/bin/claude --plugin-dir x" (#677).
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

// ResolvedPaneGeneratedArgs returns the af-authored argument words recorded for
// the local agent pane's frozen program, or nil when the instance has no local
// tmux binding. It surfaces TmuxSession.GeneratedArgs through the instance so
// validateAccountSwapPlan can seed a launch proof whose base is the pane's
// frozen program: that pane command already carries af's own injected flags,
// so re-deriving the declaration from that base treats af's words as undeclared
// user arguments and the swap is refused (#5433). The recorded declaration is
// the provenance the pane carries beside the command string, and is exactly
// what the proof needs to trust them.
func (i *Instance) ResolvedPaneGeneratedArgs() []string {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return nil
	}
	return ts.GeneratedArgs()
}

// ResolvedPaneTrustedExecutable returns the exact executable path af's built-in
// program detection recorded for the local agent pane's frozen program, or empty
// when the launch recorded none. It surfaces TmuxSession.TrustedExecutable so a
// launch proof seeded from the pane's frozen program (the #5433 pane-fallback)
// carries the pane's recorded executable provenance instead of inferring the
// base's first word: an untrusted absolute override records an empty
// TrustedExecutable deliberately, and inferring the path would launder a
// repository-controlled executable into the account boundary's trust
// (internal/sessionenv/account.go:47-54, account_command.go:269-276).
func (i *Instance) ResolvedPaneTrustedExecutable() string {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return ""
	}
	return ts.TrustedExecutable()
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
	i.setRuntimeLaunch(program, nil)
}

// setRuntimeLaunch records a positively established launch: the resolved base
// command plus, when the pane answers, the pane root's (pid, kernel start-time)
// identity — the pair that lets a later reattach prove the process standing
// behind the reused tmux name is still the launch af made (#5066). A pane query
// that cannot answer records the command alone; the record then verifies
// nothing on reattach and ages out exactly like a pre-evidence launch.
func (i *Instance) setRuntimeLaunch(program string, ts *tmux.TmuxSession) {
	if strings.TrimSpace(program) == "" {
		return
	}
	pid := 0
	var startID uint64
	if ts != nil {
		if pane, err := ts.PaneRootProcess(); err == nil {
			pid, startID = pane.PID, pane.StartID
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setRuntimeEvidenceLocked(program, pid, startID)
}

func (i *Instance) setRuntimeProgramLocked(program string) {
	i.setRuntimeEvidenceLocked(program, 0, 0)
}

func (i *Instance) setRuntimeEvidenceLocked(program string, pid int, startID uint64) {
	if i.runtimeProgram == program && i.runtimePID == pid && i.runtimeStartID == startID {
		return
	}
	// Invalidate lock-free consumers before publishing the replacement value.
	// A consumer may validate while holding a different owner lock and therefore
	// cannot take i.mu to close this ordering edge.
	i.runtimeEvidenceGeneration.Add(1)
	i.runtimeProgram = program
	i.runtimePID = pid
	i.runtimeStartID = startID
}

// settleReattachVerification probes the pane behind the reused tmux name ONCE
// and settles the recorded runtime claim to match the answer: a positive
// (pid, start-time) match keeps the claim and reports verified; anything else
// — a different process, an unanswerable probe, a record carrying no identity
// — is unverified, and the persisted claim is retired if there was one. Both
// answers come from the SAME probe so the task-run verdict and the evidence
// clear can never disagree about which pane answered (#4222 review: two
// separate probes let a pane respawned between them read verified to the
// interruption classifier while the clear saw the mismatch).
//
// verified answers the task-run question on the Lost-restore path — a pane
// that appears while the session was believed dead and cannot prove it is the
// prompted agent is treated as a replacement. claimRetired is the load path's
// deliberately narrower predicate: a tmux name surviving a routine daemon
// restart is the EXPECTED same-runtime case, so only a recorded claim that
// failed verification marks a replacement there — a row carrying none (a
// legacy record) has nothing to distrust and stays a pure reattach.
func (i *Instance) settleReattachVerification(ts *tmux.TmuxSession) (verified, claimRetired bool) {
	i.mu.RLock()
	oldProgram, pid, startID := i.runtimeProgram, i.runtimePID, i.runtimeStartID
	i.mu.RUnlock()
	if pid > 0 && ts != nil {
		if same, err := ts.PaneRootProcessMatches(pid, startID); err == nil && same {
			// Provably the same launch: keep the claim.
			return true, false
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	// Re-check under the write lock: a launch boundary that committed fresh
	// evidence while the pane query ran must not lose it to this verdict.
	if i.runtimeProgram != oldProgram || i.runtimePID != pid || i.runtimeStartID != startID {
		return false, false
	}
	if i.runtimeProgram == "" && i.runtimePID == 0 {
		return false, false
	}
	i.runtimeEvidenceGeneration.Add(1)
	i.runtimeProgram = ""
	i.runtimePID = 0
	i.runtimeStartID = 0
	return false, true
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

// SetBranchForTest sets the branch name recorded on the instance. Test-only:
// the real flow sets it inside LocalBackend.Start, which unit tests that wire a
// worktree with SetGitWorktreeForTest never run.
func (i *Instance) SetBranchForTest(branch string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Branch != branch {
		i.Branch = branch
		i.touchLocked()
	}
}

// MarkLoadRuntimeReplacedForTest seeds the loader settlement owed by a
// confirmed Start(false) respawn. Production sets it only from LocalBackend.
func (i *Instance) MarkLoadRuntimeReplacedForTest() {
	i.markLoadRuntimeReplaced()
}

// RebuildTeardownPending reports whether a recovery rebuilt the worktree at a
// new identity without confirming the stale-pane teardown — see the field's
// comment on Instance for the retry hazard it closes.
func (i *Instance) RebuildTeardownPending() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.rebuildTeardownPending
}

// refuseInputWhileRebuildTeardownPending is the input-side twin of the
// liveness guards: while a rebuild teardown obligation is owed, the pane a
// client is typing into may still be the STALE one bound to the deleted
// inode — accepted keystrokes either execute in the void or die with the
// teardown kill moments later (#5371 review). Liveness alone cannot say this
// (a committed retry restores LiveRunning/LiveReady for its owner), so the
// durable flag itself is the fence. The obligation's owner does not type —
// recover/resume run through their own seams — so nothing legitimate is
// blocked.
func (i *Instance) refuseInputWhileRebuildTeardownPending() error {
	if i.RebuildTeardownPending() {
		return fmt.Errorf(
			"session %q still owes the teardown of the runtime its rebuilt worktree replaced; input is refused until its recovery finishes",
			i.Title)
	}
	return nil
}

// SetRebuildTeardownPending records or clears the stale-pane teardown
// obligation a worktree rebuild commits. Transitions touch the row so the
// durable state survives the daemon, not only the attempt that set it.
//
// A transition also fires rebuildTeardownNotify — the daemon's synchronous
// row write — so a restart can never reload the flag a successful attempt
// already discharged: clearing without that checkpoint would let a daemon
// exit after the replacements spawned but before the settlement write
// resurrect `true` and kill the fresh panes on the next startup. On a failed
// clear-checkpoint the marker is restored and the error returned, so the
// caller keeps the row Lost instead of spawning on an unrecorded discharge.
// A failed set-checkpoint likewise keeps the flag set — the rebuild has
// committed either way — and propagates the error so the attempt reports it.
// A nil notify is a failed checkpoint too, not a skipped one: every respawn
// entry point that can rebuild installs the callback, so reaching a
// transition without it means an entry point was missed (#5371 review).
func (i *Instance) SetRebuildTeardownPending(pending bool) error {
	i.mu.Lock()
	if i.rebuildTeardownPending == pending && !i.rebuildTeardownPendingOwed {
		i.mu.Unlock()
		return nil
	}
	i.rebuildTeardownPending = pending
	i.rebuildTeardownPendingOwed = true
	notify := i.rebuildTeardownNotify
	i.touchLocked()
	i.mu.Unlock()
	if notify == nil {
		// A transition that cannot checkpoint is a transition that did not
		// happen durably: a restart reloads the pre-transition value, which for
		// both directions is "the obligation may exist". Keep the flag set and
		// refuse, so a respawn entry point that forgot to install the callback
		// cannot quietly skip the checkpoint it exists to make (#5371 review).
		i.mu.Lock()
		i.rebuildTeardownPending = true
		i.touchLocked()
		i.mu.Unlock()
		return fmt.Errorf("rebuild teardown transition has no durable checkpoint: install SetRebuildTeardownNotify before recovering")
	}
	if err := notify(i); err != nil {
		// The transition did not land durably, so the disk record is still the
		// pre-transition value — for BOTH directions that is "the obligation may
		// exist": either it was just committed and unrecorded, or its discharge
		// was refused. Leaving the flag set keeps memory honest with the disk a
		// restart would reload, and the error tells the caller to stay Lost.
		// owed stays true, so the next matching call RETRIES the checkpoint
		// rather than treating the equal in-memory value as durable — the
		// arm-before-rebuild property is only real if a failed arm is retried
		// before the rebuild commits (#5371 review).
		i.mu.Lock()
		i.rebuildTeardownPending = true
		i.touchLocked()
		i.mu.Unlock()
		return err
	}
	i.mu.Lock()
	i.rebuildTeardownPendingOwed = false
	i.mu.Unlock()
	return nil
}

// SetRebuildTeardownNotify installs the synchronous persist callback a
// rebuildTeardownPending transition fires. It is in-memory — every recover
// entry point that can reach the rebuild teardown installs it for the
// attempt about to run; SetRebuildTeardownPending refuses a transition that
// has no callback to make it durable.
func (i *Instance) SetRebuildTeardownNotify(notify func(*Instance) error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rebuildTeardownNotify = notify
}

// SetRebuildTeardownRuntimesStop installs the daemon's stop for everything a
// session owns that no tmux session carries: the supervised VS Code editor
// (a daemon-owned process rooted at the worktree) plus any runtimes the
// daemon adds there later. The rebuild teardown calls it after every bound
// tmux name is closed and BEFORE the occupancy gate — an unstopped one would
// answer that check as a stranger — and a nil callback fails the teardown
// closed, the same missed-entry-point contract the persist notify keeps
// (#5371 review).
func (i *Instance) SetRebuildTeardownRuntimesStop(stop func(*Instance) error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rebuildTeardownStopRuntimes = stop
}

// MarkAgentRuntimeReplacedForTest seeds the replacement verdict a backend
// stamps on a recovered runtime (#4222). Production sets it only from backend
// recover/load paths; tests use it to model a respawn vs a verified reattach
// around a ConfirmLive edge.
func (i *Instance) MarkAgentRuntimeReplacedForTest(replaced bool) {
	i.markAgentRuntimeReplaced(replaced)
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

// GitWorktreeForTest returns the instance's git worktree. Test-only: daemon
// tests need the restored worktree itself to install an adopted-hook surrogate
// (GitWorktree.SetHooksDoneForTest) — the channel a real restart would rebuild
// via AdoptRunningHookRuns (#4162).
func (i *Instance) GitWorktreeForTest() *git.GitWorktree {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.gitWorktree
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

// AddTabWithIDForTest is AddTabForTest with a chosen stable id — the identity an
// off-box tab is streamed by (#5571 W4.8), which a bare AddTabForTest leaves empty.
func (i *Instance) AddTabWithIDForTest(id, name string, kind TabKind) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Tabs = append(i.Tabs, &Tab{ID: id, Name: name, Kind: kind})
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
