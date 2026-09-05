package session

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
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
	return i.AgentServer().Kill(false)
}

// KillTrustingOwnLifecycleLock is Kill for the callers that hold this session's
// exclusive lifecycle lock for their ENTIRE call: daemon's KillSession (#3413),
// finishUserKill's tombstone retry (#3583), and reapDeadRoot's root-agent
// teardown (#3699). What licenses the trust is the per-session op-lock held
// unbroken across the teardown, a killsInFlight claim registered for its whole
// duration, and the instance keeping its (repo, title) map slot until the
// teardown settles — together those rule out a same-name replacement appearing
// mid-sweep. All three parts are load-bearing: the map slot alone is not
// enough, because the archived-name-reuse rename re-keys it, and killsInFlight
// is that path's own fence (Codex on #3700). Each call site carries its own
// version of the argument; do not add a fourth without writing one.
//
// Every other Kill caller (create-failure cleanup of a not-yet-registered
// instance is the standing example, and any future one) must keep calling Kill
// and get the strict #3309 default: none of them can make the same claim. See
// tmux.TmuxSession.CloseAndWaitForPaneExitTrustingOwnGeneration.
//
// This stays a second method (rather than exposing the bool on Instance
// itself) because Instance has no subtypes to lose an override on — the
// Go-embedding hazard Backend.Kill's doc explains does not apply here.
func (i *Instance) KillTrustingOwnLifecycleLock() error {
	return i.AgentServer().Kill(true)
}

// runtimeLiveBoundary lets the daemon settle predecessor-owned evidence at the
// exact lifecycle edge that exposes a replacement. The callback runs outside
// Instance.mu, immediately before ConfirmLive takes that lock and clears the
// restore fence. A pointer plus sync.Once keeps the boundary one-shot while the
// registration remains installed for the whole backend call.
type runtimeLiveBoundary struct {
	fn   func()
	once sync.Once
}

// Recover re-establishes a Lost instance's backing session (#1108).
func (i *Instance) Recover() error {
	return i.RecoverFencedWithLiveBoundary(nil)
}

// RecoverFencedWithLiveBoundary re-establishes a Lost instance's backing session
// under an OpRestoring fence, with an optional callback at the replacement's
// ConfirmLive edge.
//
// The callback exists for lifecycle facts that must be settled after a backend
// has successfully created the runtime but before the restore fence is dropped.
// It cannot veto recovery: settlement failures remain owed for retry, and a full
// disk must not turn a running replacement back into Lost.
//
// The fence is raised for the whole backend call so clients see the operation
// and hide Kill, and so the status poll — which skips any session with an op in
// flight — cannot read the teardown this call is performing as an independent
// death. ConfirmLive clears it on success; the failure path lowers it with
// ClearOp. It closes the asymmetry where the archived-restore path projects
// OpRestoring via BeginRestore but the lost-restore path did not.
//
// There is deliberately no unfenced variant (#3555). Both entry points — the
// manual Lost/Dead restore RPC and the daemon's automatic Lost-restore loop —
// come through here, because neither caller raises a lifecycle fence of its own
// and the automatic loop's only other protection is that it happens to run on
// the poll goroutine. A precondition that holds because of where its caller runs
// is not a precondition the next caller inherits.
func (i *Instance) RecoverFencedWithLiveBoundary(beforeLive func()) error {
	if err := i.BeginRecoverFence(); err != nil {
		return err
	}
	err := i.recoverUnderHeldFence(beforeLive)
	if err != nil {
		// ConfirmLive clears OpRestoring on success; lower it here on failure so
		// the row does not stay permanently busy. This method raised the fence, so
		// this method owns lowering it — the held variant below leaves that to the
		// caller that raised it, exactly as Respawn leaves it to EndLimitResume.
		i.EndRecoverFence()
	}
	return err
}

// RecoverHeldFencedWithLiveBoundary is RecoverFencedWithLiveBoundary for a caller
// that already holds the restore fence over a LONGER interval than the backend
// call — the manual Lost/Dead restore RPC, whose two network phases (a liveness
// probe and a pre-reap push bounded at 3m30s) run in front of it (#3586).
//
// It is a separate method rather than a re-entrant flag for the reason
// BeginLimitResume/Respawn are separate next door: the two legal orderings are
// made explicit instead of folded into one call that silently does different
// things depending on state it did not establish.
//
// The precondition is validated, not assumed, and it is validated through the
// SHARED ledger (RuntimeActionRecoverFenced) rather than a bare op comparison, so
// the pending-kill and startup-unknown vetoes that fence every other runtime entry
// point cover this one too. The caller owns the release: on failure the fence
// stays up for EndRecoverFence, and on success ConfirmLive clears it.
func (i *Instance) RecoverHeldFencedWithLiveBoundary(beforeLive func()) error {
	if err := i.ValidateRuntimeAction(RuntimeActionRecoverFenced); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	return i.recoverUnderHeldFence(beforeLive)
}

func (i *Instance) recoverUnderHeldFence(beforeLive func()) error {
	return i.withLiveBoundary(beforeLive, func() error { return i.currentBackend().Recover(i) })
}

// EndRecoverFence lowers the fence BeginRecoverFence raised, and reports whether
// it actually lowered one. Safe to defer unconditionally: it is a no-op once
// ConfirmLive has cleared the op on the success path, and it never disturbs an op
// some other owner raised.
//
// Deferring it is how the manual restore covers EVERY early return between the
// raise and the backend call — the probe-alive heal, the indeterminate refusal,
// the two durable-branch refusals, the failed pre-reap push — without each one
// having to remember. A missed path would leave the row permanently busy: the poll
// skips any session with an op in flight and every runtime action refuses it, which
// is strictly worse than the advertised-Kill bug the fence exists to fix.
//
// ClearOp is unconditionally legal, so the OpRestoring check is not about legality:
// it makes sure a kill or archive overlay that SUPERSEDED this fence is not cleared
// out from under its own owner — which is why the check and the clear share one
// critical section in clearOpIfHeld rather than being read-then-write.
func (i *Instance) EndRecoverFence() bool {
	return i.clearOpIfHeld(OpRestoring, "restore", nil)
}

// BeginRecoverFence validates the recover precondition and raises the restore
// fence in ONE critical section (#3555, the same defect #2997 fixed in
// BeginLimitResume next door).
//
// It is exported because the fence's owner is not always the backend call: the
// manual restore RPC raises it at the top of the operation so the fence is
// COEXTENSIVE with the claim that already refuses Kill at the admission gate
// (#3586), then continues through RecoverHeldFencedWithLiveBoundary. Raising and
// validating stay welded together here whichever caller does it, which is the
// property #3555 established and the reason there is still no way to raise this
// fence without validating, or to validate without raising.
//
// Splitting them is its own race, and the epoch guard cannot cover it: an
// observation landing in the gap is CURRENT, not stale, so nothing drops it, and
// tkMarkRestoring is keyed on the op axis alone — it would happily fence a
// liveness that had just been clobbered. Recovery would then run against a
// session that is no longer lost, which on a remote backend means replacing a
// sandbox that had just become reachable again and stranding its unpushed work.
//
// Under one lock there are only two arrivals: an observation applied before the
// critical section, which validation sees and refuses, and one blocked until
// after the raise, which the epoch guard drops as superseded. See
// lifecycleViewLocked, which documents this as the pattern.
func (i *Instance) BeginRecoverFence() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionRecoverLost); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	if err := i.transitionLocked(MarkRestoring()); err != nil {
		return fmt.Errorf("recover: raise restore fence: %w", err)
	}
	return nil
}

func (i *Instance) withLiveBoundary(beforeLive func(), run func() error) error {
	if beforeLive == nil {
		return run()
	}
	boundary := &runtimeLiveBoundary{fn: beforeLive}
	i.liveBoundaryMu.Lock()
	if i.liveBoundary != nil {
		i.liveBoundaryMu.Unlock()
		return fmt.Errorf("session %q already has a live boundary registered", i.Title)
	}
	i.liveBoundary = boundary
	i.liveBoundaryMu.Unlock()
	defer func() {
		i.liveBoundaryMu.Lock()
		if i.liveBoundary == boundary {
			i.liveBoundary = nil
		}
		i.liveBoundaryMu.Unlock()
	}()
	return run()
}

func (i *Instance) runLiveBoundary() {
	i.liveBoundaryMu.Lock()
	boundary := i.liveBoundary
	i.liveBoundaryMu.Unlock()
	if boundary != nil {
		boundary.once.Do(boundary.fn)
	}
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
	return i.clearOpIfHeld(OpRespawning, "limit resume", func() {
		// A preflighted account replacement also fences lazy sibling starts. If the
		// identity was never committed, releasing the resume owns and retires that
		// provisional plan; a committed replacement keeps it until delivery clears
		// pendingAccountSwap. This runs inside clearOpIfHeld's lock rather than
		// after it, so a second admission cannot raise the fence and install a new
		// plan in the gap and have this retire THAT one.
		if i.pendingAccountSwap == nil {
			i.accountSwapLaunch = nil
		}
	})
}

// clearOpIfHeld lowers the in-flight fence ONLY while it is still the op the caller
// raised, deciding and clearing in ONE critical section.
//
// The two halves cannot be split (Codex on #3597), and the reason is the very case
// the comparison exists for. tkBeginKill is allowed-from-always — a kill supersedes
// any in-flight op — so a BeginKill landing after a released read lock and before
// the write lock would install OpKilling, and the fence owner's release would then
// clear a teardown overlay it was supposed to leave alone: Kill and Archive back on
// offer for a session whose teardown is already running. Read-then-write made that
// window real; one write lock removes it.
//
// It is shared rather than written twice because both fence owners want the identical
// rule, and a second copy is the one that would keep the split.
// cleared, when non-nil, runs under the same write lock right after the fence
// comes down, for state a fence owner must retire indivisibly with it.
func (i *Instance) clearOpIfHeld(held InFlightOp, operation string, cleared func()) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != held {
		return false
	}
	if err := i.transitionLocked(ClearOp()); err != nil {
		log.WarningLog.Printf("%s: clearing the in-flight fence for %q: %v", operation, i.Title, err)
		return false
	}
	if cleared != nil {
		cleared()
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
	return i.RespawnWithLiveBoundary(nil)
}

// RespawnWithLiveBoundary is Respawn with the same pre-ConfirmLive callback as
// RecoverFencedWithLiveBoundary. Limit recovery uses it to retire facts owned by the
// exited, limit-blocked process before its replacement becomes visible.
func (i *Instance) RespawnWithLiveBoundary(beforeLive func()) error {
	if op := i.GetInFlightOp(); op != OpRespawning {
		return fmt.Errorf("respawn of %q requires the limit-resume fence (in-flight op is %s)", i.Title, opLabel(op))
	}
	return i.withLiveBoundary(beforeLive, func() error { return i.currentBackend().Respawn(i) })
}

// RespawnForAccountSwap starts the replacement identity in a fresh provider
// conversation. Local recovery normally resumes the prior conversation, which
// belongs to the previous account's separate home; Docker already provisions a
// fresh sandbox and launch through its ordinary Respawn implementation.
func (i *Instance) RespawnForAccountSwap() error {
	return i.RespawnForAccountSwapWithLiveBoundary(nil)
}

// RespawnForAccountSwapWithLiveBoundary is the fresh-conversation account
// replacement with the same pre-ConfirmLive callback used by ordinary respawn.
// The idle-evidence mechanism owns that boundary; account swapping only routes
// its distinct launch through it.
func (i *Instance) RespawnForAccountSwapWithLiveBoundary(beforeLive func()) error {
	if op := i.GetInFlightOp(); op != OpRespawning {
		return fmt.Errorf("account respawn of %q requires the limit-resume fence (in-flight op is %s)", i.Title, opLabel(op))
	}
	if err := i.withLiveBoundary(beforeLive, func() error {
		backend := i.currentBackend()
		if local, ok := backend.(*LocalBackend); ok {
			return local.respawnFresh(i)
		}
		return backend.Respawn(i)
	}); err != nil {
		return err
	}
	return i.markAccountSwapReplacementPanesStarted()
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

// ClaimWorktreeRelocationForRetry atomically consumes any durable recovery record
// and returns the point-in-time directory claim later archive steps must
// revalidate. Resolution never leaves a settled record behind for another reader
// to reinterpret.
func (i *Instance) ClaimWorktreeRelocationForRetry() (git.RelocationClaim, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.gitWorktree == nil {
		return git.RelocationClaim{}, fmt.Errorf("cannot resolve worktree relocation for %q: instance has no worktree", i.Title)
	}
	return i.gitWorktree.ClaimRelocationSource()
}

// PreserveWorktreeRelocationClaimForRetry returns ownership of a consumed
// recovery claim when an earlier archive gate aborts before the worktree use
// boundary. Claims made from record-free state are no-ops.
func (i *Instance) PreserveWorktreeRelocationClaimForRetry(claim git.RelocationClaim) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw != nil {
		gw.PreserveRelocationClaim(claim)
	}
}

// PreserveWorktreeRelocationClaimAsUnresolved fences a resolved archive when a
// later read-only gate cannot answer. Unlike the ordinary abort helper, it also
// materializes record-free claims so kill cannot read absence as permission.
func (i *Instance) PreserveWorktreeRelocationClaimAsUnresolved(claim git.RelocationClaim) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw != nil {
		gw.PreserveRelocationClaimAsUnresolved(claim)
	}
}

// PrepareWorktreeRelocationClaimForCleanup persists a resolved archived-path
// identity as a cleanup-only obligation. It is the non-relocating completion
// path used when the origin repo is gone.
func (i *Instance) PrepareWorktreeRelocationClaimForCleanup(claim git.RelocationClaim) error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot prepare worktree cleanup for %q: instance has no worktree", i.Title)
	}
	return gw.PrepareRelocationClaimForCleanup(claim)
}

// SetRepoGoneFinalizationCheckpoint installs the daemon's durable writer for
// the post-content, pre-root cleanup boundary. Kill runs backend teardown outside
// i.mu, so the callback may safely snapshot this instance for persistence.
func (i *Instance) SetRepoGoneFinalizationCheckpoint(checkpoint func() error) func() {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return func() {}
	}
	return gw.SetRepoGoneFinalizationCheckpoint(checkpoint)
}

// WorktreeRelocationRecovery snapshots the durable relocation lifecycle — a
// recovery record or an active consumed claim, projected to record form — for
// this instance's worktree. Deliberately not gated on started: destruction
// admission runs against archived instances, whose runtime is inert.
func (i *Instance) WorktreeRelocationRecovery() (git.RelocationRecovery, bool) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return git.RelocationRecovery{}, false
	}
	return gw.GetRelocationRecovery()
}

// SettleStalledWorktreeRelocationForAbsentPath clears an identity-unknown
// stalled relocation record once its pathname conclusively answers ENOENT.
// See GitWorktree.SettleStalledRelocationForAbsentPath.
func (i *Instance) SettleStalledWorktreeRelocationForAbsentPath() error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot settle stalled worktree relocation for %q: instance has no worktree", i.Title)
	}
	return gw.SettleStalledRelocationForAbsentPath()
}

// RestoreStalledWorktreeFenceAfterFailedSettle re-materializes the reclaimable
// stalled fence after a failed settle. See
// GitWorktree.RestoreStalledFenceAfterFailedSettle.
func (i *Instance) RestoreStalledWorktreeFenceAfterFailedSettle() error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot restore stalled worktree fence for %q: instance has no worktree", i.Title)
	}
	return gw.RestoreStalledFenceAfterFailedSettle()
}

// SettleWorktreeRelocationClaim revalidates a consumed recovery claim and
// releases its ownership without relocating anything. The kill admission
// producer uses it when a reclaimed stalled record turns out to need no
// cleanup authority because the origin repository answered present.
func (i *Instance) SettleWorktreeRelocationClaim(claim git.RelocationClaim) error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot settle worktree relocation for %q: instance has no worktree", i.Title)
	}
	return gw.SettleRelocationClaim(claim)
}

// ValidateWorktreeDestructionAdmission is the pre-commit guard. The local
// backend separately consumes and revalidates the exact cleanup identity before
// pane teardown; it must not repeat the origin-path admission after the durable
// kill has committed.
func (i *Instance) ValidateWorktreeDestructionAdmission() error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return nil
	}
	path, recovery, unresolved := gw.RelocationSnapshot()
	if !unresolved || recovery.State == git.RelocationRecoveryCleanupStalled {
		return nil
	}
	if recovery.State == git.RelocationRecoveryCleanupReady ||
		recovery.State == git.RelocationRecoveryCleanupFinalizing {
		if err := gw.ValidateRelocationCleanupAdmission(); err != nil {
			return fmt.Errorf("cleanup worktree in state %s at %s is not admissible for repo-gone kill: %w", recovery.State, path, err)
		}
		return nil
	}
	return fmt.Errorf(
		"worktree recovery state %s is unresolved at %s; retry archive or restore before destructive cleanup",
		recovery.State, path,
	)
}

// ArchiveTeardownWithHook is ArchiveTeardown with one additional operator
// callback at the only safe cleanup point: every pane has been confirmed dead,
// but the worktree still occupies its live path. A callback failure is returned
// separately from the relocation result and never prevents the move.
func (i *Instance) ArchiveTeardownWithHook(dest string, beforeMove func() error) (hookErr, archiveErr error) {
	claim, err := i.ClaimWorktreeRelocationForRetry()
	if err != nil {
		return nil, err
	}
	// false: neither this exported wrapper nor ArchiveTeardown has a production
	// caller that holds the daemon's archive op-lock (#3413) — only
	// daemon/archive.go's own direct ArchiveTeardownWithClaim call does, and it
	// passes true explicitly. See closeTabForDestructiveTeardown.
	return i.ArchiveTeardownWithClaim(dest, claim, beforeMove, false)
}

// ArchiveTeardownWithClaim carries the source claim obtained before teardown to
// the hook and move use boundaries. Each boundary revalidates it independently.
//
// trustLiveGeneration must come from the caller — only daemon/archive.go's
// archiveTeardown, which holds this session's exclusive lifecycle lock for the
// whole call, may pass true (#3413); see closeTabForDestructiveTeardown.
func (i *Instance) ArchiveTeardownWithClaim(dest string, claim git.RelocationClaim, beforeMove func() error, trustLiveGeneration bool) (hookErr, archiveErr error) {
	claimHandled := false
	mode := teardownArchive{
		dest: dest, claim: &claim, claimHandled: &claimHandled,
		beforeMove: beforeMove, hookErr: &hookErr, trustLiveGeneration: trustLiveGeneration,
	}
	archiveErr = i.teardownTabs(mode)
	if !claimHandled {
		i.PreserveWorktreeRelocationClaimForRetry(claim)
	}
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
	claim, err := i.ClaimWorktreeRelocationForRetry()
	if err != nil {
		return err
	}
	return i.RestoreArchivedWorktreeWithClaim(dest, claim)
}

// RestoreArchivedWorktreeWithClaim carries recovery ownership obtained before
// restore admission through to the relocation boundary, avoiding a second
// reader between source resolution and use.
func (i *Instance) RestoreArchivedWorktreeWithClaim(dest string, claim git.RelocationClaim) error {
	return i.restoreArchivedWorktree(dest, claim, RuntimeActionRestoreArchived)
}

// RestoreArchivedWorktreeHeldFencedWithClaim is the move for a caller that already
// holds the restore fence (BeginArchivedRestoreFence). The daemon's local archived
// route raises it before this, so the fence covers the relocate rather than
// starting after it (#3596), and re-validating RuntimeActionRestoreArchived here
// would refuse the operation's own fence.
func (i *Instance) RestoreArchivedWorktreeHeldFencedWithClaim(dest string, claim git.RelocationClaim) error {
	return i.restoreArchivedWorktree(dest, claim, RuntimeActionRestoreArchivedFenced)
}

func (i *Instance) restoreArchivedWorktree(dest string, claim git.RelocationClaim, action RuntimeAction) error {
	i.mu.RLock()
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(action); err != nil {
		i.mu.RUnlock()
		i.PreserveWorktreeRelocationClaimForRetry(claim)
		return err
	}
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot restore %q: instance has no worktree", i.Title)
	}
	return gw.RestoreWorktreeToWithClaim(dest, claim)
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
	// Enter the restore fence through the chokepoint (#1195 Phase 2d): BeginRestore
	// is legal only from Archived and sets started=true + Lost + OpRestoring — the
	// exact head this used to write by hand, now enforcing I3 (a restore may begin
	// only from an archived session; no double-restore).
	return i.restoreFromArchive(RuntimeActionRestoreArchived, BeginRestore())
}

// RestoreFromArchiveHeldFenced is RestoreFromArchive for a caller that already
// holds the restore fence over the worktree relocate in front of it (#3596).
//
// It differs in the two places the held fence shows: the precondition is
// RuntimeActionRestoreArchivedFenced (OpRestoring rather than OpNone) and the edge
// is BeginRestoreUnderHeldFence rather than BeginRestore, whose `op == OpNone`
// guard is deliberately left strict — that guard is what makes a double-restore
// impossible, and widening it would admit any restore in flight rather than this
// operation's own fence.
//
// Everything after is identical, including the failure edge: AbortRestoreToLost
// drops {LiveLost, OpRestoring} to a plain Lost, so a re-spawn that fails still
// hands the row to the #1108 loop against the now-restored worktree, and the
// caller's own deferred release then finds no fence to lower.
func (i *Instance) RestoreFromArchiveHeldFenced() error {
	return i.restoreFromArchive(RuntimeActionRestoreArchivedFenced, BeginRestoreUnderHeldFence())
}

func (i *Instance) restoreFromArchive(action RuntimeAction, enter TransitionEvent) error {
	if err := i.ValidateRuntimeAction(action); err != nil {
		return err
	}
	if err := i.Transition(enter); err != nil {
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

// BeginArchivedRestoreFence validates the archived-restore precondition and raises
// the restore fence in ONE critical section, for a caller that owns it across more
// than the re-spawn — the daemon's local archived route, whose worktree relocate
// runs in front of RestoreFromArchive (#3596). It is the sibling of
// BeginRecoverFence (#3555/#3586), and welded together for the same reason: a
// validation that released the lock before the raise could fence a liveness an
// observation had just moved.
//
// The fence is MarkRestoring — the op axis alone. Leaving liveness at LiveArchived
// is the whole design (#3596 triage): the snapshot reconcile keys its Archived->live
// REBUILD on seeing that exact transition, so flipping liveness early would make it
// see live->live and skip the rebuild, stranding the row live-but-not-started
// (#1203). The visible effect is ShownArchived, which yields the row to the live
// Instances section the moment a restore starts — the eager feedback #1210 asked
// for, now beginning when the restore actually begins.
func (i *Instance) BeginArchivedRestoreFence() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionRestoreArchived); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := i.transitionLocked(MarkRestoring()); err != nil {
		return fmt.Errorf("restore: raise restore fence: %w", err)
	}
	return nil
}

// EndArchivedRestoreFence lowers the fence BeginArchivedRestoreFence raised, and
// reports whether it actually lowered one. Safe to defer unconditionally: it is a
// no-op once the re-spawn has moved the row off OpRestoring, and it never disturbs
// an op some other owner raised (see clearOpIfHeld).
func (i *Instance) EndArchivedRestoreFence() bool {
	return i.clearOpIfHeld(OpRestoring, "archive restore", nil)
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
