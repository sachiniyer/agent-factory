package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// currentBackend snapshots the instance's backend under i.mu (#2096). The
// backend is NOT immutable: a restore/recover of an off-box session rebinds it
// via bindProvisionResult under i.mu.Lock, and the restore paths consult the
// instance (Capabilities, liveness) before taking the per-instance opLock, so a
// bare field read genuinely races that write.
//
// Every read goes through here — or through the *Locked variant below — and the
// returned Backend is then used OUTSIDE the lock: the delegated calls
// (Start/Recover/Preview/…) block on tmux, docker, and ssh I/O, and several
// re-enter i.mu, so holding it across them would deadlock. Snapshot-then-call
// only guarantees the pointer read is synchronized; serializing an operation
// against a concurrent rebind is the opLock's job, not this lock's.
//
// Callers must not already hold i.mu — sync.RWMutex is not reentrant, and a
// recursive RLock deadlocks the moment a writer queues between the two
// acquisitions. Code that already holds the lock uses capabilitiesLocked.
func (i *Instance) currentBackend() Backend {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.backend
}

// firstTimeSetup is true if this is a new instance. Otherwise, it's one loaded from storage.
func (i *Instance) Start(firstTimeSetup bool) error {
	return i.currentBackend().Start(i, firstTimeSetup)
}

// Kill terminates the instance and cleans up all resources. It delegates to the
// agent-server's Kill (not backend.Kill directly) so the WS PTY broker is torn
// down FIRST: every open subscriber's NextEvent returns io.EOF and the clientless
// capture goroutine stops, instead of hanging until the WS keepalive lapses and
// leaking the capture goroutine when a session is killed with a live stream open
// (#1632). The agent-server then kills the underlying session.
func (i *Instance) Kill() error {
	return i.AgentServer().Kill()
}

// Recover re-establishes a Lost instance's backing session (#1108). Called by
// the daemon's restore loop and by user-initiated restore (#1300); loads stay
// side-effect free (#970). Lifecycle eligibility is enforced here, before any
// backend can provision or spawn a runtime; backend capability alone is never
// permission to revive a row.
func (i *Instance) Recover() error {
	if err := i.ValidateRuntimeAction(RuntimeActionRecoverLost); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	return i.currentBackend().Recover(i)
}

// Respawn re-establishes the instance's backing session in place without a
// liveness precondition — the guard-free core of Recover. The usage-limit
// manual-retry (#1146, resumeFromLimit) uses it to re-spawn an agent that exited
// while blocked at a limit wall: that session is LiveLimitReached, which Recover's
// !Lost guard rejects, but the re-spawn mechanics are identical. The caller owns
// the precondition, enforced here before the guard-free backend core runs.
// BeginLimitResume validates the limit-resume precondition and raises the
// OpRespawning fence for the WHOLE resume, in one critical section. The caller owns
// it until EndLimitResume (or until ConfirmLive clears it on success).
//
// It is a separate method from Respawn for the reason SwapAgentProgram and
// RecordHandoffSwap are separate: the two legal orderings are made explicit rather
// than folded into one re-entrant call that silently does different things.
//
// The fence must cover the resume's whole destructive sequence, not just the backend
// call (#2997, and #3004 review). For an answered-dead REMOTE agent the daemon
// probes, then pushes the sandbox's unpushed work and durably records its branch,
// and only then re-spawns. That push is a network git operation and the longest
// phase of the resume; run unfenced it is exactly the window the poll walks into,
// observing the dead agent and applying LiveLost while no op is in flight. The
// resume then loses its own precondition and refuses, so the queued prompt is never
// delivered — the harm the fence exists to prevent, reached before the fence existed.
//
// Validation and the raise share i.mu because splitting them is its own race: a
// current observation landing in the gap is not stale, so the epoch guard cannot
// drop it, and tkBeginRespawn is keyed on the op axis alone — it would happily fence
// a liveness that had just been clobbered. See lifecycleViewLocked, which documents
// this as the pattern.
func (i *Instance) BeginLimitResume() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionResumeLimit); err != nil {
		return err
	}
	return i.transitionLocked(BeginRespawn())
}

// EndLimitResume lowers the fence BeginLimitResume raised. Safe to defer
// unconditionally: it is a no-op once ConfirmLive has cleared the op on the success
// path, and it never disturbs an op some other owner raised.
//
// Lowering it is not optional on the failure paths. A stranded fence leaves the
// session permanently busy — the poll skips it forever and every runtime action and
// lifecycle control refuses it as in-flight — which is worse than the clobber this
// whole mechanism exists to prevent.
// ClearOp is unconditionally legal (it only ever moves the op axis back to None and
// leaves liveness alone), so the OpRespawning check is not about legality: it makes
// sure a kill or archive overlay that SUPERSEDED this fence is not cleared out from
// under its own owner.
// Reports whether it actually lowered the fence, so a caller that must announce the
// released state to its clients can tell an effective release from a no-op and not
// publish a duplicate settled event on the path that already published one.
func (i *Instance) EndLimitResume() bool {
	if i.GetInFlightOp() != OpRespawning {
		return false
	}
	if err := i.Transition(ClearOp()); err != nil {
		log.WarningLog.Printf("limit resume: clearing the in-flight fence for %q: %v", i.Title, err)
		return false
	}
	return true
}

// Respawn re-spawns this session's runtime. It REQUIRES the caller to hold the
// limit-resume fence (BeginLimitResume): the fence has to be up before the daemon's
// probe-and-preserve phase, which runs well before this call, so raising one here
// would be too late to protect the sequence it belongs to.
//
// On success the backend ends in ConfirmLive, which is allowed from OpRespawning and
// clears it. On failure the fence stays up and the caller's EndLimitResume lowers it.
func (i *Instance) Respawn() error {
	if op := i.GetInFlightOp(); op != OpRespawning {
		return fmt.Errorf("respawn of %q requires the limit-resume fence (in-flight op is %s)", i.Title, opLabel(op))
	}
	return i.currentBackend().Respawn(i)
}

// PrepareAgentSwap resolves and validates the incoming launch while the outgoing
// agent is still untouched. The returned immutable plan is the only value
// SwapAgent accepts, so the checked command and the launched command cannot drift.
func (i *Instance) PrepareAgentSwap(target string) (AgentSwapPlan, error) {
	if err := i.ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		return AgentSwapPlan{}, err
	}
	if err := i.ValidateHandoffTarget(target); err != nil {
		return AgentSwapPlan{}, err
	}
	if !i.Capabilities().Handoff {
		return AgentSwapPlan{}, ErrHandoffUnsupported
	}
	return i.currentBackend().PrepareAgentSwap(i, target)
}

// SwapAgent executes a prepared runtime replacement. The daemon must already
// have raised OpReplacing and recorded plan.target as Instance.Program. Success
// deliberately leaves that fence raised: the replacement is not a completed
// handoff until the daemon has delivered (or explicitly parked) its mission.
func (i *Instance) SwapAgent(plan AgentSwapPlan) (InstanceData, error) {
	view := i.LifecycleView()
	if view.InFlightOp != OpReplacing {
		return InstanceData{}, fmt.Errorf("session %q has no agent replacement in flight", i.Title)
	}
	if !i.Capabilities().Handoff {
		return InstanceData{}, ErrHandoffUnsupported
	}
	if target := i.AgentProgram(); target != plan.target || strings.TrimSpace(plan.program) == "" {
		return InstanceData{}, fmt.Errorf("session %q handoff plan no longer matches its recorded target", i.Title)
	}
	if plan.conversation.HasID() {
		i.SetAgentConversation(plan.conversation)
	}
	if err := i.currentBackend().SwapAgent(i, plan); err != nil {
		return InstanceData{}, err
	}
	// Returning the durable projection from the successful runtime operation
	// makes it impossible for a caller to checkpoint the target before the
	// backend has actually established it.
	return i.handoffStorageCheckpoint(), nil
}

// ArchiveTeardown tears down every tab's tmux session for an archive AND
// relocates the worktree to dest in one operation (#1028) — the tmux half of
// Kill, but it PRESERVES the record and MOVES the worktree instead of deleting
// it. It routes through the shared teardownTabs core in the archive mode, so the
// #802 "wait for every pane to exit before touching the worktree" ordering is
// shared code with Kill rather than the duplicated prose it was when the move
// lived in a separate daemon step (#1195 Phase 2b). It is deliberately
// best-effort for tmux (a stuck session only logs, mirroring Kill) and:
//   - keeps the AGENT tab's tmux binding (its session name) so a failed archive
//     can re-spawn it in place via the Lost-restore loop;
//   - drops the shell/process tabs entirely — their tmux sessions were just torn
//     down, so only the agent session is brought back for them (Sachin's #1028
//     requirement);
//   - KEEPS the web tabs (#1809): a web tab has no tmux session and no process —
//     it is just a URL — so nothing was torn down and it round-trips through the
//     archived record to render again on un-archive;
//   - leaves gitWorktree and started untouched, so the daemon caller controls
//     the final state (started=false + Archived on success; Lost on a failed
//     move — returned here — where started stays true so the loop re-spawns the
//     agent).
//
// Returns the worktree-move error (nil on success). Local instances only —
// remote sessions have no local tmux/worktree and the daemon rejects archiving
// them before reaching here.
func (i *Instance) ArchiveTeardown(dest string) error {
	_, err := i.ArchiveTeardownWithHook(dest, nil)
	return err
}

// ArchiveTeardownWithHook is ArchiveTeardown with one additional operator
// callback at the only safe cleanup point: every pane has been confirmed dead,
// but the worktree still occupies its live path. A callback failure is returned
// separately from the relocation result and never prevents the move.
func (i *Instance) ArchiveTeardownWithHook(dest string, beforeMove func() error) (hookErr, archiveErr error) {
	mode := teardownArchive{dest: dest, beforeMove: beforeMove, hookErr: &hookErr}
	archiveErr = i.teardownTabs(mode)
	return hookErr, archiveErr
}

// SetArchived flips the instance into the inert Archived state atomically:
// started=false (no tmux binding backs it) and liveness=Archived, clearing any
// in-flight op. Called by the daemon after a successful archive move.
func (i *Instance) SetArchived() {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, resetAt := i.lifecycleStateLocked()
	i.started = false
	i.liveness = LiveArchived
	i.inFlightOp = OpNone
	i.clearAgentModelChangeLocked()
	i.noteStateChangeLocked(lv, op, resetAt)
}

// RestoreArchivedWorktree moves this instance's archived worktree back to dest
// and re-registers it against the origin repo (#1028). Surfaces git.ErrRepoGone
// when the repo has been deleted so the caller can leave the archive intact.
func (i *Instance) RestoreArchivedWorktree(dest string) error {
	i.mu.RLock()
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionRestoreArchived); err != nil {
		i.mu.RUnlock()
		return err
	}
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot restore %q: instance has no worktree", i.Title)
	}
	return gw.RestoreWorktreeTo(dest)
}

// ArchivedBranchForReclaim reports the branch an archived session is holding
// when — and only when — that branch may safely be renamed aside so a new session
// can take its title (#2127). ok is false whenever it may not be, and the caller
// must then leave the branch alone.
//
// It lives here rather than in the daemon because the archived instance's
// worktree is not reachable through GetGitWorktree: that accessor is gated on
// `started`, which archiving clears, so a caller outside this package cannot ask
// the question at all. Answering it here also keeps every read of gitWorktree
// under i.mu, like the rest of this file.
//
// Four declines, each one a case where renaming the user's branch is worse than
// refusing the create:
//
//   - Not a local worktree (hook/docker/ssh): there is no local branch to move.
//   - No worktree or no recorded branch: nothing to reclaim.
//   - An EXTERNAL worktree (`--here`, or a pre-#930 adopted checkout). af adopted
//     that branch rather than creating it; renaming it is not af's call.
//   - PUBLISHED, or an upstream that could not be determined. A rename desyncs a
//     pushed branch's local name from the remote it tracks and from any open PR.
//     The unknown case declines for the same reason: a probe that cannot answer
//     must not be what authorizes rewriting a user's branch.
func (i *Instance) ArchivedBranchForReclaim() (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveArchived {
		return "", false
	}
	// capabilitiesLocked, NOT Capabilities: this method already holds i.mu.RLock,
	// and Capabilities re-takes it. sync.RWMutex is not reentrant, so a nested
	// RLock deadlocks against a queued writer (#2006) — and this runs on the
	// m.mu-held create path (reserveCreate -> reclaimArchivedBranchLocked), so the
	// wedge would take the whole daemon create path with it. capabilitiesLocked is
	// the lock-holder variant that exists for exactly this.
	if i.capabilitiesLocked().Workspace != WorkspaceLocalWorktree {
		return "", false
	}
	gw := i.gitWorktree
	if gw == nil || gw.GetBranchName() == "" || gw.IsExternalWorktree() {
		return "", false
	}
	if published, known := gw.BranchIsPublished(); published || !known {
		return "", false
	}
	return gw.GetBranchName(), true
}

// ArchivedCandidateBranchIsFree reports whether `candidate` is a branch name the
// archived session's worktree can be renamed ONTO — free, and confirmed free
// (#2127, P3 on #2465). It exists for the same reason as ArchivedBranchForReclaim:
// the archived worktree is not reachable through GetGitWorktree, so the daemon
// cannot run the check itself.
//
// free is false BOTH when a branch of that name already exists (git refuses to
// rename onto it) and when existence could not be determined — an unknown answer
// is treated as taken, so the reclaim declines rather than renaming onto a name it
// could not rule out.
func (i *Instance) ArchivedCandidateBranchIsFree(candidate string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	gw := i.gitWorktree
	if gw == nil {
		return false
	}
	exists, ok := gw.BranchExists(candidate)
	return ok && !exists
}

// RenameArchived atomically relocates an archived instance's worktree to dest (a
// new title-keyed archive dir) and updates its Title, so a fresh session can reuse
// the archived session's name (feat: reuse archived name). Both mutations happen
// under i.mu so a concurrent Snapshot/ToInstanceData never observes a torn state
// (new title paired with the old worktree path, or vice versa). Archived instances
// are inert — no async Start/Recover goroutine touches them — so holding i.mu
// across the git move only blocks a brief Snapshot RLock, never a live operation.
//
// The stable id and worktree contents are preserved: only the on-disk directory +
// git's two-way registration move, and only the display title changes.
// On a relocation failure the worktree and title are left untouched and the error
// is surfaced, so the caller can abort the reuse without having half-renamed the
// archived session.
//
// newBranch, when non-empty, moves the BRANCH aside with the title (#2127).
// Freeing the title alone was never enough: archiving relocates the worktree
// rather than removing it (#2013), so the archived session keeps its branch
// checked out, and the new session — which derives that same branch — then failed
// at `git worktree add` on a name the rename was supposed to have freed. Empty
// keeps the branch where it is, which is what a session with no local branch to
// move (a hook/sandbox workspace) needs.
//
// Branch first, worktree second, and the branch is put back if the worktree move
// fails. Both orders leave a window; this one's window is the cheap, exactly
// reversible half — a renamed branch with the worktree still at its old path is
// undone by one more rename, whereas a moved worktree whose branch rename then
// failed would need the bytes moved back to recover.
func (i *Instance) RenameArchived(newTitle, dest, newBranch string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.liveness != LiveArchived {
		return fmt.Errorf("cannot rename session %q: it is not archived", i.Title)
	}
	gw := i.gitWorktree
	if gw == nil {
		return fmt.Errorf("cannot rename archived session %q: it has no worktree to relocate", i.Title)
	}
	oldBranch := gw.GetBranchName()
	if newBranch != "" && newBranch != oldBranch {
		if err := gw.RenameBranch(newBranch); err != nil {
			return fmt.Errorf("cannot free the archived branch of %q: %w", i.Title, err)
		}
	}
	// MoveWorktree relocates the bytes + repairs git's registration and, on success,
	// updates gw's stored worktree path — all under i.mu here, matching how
	// ToInstanceData reads the worktree path under i.mu.RLock.
	if err := gw.MoveWorktree(dest); err != nil {
		if newBranch != "" && newBranch != oldBranch {
			// Best-effort: the move already failed, so this is recovery, and a
			// second failure must not mask the first. It is reported with it —
			// a branch left under the new name while the record still says the
			// old one is exactly the drift a silent rollback would hide.
			if rbErr := gw.RenameBranch(oldBranch); rbErr != nil {
				return fmt.Errorf("%w (and the branch could not be renamed back from %q to %q: %v)",
					err, newBranch, oldBranch, rbErr)
			}
		}
		return err
	}
	i.Title = newTitle
	i.Branch = gw.GetBranchName()
	return nil
}

// RestoreFromArchive re-spawns an archived instance's agent after its worktree
// has been moved back into place (#1028), flipping it live. It marks the
// instance started + Lost so the Recover re-spawn path is eligible (the same
// re-spawn the #1108 Lost-restore loop drives), then Recover brings the agent
// session up and sets Running (markLive clears the OpRestoring fence). On a
// Recover failure the instance is dropped to a plain Lost (op cleared), so the
// daemon's Lost-restore loop keeps retrying — the worktree is already back in
// place, so the session self-heals rather than stranding as Archived with no
// tmux. The agent tab and any web tabs are restored; shell/process tabs were
// dropped at archive time (#1028), while web tabs — pure metadata with no tmux to
// re-spawn — ride back on the record and render again (#1809).
//
// liveness is set to Lost (so Recover's ==Lost gate accepts it) and OpRestoring
// fences the re-spawn window: the daemon poll skips an instance with an
// in-flight op, so it never probes the half-spawned session and marks it Lost
// out from under the restore. This replaces the old "park it in Lost purely to
// trigger the re-spawn loop" overload (#1195).
func (i *Instance) RestoreFromArchive() error {
	if err := i.ValidateRuntimeAction(RuntimeActionRestoreArchived); err != nil {
		return err
	}
	// Enter the restore fence through the chokepoint (#1195 Phase 2d): BeginRestore
	// is legal only from Archived and sets started=true + Lost + OpRestoring — the
	// exact head this used to write by hand, now enforcing I3 (a restore may begin
	// only from an archived session; no double-restore).
	if err := i.Transition(BeginRestore()); err != nil {
		return err
	}
	if err := i.currentBackend().Recover(i); err != nil {
		// Re-spawn failed: drop the fence to a plain Lost (started left true) so
		// the #1108 restore loop owns the retry against the now-restored worktree.
		_ = i.Transition(AbortRestoreToLost())
		return err
	}
	return nil
}

// CloseAttachOnly releases resources this instance opened to view or drive its
// session without destroying the session, worktree, or off-box workspace. Use it
// — never Kill — to discard a duplicate Instance built from disk that lost a
// race to the canonical tracked Instance (#867); see Backend.CloseAttachOnly.
func (i *Instance) CloseAttachOnly() error {
	return i.currentBackend().CloseAttachOnly(i)
}

// CheckAndHandleTrustPrompt checks for and dismisses the trust prompt for supported programs.
func (i *Instance) CheckAndHandleTrustPrompt() bool {
	return i.currentBackend().CheckAndHandleTrustPrompt(i)
}

// Capabilities returns the backing runtime's capability descriptor (#1592
// Phase 1). A nil backend (a not-yet-initialised instance) reports local
// full parity: the UI treats a backend-less instance as a capable local
// session, so returning the zero value instead would be an incoherent
// descriptor (local workspace but every capability off) and would regress
// e.g. the tab-management footer.
//
// The backend read is synchronized (#2096): the daemon's restore loops consult
// Capabilities().Recover BEFORE taking the instance's opLock, so it runs
// concurrently with a restore rebinding the backend.
func (i *Instance) Capabilities() Capabilities {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.capabilitiesLocked()
}

// capabilitiesLocked is Capabilities' already-locked half, for callers that
// already hold i.mu (LifecycleView resolves the recover capability inside its
// single read-locked snapshot). It must NOT take the lock itself: sync.RWMutex is
// not reentrant, so a nested RLock deadlocks against a queued writer — which on
// this path is exactly the restore goroutine the lock exists to exclude.
func (i *Instance) capabilitiesLocked() Capabilities {
	if i.backend == nil {
		return (&LocalBackend{}).Capabilities()
	}
	return i.backend.Capabilities()
}

// GetBackend returns the backend for the instance (mainly for testing).
func (i *Instance) GetBackend() Backend {
	return i.currentBackend()
}

// SetBackend sets the backend for the instance (mainly for testing). It writes
// under i.mu to match bindProvisionResult, so a test swapping the backend cannot
// race a reader on a background tick.
func (i *Instance) SetBackend(b Backend) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.backend = b
}
