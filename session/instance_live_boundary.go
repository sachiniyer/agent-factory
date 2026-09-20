package session

import (
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// runtimeLiveBoundary settles predecessor evidence before a backend-proven
// replacement becomes live; plain reattachments do not consume it.
type runtimeLiveBoundary struct {
	fn   func() error
	once sync.Once
	err  error
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
// An ordinary settlement failure cannot veto recovery: it remains owed for
// retry, and a full disk must not tear down a running replacement. There is one
// stricter boundary. When replacement closed an active task run and that close
// is not durable, the callback refuses ConfirmLive and the backend tears down
// the replacement. Otherwise a daemon restart could reattach it, reload the run
// as active, and let its idle edge execute the predecessor's on_complete. The
// process-local OpRestoring hold remains raised until that teardown is proved;
// an indeterminate teardown is never converted into a live replacement by a
// later settlement retry.
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
func (i *Instance) RecoverFencedWithLiveBoundary(beforeLive func() error) error {
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
func (i *Instance) RecoverHeldFencedWithLiveBoundary(beforeLive func() error) error {
	if err := i.ValidateRuntimeAction(RuntimeActionRecoverFenced); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	return i.recoverUnderHeldFence(beforeLive)
}

func (i *Instance) recoverUnderHeldFence(beforeLive func() error) error {
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
// out from under its own owner. The interrupted-run settlement hold is checked in
// that same critical section: a deferred release cannot bypass the live-boundary
// veto while disk still describes the predecessor run as active.
func (i *Instance) EndRecoverFence() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.runtimeReplacementSettlementBlocked && i.inFlightOp == OpRestoring {
		return false
	}
	return i.clearOpIfHeldLocked(OpRestoring, "restore", nil)
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

func (i *Instance) withLiveBoundary(beforeLive func() error, run func() error) error {
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

func (i *Instance) runLiveBoundary() error {
	i.liveBoundaryMu.Lock()
	boundary := i.liveBoundary
	i.liveBoundaryMu.Unlock()
	if boundary != nil {
		boundary.once.Do(func() { boundary.err = boundary.fn() })
		return boundary.err
	}
	return nil
}

// Respawn re-establishes the instance's backing session in place without a
// liveness precondition — the guard-free core of Recover. The usage-limit
// manual-retry (#1146, resumeFromLimitOutcome) uses it to re-spawn an agent that exited
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
	return i.clearOpIfHeldLocked(held, operation, cleared)
}

// clearOpIfHeldLocked is the already-locked half used by EndRecoverFence, whose
// interrupted-run settlement hold must be checked in the same critical section
// as the release it can veto. Caller holds i.mu.
func (i *Instance) clearOpIfHeldLocked(held InFlightOp, operation string, cleared func()) bool {
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
func (i *Instance) RespawnWithLiveBoundary(beforeLive func() error) error {
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
func (i *Instance) RespawnForAccountSwapWithLiveBoundary(beforeLive func() error) error {
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
