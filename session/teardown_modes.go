package session

// The teardownMode implementations teardownTabs drives: teardownKill (full
// kill — kill-session on every tab, delete the worktree, drop the record),
// teardownReleasePTY (surrender this instance's attach PTYs only, for the
// duplicate-instance discard), and teardownArchive (kill the tabs, relocate
// the worktree, keep the record restorable). The dispatch point stays in
// teardown.go — the teardownMode interface, the teardownState taxonomy, the
// shared closeTabForDestructiveTeardown safety gate, and the teardownTabs
// skeleton; this file is the per-mode behavior the core invokes.
// Split out of teardown.go as a file-length decomposition (#1145) — same
// package, so nothing here is a new boundary.

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// teardownKill tears the session fully down: kill-session on every tab (waiting
// for each pane to exit, #802), DELETE the worktree, clear every tmux ref and
// the worktree pointer. started is cleared. Best-effort — a stuck tmux or a
// failed worktree cleanup only logs, so the caller can still drop the record
// (#478).
//
// "Best-effort" covers failures tmux and git ANSWERED with. It does NOT cover a
// tripped deadline (#1917): a timeout leaves the pane's liveness or the
// worktree's removal genuinely unknown, and this mode reports those so the caller
// keeps the record and retries rather than destroying a workspace on a guess.
type teardownKill struct {
	cleanupClaim *git.RelocationClaim
	claimHandled *bool
	// recheckOrigin re-establishes an archived worktree's origin immediately
	// before ordinary cleanup — after every pane wait, at the deletion
	// boundary itself (#3278 review). Nil for everything but an archived,
	// record-free, local worktree.
	recheckOrigin func() error
	// recheckMissing re-probes a worktree-missing LIVE row's recorded path at
	// the deletion boundary (#5102). Nil for every other row.
	recheckMissing func() error
	// trustLiveGeneration is set only by prepareKillTeardown when its caller
	// holds this session's exclusive lifecycle lock (#3413); see closeTab.
	trustLiveGeneration bool
	// keepWorkspace is set for a startup-state-unknown record: its persisted
	// worktree identity was never proven to be af's, so kill drops the record
	// and its tmux binding but must not destroy the workspace — the same
	// keep-unknown rule that fences `git branch -D` off a nil
	// BranchCreatedByUs (#5503 review).
	keepWorkspace bool
}

// prepareKillTeardown consumes a cleanup-ready record only after the daemon's
// pre-tombstone admission check has validated it. The returned cleanup gives
// ownership back if pane teardown aborts before handleWorktree can revalidate
// the claim at the deletion boundary.
//
// trustLiveGeneration is threaded straight from the caller into every
// teardownKill this returns (#3413); see closeTabForDestructiveTeardown for
// which callers may pass true.
func (i *Instance) prepareKillTeardown(trustLiveGeneration bool) (teardownKill, func(), error) {
	noop := func() {}
	i.mu.RLock()
	gw := i.gitWorktree
	archived := i.liveness == LiveArchived
	startupUnknown := i.startupStateUnknown
	worktreeMissing := i.worktreeMissing
	i.mu.RUnlock()
	if gw == nil {
		return teardownKill{trustLiveGeneration: trustLiveGeneration}, noop, nil
	}
	if startupUnknown {
		// The record is a quarantine: af never proved the workspace was its
		// own, so the kill may only drop the record and its tmux binding.
		return teardownKill{trustLiveGeneration: trustLiveGeneration, keepWorkspace: true}, noop, nil
	}
	if gw.CleanupRetryPending() {
		return teardownKill{}, noop, fmt.Errorf("%w: kill cleanup previously stalled; refusing to repeat pane teardown or enter an unbounded delete in this daemon process — restart the daemon to retry from the persisted record", ErrWorkspaceStateUnknown)
	}
	// Same discharge as kill admission (#5102): it may have run before the
	// tombstone, but the path's absence is re-proven here at teardown.
	gw.SettleAbsentIdentityUnknownStall()
	_, recovery, unresolved := gw.RelocationSnapshot()
	if !unresolved {
		// Record-free archived teardown re-establishes the origin at the point
		// of no return (#3278 review). Admission proved the origin PRESENT
		// before the tombstone, but a repository deleted afterwards would send
		// the archived worktree through ordinary git cleanup, whose answered
		// failures against the missing origin settle the kill and let the
		// record delete orphan the archive — the same interval restore's
		// authoritative pre-move recheck exists for. The check is carried into
		// handleWorktree so it runs at the deletion boundary itself, after
		// every pane wait, not merely at teardown entry. Reporting unknown
		// keeps the tombstoned record as the retry handle; killing again
		// re-enters the daemon's admission producer, which authorizes
		// identity-qualified cleanup against the now-gone origin.
		if archived && !gw.IsExternalWorktree() && gw.GetRepoPath() != "" &&
			i.Capabilities().Workspace == WorkspaceLocalWorktree {
			return teardownKill{trustLiveGeneration: trustLiveGeneration, recheckOrigin: func() error {
				beforeKillTeardownOriginRecheck()
				// An already-absent archived directory has nothing to orphan,
				// and ordinary cleanup settles the missing path and clears the
				// stale record — recheck only while the directory still
				// exists, or a gone origin would strand an empty record
				// forever. Bounded: this runs under the session operation
				// lock, so a stalled mount must time out — and a timeout must
				// STOP here (#3278 review): falling through would reach
				// Cleanup's own unbounded stat of the same stalled path.
				info, statErr := git.BoundedLstat(gw.GetWorktreePath())
				if statErr != nil {
					if errors.Is(statErr, os.ErrNotExist) {
						return nil
					}
					return fmt.Errorf(
						"%w: the archived worktree's state at %s could not be established (%v) — kill the session again once the path answers",
						ErrWorkspaceStateUnknown, gw.GetWorktreePath(), statErr,
					)
				}
				// A genuine archive is a real directory, never a symlink —
				// and cleanup's own first probe FOLLOWS links, so a symlink
				// occupant pointing into a stalled mount would wedge the kill
				// under its operation lock (#3278 review). Refuse it here.
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf(
						"%w: the occupant at %s is a symlink, not the archived directory — refusing to enter cleanup through it",
						ErrWorkspaceStateUnknown, gw.GetWorktreePath(),
					)
				}
				if err := git.CheckRepoPresentForRelocation(gw.GetRepoPath()); err != nil {
					return fmt.Errorf(
						"%w: origin repo state for this archived worktree changed or could not be established after kill admission — kill the session again to re-establish cleanup authorization: %v",
						ErrWorkspaceStateUnknown, err,
					)
				}
				return nil
			}}, noop, nil
		}
		// A live row whose worktree af has seen gone (#5102) reaches here with
		// nothing fencing the path any more, and ordinary Cleanup ends in an
		// unregistered RemoveAll. A directory that appears there after that
		// proof is unverified, so the absence is re-proven at the deletion
		// boundary and only git's stale registration is reaped. Three facts
		// arm it, because each alone misses a row the others catch:
		//
		//   - worktreeMissing: the poll recorded the deletion. Durable, so it
		//     also covers a row reloaded by a restarted daemon.
		//   - AbsentStallSettled: this process discharged an identity-unknown
		//     stall on proven absence — at kill admission, or just above. Such
		//     a stall keeps the presence probe answering unknown, so the flag
		//     never reached the row, and the discharge leaves no record either.
		//     Admission shares this GitWorktree, so its discharge is visible
		//     here though the settle above then has nothing left to clear, and
		//     an occupant that appeared before teardown entry is refused rather
		//     than taken for the session's worktree.
		//   - an ENOENT at entry: the worktree was deleted outside af and the
		//     poll has not run yet, or a restarted daemon lost the discharge
		//     proof. Only a conclusive ENOENT arms; an lstat that cannot answer
		//     leaves the row to ordinary cleanup, as before.
		//
		// An occupant refuses; the tombstone stays the retry handle, and once
		// the user clears the path a further kill passes admission and settles.
		// Archived rows keep recheckOrigin above: its registered-only cleanup
		// already refuses any occupant that is not a checkout of this session's
		// branch.
		if !gw.IsExternalWorktree() && gw.GetRepoPath() != "" &&
			i.Capabilities().Workspace == WorkspaceLocalWorktree &&
			(worktreeMissing || gw.AbsentStallSettled() || worktreeAbsentAtEntry(gw.GetWorktreePath())) {
			return teardownKill{trustLiveGeneration: trustLiveGeneration, recheckMissing: func() error {
				beforeKillTeardownOriginRecheck()
				return recheckMissingWorktreeAbsent(gw.GetWorktreePath())
			}}, noop, nil
		}
		return teardownKill{trustLiveGeneration: trustLiveGeneration}, noop, nil
	}
	if recovery.State != git.RelocationRecoveryCleanupReady &&
		recovery.State != git.RelocationRecoveryCleanupFinalizing {
		return teardownKill{}, noop, fmt.Errorf(
			"%w: kill cleanup has unresolved worktree recovery state %s",
			ErrWorkspaceStateUnknown, recovery.State,
		)
	}
	claim, err := gw.ClaimRelocationSource()
	if err != nil {
		return teardownKill{}, noop, fmt.Errorf("%w: cannot claim cleanup-ready worktree: %v", ErrWorkspaceStateUnknown, err)
	}
	handled := false
	mode := teardownKill{cleanupClaim: &claim, claimHandled: &handled, trustLiveGeneration: trustLiveGeneration}
	return mode, func() {
		if !handled {
			gw.PreserveRelocationClaim(claim)
		}
	}, nil
}

// closeTab waits for the pane to exit before the worktree delete in
// handleWorktree: a process still flushing state mid-shutdown races git's
// recursive delete and leaves a half-deleted directory ("Directory not empty",
// #802). Best-effort for anything tmux ANSWERED with; an unknown stops the core.
var killCloseTab = func(ts *tmux.TmuxSession, title, tabName string) (teardownState, bool, error) {
	return closeTabForDestructiveTeardown(ts, "kill", title, tabName, false)
}

func (m teardownKill) closeTab(ts *tmux.TmuxSession, title, tabName string) (teardownState, bool, error) {
	// Bypasses the stubbable var above when trusted (#3413): killCloseTab exists
	// for tests to stub the ORDINARY close, and no existing test needs to stub
	// the trusted variant — production always wants the real behavior here.
	if m.trustLiveGeneration {
		return closeTabForDestructiveTeardown(ts, "kill", title, tabName, true)
	}
	return killCloseTab(ts, title, tabName)
}

func (m teardownKill) handleWorktree(gw *git.GitWorktree, title string) (teardownState, error) {
	if gw == nil {
		return stateKnown, nil
	}
	if m.keepWorkspace {
		// stateKnown, not stateUnknown: the teardown SUCCEEDED — the unknown
		// verdict is what keeps the record, and this workspace is deliberately
		// left, not unresolvable. The tree and its branch stay on disk for the
		// owner to inspect; only the af record and tmux binding are dropped.
		log.InfoLog.Printf("kill %q: leaving unproven workspace %q in place (startup-state-unknown record)", title, gw.GetWorktreePath())
		return stateKnown, nil
	}
	var cleanupState git.CleanupState
	var err error
	if m.cleanupClaim != nil {
		cleanupState, err = gw.CleanupClaimedRepoGone(*m.cleanupClaim)
		*m.claimHandled = true
	} else {
		if m.recheckMissing != nil {
			if recheckErr := m.recheckMissing(); recheckErr != nil {
				return stateUnknown, recheckErr
			}
			// Proven absent: nothing of the session's is on disk to delete, and
			// registered-only cleanup can never RemoveAll a directory that
			// appears while git runs (#5102).
			cleanupState, err = gw.CleanupRegisteredOnly()
		} else if m.recheckOrigin != nil {
			if recheckErr := m.recheckOrigin(); recheckErr != nil {
				return stateUnknown, recheckErr
			}
			// Registered-only: the archived record-free kill may delete only
			// what git's registration vouches for — never the unregistered
			// RemoveAll fallback, which cannot distinguish the archive from a
			// directory that replaced it (#3278 review).
			cleanupState, err = gw.CleanupRegisteredOnly()
		} else {
			cleanupState, err = gw.Cleanup()
		}
		// The pre-check above narrows the race; this is what closes it (#3278
		// review). Cleanup waits for hooks and reaps writers before its
		// destructive git command, so an origin deleted anywhere in that
		// interval still turns every git failure into an answered, settled
		// error — with the archived directory untouched. No probe placement
		// can outrun that; the sound invariant is the postcondition: an
		// archived kill must never report ordinary cleanup settled while the
		// archived directory still occupies its path, because the caller's
		// next step deletes the row that is the directory's only handle.
		if (m.recheckOrigin != nil || m.recheckMissing != nil) && cleanupState == git.CleanupSettled {
			if settleErr := ArchivedCleanupSettled(gw.GetWorktreePath()); settleErr != nil {
				retained := fmt.Errorf(
					"%w: ordinary cleanup reported settled but %v — kill the session again to re-establish cleanup authorization",
					ErrWorkspaceStateUnknown, settleErr,
				)
				if err != nil {
					retained = errors.Join(retained, err)
				}
				return stateUnknown, retained
			}
		}
	}
	// Same rule as closeTab, one layer down (#1917), and read off Cleanup's own
	// reported STATE rather than re-derived from its error here. A git command that
	// ANSWERED with a failure leaves Cleanup's #802/#726 decision tree in charge and
	// stays best-effort, so a stuck-but-diagnosed worktree never blocks dropping the
	// record. A TIMEOUT is different in kind: git was SIGKILLed mid-delete, so the
	// worktree directory and its registration may both still be there. Reporting it
	// keeps the record — and with it the user's handle on the session — so the
	// cleanup can be retried, instead of orphaning a registered worktree whose
	// session row we just deleted.
	if cleanupState != git.CleanupSettled {
		return stateUnknown, fmt.Errorf("kill %q: git worktree cleanup: %w", title, err)
	}
	if err != nil {
		log.WarningLog.Printf("kill %q: git worktree cleanup failed: %v", title, err)
	}
	return stateKnown, nil
}

func (teardownKill) clearsStarted() bool { return true }

// reapsPendingTabCleanup: kill deletes the record, which is the cleanup handle's
// only home. A pending session not killed here is untracked forever.
func (teardownKill) reapsPendingTabCleanup() bool { return true }

func (teardownKill) finalize(i *Instance, closed []closedTab, gw *git.GitWorktree) {
	clearClosedTmuxRefs(i, closed)
	if i.gitWorktree == gw {
		if i.gitWorktree != nil {
			i.gitWorktree = nil
			i.touchLocked()
		}
	}
}

// teardownReleasePTY releases only this instance's hold on its tmux sessions —
// the attach PTYs and `tmux attach-session` children — WITHOUT running
// kill-session. The server-side tmux sessions and the worktree are left intact.
// Used to discard a duplicate Instance built from disk that lost a race to the
// canonical tracked one (#867/#1065): it must surrender every tab's PTY without
// tearing down the live sessions the canonical Instance shares. Per-tab close
// errors are collected and returned (not merely logged).
type teardownReleasePTY struct{}

// closeTab is always stateKnown: CloseAttachOnly runs no tmux command (it only
// releases what this object opened, and post-#1592-PR7 that is nothing), so there
// is no deadline to trip and nothing to be unknown about. This mode touches no
// worktree either way.
func (teardownReleasePTY) closeTab(ts *tmux.TmuxSession, _, tabName string) (teardownState, bool, error) {
	if err := ts.CloseAttachOnly(); err != nil {
		return stateKnown, false, fmt.Errorf("tab %q: %w", tabName, err)
	}
	return stateKnown, false, nil
}

func (teardownReleasePTY) handleWorktree(_ *git.GitWorktree, _ string) (teardownState, error) {
	return stateKnown, nil
}

func (teardownReleasePTY) clearsStarted() bool { return true }

// reapsPendingTabCleanup: false. This mode runs no kill-session, so it can never
// confirm a pending session dead — and it shares its tmux with the canonical
// instance, which still needs those handles.
func (teardownReleasePTY) reapsPendingTabCleanup() bool { return false }

func (teardownReleasePTY) finalize(i *Instance, closed []closedTab, _ *git.GitWorktree) {
	clearClosedTmuxRefs(i, closed)
}

// teardownArchive tears down every tab's tmux session and RELOCATES the worktree
// to dest (#1028) — the tmux half of Kill, but it preserves the record and MOVES
// the worktree instead of deleting it. Folding the move into the core (via
// handleWorktree, right after closeTab's pane-exit wait) is the whole point of
// Phase 2b: the #802 "wait for every pane to exit before touching the worktree"
// ordering becomes shared code instead of the duplicated prose it was when the
// move lived in a separate daemon step. It keeps the agent tab's tmux binding as
// a name-holder (a failed move / un-archive re-spawns it), keeps the
// metadata-only tabs (web and VS Code — nothing was torn down, #1809/#1817) and
// drops the shell/process tabs; started is left true (the OpArchiving fence, not the #990 started guard,
// owns the teardown window) so a failed move self-heals via the Lost-restore
// loop.
type teardownArchive struct {
	dest  string
	claim *git.RelocationClaim
	// claimHandled flips at the worktree boundary. If pane teardown returns
	// earlier, ArchiveTeardownWithClaim rematerializes any record this claim
	// consumed instead of leaving absence to authorize a later recovery.
	claimHandled *bool
	beforeMove   func() error
	hookErr      *error
	// trustLiveGeneration is set only by ArchiveTeardownWithClaim's caller when
	// it holds this session's exclusive lifecycle lock (#3413); see closeTab.
	trustLiveGeneration bool
	// worktreeGone selects the no-move route for a worktree deleted outside af
	// (#5102): the tabs tear down exactly as usual, but there is nothing to
	// relocate, so handleWorktree only re-confirms the absence. claim is nil.
	worktreeGone bool
	// worktreeAdopted is the other no-move route: an earlier archive's move
	// landed at the destination before the daemon could record it, and the
	// record was re-aimed there after git proved it (#5102). The bytes are
	// already where the archive puts them, so handleWorktree only re-confirms
	// the adopted identity — before the hook and again after it.
	worktreeAdopted bool
}

// closeTab waits for the pane to exit before handleWorktree relocates the
// worktree: a process still flushing state races the move otherwise (#802).
//
// It shares closeTabForDestructiveTeardown with kill rather than open-coding the
// same handling, because it needs the SAME guarantee: this path used to log every
// close error — timeouts included — and return nil, so a wedged tmux server led
// straight into moving a live agent's workspace out from under it. Archive is the
// default reap action, so that ran far more often than the kill path did (#1917
// review).
var archiveCloseTab = func(ts *tmux.TmuxSession, title, tabName string) (teardownState, bool, error) {
	return closeTabForDestructiveTeardown(ts, "archive", title, tabName, false)
}

func (m teardownArchive) closeTab(ts *tmux.TmuxSession, title, tabName string) (teardownState, bool, error) {
	// See teardownKill.closeTab: bypasses the stubbable var above when trusted.
	if m.trustLiveGeneration {
		return closeTabForDestructiveTeardown(ts, "archive", title, tabName, true)
	}
	return archiveCloseTab(ts, title, tabName)
}

// ErrHookTeardownUnconfirmed marks a pre-move hook callback failure that is a
// SAFETY fact rather than an operator-command failure: af could not establish
// that the hook's processes are gone. Archive treats it as a refusal to relocate
// (see handleWorktree), because every other beforeMove error is deliberately
// best-effort and moves the tree anyway.
var ErrHookTeardownUnconfirmed = errors.New("post-worktree hook teardown could not be confirmed")

func (m teardownArchive) handleWorktree(gw *git.GitWorktree, title string) (teardownState, error) {
	if gw == nil {
		return stateKnown, fmt.Errorf("cannot archive %q: instance has no worktree to relocate", title)
	}
	if m.claim != nil {
		// This is the first path-use boundary for the consumed claim. Only transfer
		// ownership at the move itself (below): an earlier return (including a
		// vanished worktree handle or an ErrHookTeardownUnconfirmed safety
		// refusal) must leave ArchiveTeardownWithClaim responsible for
		// rematerializing it. Setting claimHandled before those returns would let
		// the caller skip PreserveWorktreeRelocationClaimForRetry and leak the
		// in-memory activeRelocationClaim, wedging same-process retries.
		// Resolution was a point-in-time assertion made before pane teardown.
		// Revalidate it at the operator hook's use boundary; failure is a safety
		// error, not a best-effort hook error, so the move below must not run.
		if err := gw.RevalidateRelocationClaim(*m.claim); err != nil {
			return stateUnknown, fmt.Errorf("archive %q: worktree identity changed before on-archive hook: %w", title, err)
		}
	}
	if m.worktreeAdopted {
		// The adoption proof was about one directory, and editor and tmux
		// teardown have run since. Re-confirm it before the operator hook runs
		// in it, not only before commit (#5102).
		if state, err := reconfirmAdoptedForArchive(gw, title); err != nil {
			return state, err
		}
	}
	if m.beforeMove != nil {
		// Cleanup policy is deliberately best-effort. Record its failure for the
		// daemon to surface after the archive commits, then always relocate the
		// worktree so a broken operator hook cannot strand or lose the session.
		beforeMoveErr := m.beforeMove()
		// With ONE exception, and it is the same distinction the claim
		// revalidation above makes: "the operator's command failed" is best-effort,
		// but "a process from that command may still be running in this tree" is a
		// safety fact, and nothing downstream re-checks it. Relocating underneath
		// such a process lets it keep writing through its old cwd into the archived
		// tree (#3650 review). stateUnknown keeps the record recoverable, so this
		// refuses the move without stranding the session.
		if errors.Is(beforeMoveErr, ErrHookTeardownUnconfirmed) {
			return stateUnknown, fmt.Errorf("archive %q: %w", title, beforeMoveErr)
		}
		if m.hookErr != nil {
			*m.hookErr = beforeMoveErr
		}
	}
	if m.worktreeGone {
		return confirmGoneForArchive(gw, title)
	}
	if m.worktreeAdopted {
		// And again after the hook, which can run for minutes.
		return reconfirmAdoptedForArchive(gw, title)
	}
	// The move is now BOUNDED, which is the case this comment used to reserve:
	// "if the move is ever bounded, a tripped deadline must return stateUnknown
	// here". A git SIGKILLed mid-move or mid-repair may have moved the bytes,
	// written part of its registration, or neither — so finalize must not run,
	// because it clears the tmux refs and worktree pointer a recovery needs to
	// find intact, and the daemon would drop the record on a half-done relocate.
	//
	// Every other outcome keeps the pre-#1917 contract: an ordinary move failure
	// still reports stateKnown so the daemon's rollback to Lost (which requires
	// finalize to have run) fires as before.
	var moveErr error
	if m.claim != nil {
		// Ownership transfers only at the move itself: this is the point where the
		// claim is actually settled/consumed, so every earlier return leaves
		// claimHandled == false and ArchiveTeardownWithClaim re-preserves it.
		if m.claimHandled != nil {
			*m.claimHandled = true
		}
		moveErr = gw.ArchiveWorktreeWithClaim(m.dest, *m.claim)
	} else {
		moveErr = gw.ArchiveWorktree(m.dest)
	}
	if err := moveErr; err != nil {
		if errors.Is(err, git.ErrRelocateStateUnknown) {
			return stateUnknown, fmt.Errorf("archive %q: %w", title, err)
		}
		return stateKnown, err
	}
	return stateKnown, nil
}

func (teardownArchive) clearsStarted() bool { return false }

// reapsPendingTabCleanup: archive MOVES the worktree a pending process is still
// cwd'd in, so it must confirm that process dead first — the #802 ordering the
// live tabs already get.
func (teardownArchive) reapsPendingTabCleanup() bool { return true }

func (teardownArchive) finalize(i *Instance, _ []closedTab, gw *git.GitWorktree) {
	// Stamp the occupant nonce prune later requires (#5189). Best effort: a row
	// without one is judged by the stricter legacy predate rule instead, so a
	// failed stamp can only make prune refuse more, never delete wrongly.
	i.prune.nonce = ""
	if gw != nil {
		if nonce, err := git.StampArchiveNonce(gw.GetWorktreePath()); err != nil {
			log.WarningLog.Printf("archive %q: %v (prune will judge it by the legacy predate rule)", i.Title, err)
		} else {
			i.prune.nonce = nonce
		}
	}
	// Reduce to the tabs an un-archive can actually bring back: the agent tab
	// (i.Tabs[0]) and every web tab. The agent's tmux binding is KEPT (the
	// server-side session is gone, but the name-holder lets a rollback Recover
	// re-spawn it, and a successful archive persists it as an inert name-holder);
	// the shell/process tabs are dropped because this teardown just killed the
	// tmux sessions that WERE them — there is nothing left to restore (#1028).
	//
	// Metadata-only tabs are different in kind and are kept (#1809/#1817): they
	// have no tmux session and no process — a web tab IS its URL, and a VS Code tab
	// is a pointer to a daemon-managed editor no tab owns — so teardown destroys
	// nothing and the record round-trips through TabData exactly as it already does
	// across a daemon restart. Dropping them was collateral damage from the #1028
	// rule, written before these kinds existed, and it silently and permanently
	// erased them on the documented RESTORABLE reap path.
	//
	// The test is !HasTmux() rather than an enumeration of kinds: this filter read
	// "== TabKindWeb" while web was the only such kind, so TabKindVSCode was dropped
	// from both memory and the persisted record the moment it was added (#1817) —
	// restore could not bring back what was never written. Asking the KIND its own
	// property keeps the next such kind correct by default.
	//
	// THE ASSUMPTION THIS RESTS ON: keep exactly the tabs whose teardown destroyed
	// nothing, and today that set is exactly the tmux-less kinds — a tab either owns
	// a PTY this teardown just killed, or it owns nothing at all. HasTmux is
	// therefore the whole question, not a proxy for it. A future kind that holds NO
	// tmux but some other teardown-destroyed resource would break the equivalence
	// and be wrongly kept here; that kind does not exist, and inventing a second
	// predicate to guard against it is how one idea grows two names. If you are
	// adding such a kind, this filter is the site to split.
	//
	// The filter preserves relative order (the agent stays at 0, kept tabs keep
	// their sequence) rather than re-appending, because tab addressing — panes and
	// the 1-9 number keys — is position-sensitive today.
	//
	// gitWorktree is left in place (the move relocated it; it still points at
	// valid bytes) and started is left as the fence set it, so the refs are
	// deliberately NOT cleared here.
	if len(i.Tabs) == 0 {
		return
	}
	kept := make([]*Tab, 0, len(i.Tabs))
	kept = append(kept, i.Tabs[0])
	for _, tab := range i.Tabs[1:] {
		if !tab.Kind.HasTmux() {
			kept = append(kept, tab)
		}
	}
	if len(i.Tabs) != len(kept) {
		i.Tabs = kept
		i.touchLocked()
	}
}
