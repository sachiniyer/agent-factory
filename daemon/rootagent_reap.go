package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The root agent's self-heal teardown, split out of rootagent.go (which was at
// the file-length ceiling) because the #3699 trust decision and the exclusivity
// argument that licenses it need room to be written down where the call is.

// reapDeadRoot removes a Dead root instance so ensureRootAgent can re-create
// the title. On success it returns the state snapshotted under the operation
// lock from the exact record it deleted. The boolean reports whether
// the root was actually reaped; false means a concurrent operation owns or
// changed the title, or provider conversation discovery is still polling, so
// ensure should wait for a later tick instead of falling through to
// CreateSession. Mirrors KillSession's teardown but deliberately does NOT
// record rootKilledAt: this is the daemon healing itself, not a user decision.
func (m *Manager) reapDeadRoot(repoID string, inst *session.Instance) (reapedRootState, bool, error) {
	key := daemonInstanceKey(repoID, session.RootSessionTitle)
	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		// A user kill (or its finish pass) owns this title right now. Let that
		// operation decide whether the root is removed or left for the next tick.
		return reapedRootState{}, false, nil
	}
	defer opLock.Unlock()

	// Claim the title for the whole reap, in the SAME m.mu hold that clears the
	// guards — and claim it in killsInFlight, not merely by leaving the instance
	// in its map slot (Codex on #3700).
	//
	// The map slot is not sufficient, because one path re-keys it out from under
	// this call. An ARCHIVED root reaches here on purpose (ensureResolvedRoot
	// routes Archived to reap-and-recreate, since an archived root is inert and
	// must not be adopted as live), and for a LiveArchived instance
	// findArchivedOnlyCollisionLocked does NOT refuse — it SELECTS it, and
	// renameArchivedForReuseLocked then moves it to a new key so a colliding
	// create can take the name. The title need not even be "root": toTmuxName
	// DELETES whitespace while IsReservedTitle only trims it, so the perfectly
	// creatable title "ro ot" derives the identical tmux name and its create can
	// start a replacement under the exact name this sweep is sweeping. A trusted
	// sweep would then adopt that replacement's generation and reap it — #3309,
	// reopened, which is the one outcome worse than the bug being fixed.
	//
	// killsInFlight is that path's own fence (manager_create_titles.go refuses
	// the reuse outright while it is set), so registering closes it. Doing the
	// registration in the same critical section as the identity re-confirm is
	// what makes the two orderings both safe: a rename that commits first moves
	// m.instances[key] off inst, so the check below bails; a registration that
	// lands first makes the rename refuse and the ordinary availability check
	// then rejects the create against the instance still holding the slot.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	capturePending := m.pendingConversationCaptures[inst] > 0
	if killing || current != inst || capturePending {
		m.mu.Unlock()
		return reapedRootState{}, false, nil
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	// Released on EVERY return path, including the retaining ones: this is a heal
	// that either completes or leaves the record for the next tick, so a leaked
	// claim would refuse every later operation on the title for the daemon's
	// lifetime — the #1917 wedge, in the loop built to prevent it.
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	// Snapshot only after taking the same operation lock used by async
	// conversation capture and re-confirming that this is still the tracked
	// instance. Reading before the lock leaves a narrow loss window: capture can
	// commit a newly discovered ID after the read but before the reap acquires
	// the lock, and the reap would then delete the updated record while carrying
	// the stale snapshot.
	//
	// Both fields come out of ONE projection — the same one the record on disk is
	// written from — rather than two reads of the instance. Two reads could land
	// on either side of a capture commit and hand the replacement a conversation
	// and a roster that never coexisted; there is no reason to leave that open
	// when the whole record is available atomically.
	snapshot := inst.ToInstanceData()
	carried := reapedRootState{tabs: snapshot.Tabs, notice: snapshot.RootRecreateContext}
	if snapshot.AgentConversation != nil {
		carried.conversation = *snapshot.AgentConversation
	}

	// The reaped root's per-session editor is bound to the record this pass is
	// deleting, so it must not outlive it — a carried vscode tab re-resolves
	// against the REPLACEMENT record and lazily spawns a fresh editor on the next
	// proxy request (WebTabTarget), which is what makes the carry safe here. Stop
	// before runtime teardown and confirm again before record deletion, mirroring
	// KillSession/finishUserKill and closing a proxy spawn that resolved the dead
	// root immediately before this pass took ownership. Either unknown result
	// retains the record for the next ensure pass.
	if err := m.stopVSCodeForInstance(key, inst.ID); err != nil {
		return reapedRootState{}, false, fmt.Errorf("reaping dead root for repo %s: VS Code editor teardown is not confirmed, retaining its record for a retry: %w", repoID, err)
	}

	// Best-effort by design (#478): tmux is already gone and an in-place
	// worktree's Cleanup is a no-op, so failures Kill can ANSWER for only log
	// inside Kill and never surface here.
	//
	// An error that does reach us therefore means the teardown could not complete
	// SAFELY — tmux never confirmed the pane dead, or a worktree removal was cut off
	// mid-delete — so the workspace is still there. Deleting the record would orphan
	// it and leave nothing pointing at it. Keep the record; this loop runs every
	// tick, so it IS the retry (#1917: found by auditing every record delete against
	// the invariant, not reported).
	//
	// Trusting (#3699) — and this is the call site #3583 deliberately left strict
	// pending the argument below, because the root is the ONE title whose ensure
	// loop re-creates the same name immediately, which is what made #3309's guard
	// look most load-bearing right here. Left strict it cost the root its only
	// path back: a vanished session reaches the sweep with NO captured
	// predecessor, so the generation cohort is EMPTY and markedOrphanProcesses
	// refuses every marked survivor — including one carrying this session's OWN
	// generation, which `claude daemon run` leaves behind as a matter of course
	// (it inherits AF_SESSION/AF_HOME/AF_SESSION_GEN from the pane and outlives
	// it). The blind branch signals nothing by design, so the survivor never
	// died, every retry refused identically, the record was never deleted, and
	// this loop could never re-create the title: a permanently dead root pinned
	// at the 5m backoff cap.
	//
	// The claim the trust needs is narrow — no same-name replacement can appear
	// between the start and the end of THIS teardown — and a foreign af HOME is
	// excluded by markedOrphanProcesses BEFORE the generation check runs, so
	// "replacement" means one THIS daemon minted. There are two ways it could:
	//
	//   - A CREATE taking the name. Every create funnels through reserveCreate ->
	//     findTitleConflictLocked, which refuses a title colliding with a LIVE
	//     m.instances entry under m.mu. This pass re-confirmed m.instances[key] ==
	//     inst above and leaves inst in that slot until after deleteSessionRecord
	//     succeeds; the durable row outlives the teardown too, so a refreshLocked
	//     rebuild cannot free the slot either — it preserves the same *Instance
	//     pointer for a key it already holds. The ONE path that re-keys the slot
	//     mid-teardown is the archived-name-reuse rename, and the killsInFlight
	//     claim taken above is what fences it; see the note there, and do not
	//     weaken that claim back to "the map slot is enough".
	//   - A RESTORE/RESPAWN of this instance — Recover, limit-resume, handoff —
	//     each of which can re-establish the runtime under the same tmux name with
	//     a NEW generation. Every one of them takes m.opLockFor(key), which this
	//     pass holds (TryLock, above) for its entire body. The automatic
	//     Lost-restore loop additionally skips the reserved root title outright,
	//     and runs on this same poll goroutine after EnsureRootAgents.
	//
	// That is the same guarantee KillSession and finishUserKill make, and it is
	// made the same way: the op-lock held unbroken across the teardown, plus a
	// killsInFlight claim registered for its duration. Theirs additionally has to
	// OUTLIVE one op-lock hold (finishUserKill retries a tombstone across ticks);
	// this one does not — it settles inside this call or leaves the record for the
	// next tick, which re-derives the whole argument from scratch.
	teardownErr := inst.KillTrustingOwnLifecycleLock()
	if err := m.stopVSCodeForInstance(key, inst.ID); err != nil {
		return reapedRootState{}, false, fmt.Errorf("reaping dead root for repo %s: VS Code editor teardown is not confirmed after runtime teardown, retaining its record for a retry: %w", repoID, err)
	}
	// Through the one choke point (#1917): it refuses while the teardown's outcome
	// is unknown. This site was still log-and-delete after two audits I called
	// exhaustive — which is the argument for there being exactly one place to call.
	deleted, err := m.deleteSessionRecord(repoID, session.RootSessionTitle, inst.ID, teardownErr)
	if err != nil {
		// Return the ERROR, not (false, nil) (#1917 round 8). "No, but fine" is
		// absence-of-error wearing a different hat: the caller reads it as "nothing to
		// reap" and skips rootEnsureFailed, so a persistent tmux/file-lock timeout
		// re-runs this whole bounded teardown on EVERY tick — occupying the single
		// status/restore poll loop and spamming warnings — instead of backing off.
		// A failure has to look like one for the retry cadence to see it.
		return reapedRootState{}, false, fmt.Errorf("reaping dead root for repo %s: %w", repoID, err)
	}
	if !deleted {
		log.InfoLog.Printf("dead root reap for repo %s skipped storage delete: current root record has a different instance identity", repoID)
		return reapedRootState{}, false, nil
	}
	m.mu.Lock()
	if m.instances[key] == inst {
		delete(m.instances, key)
		m.forgetSessionRuntimeStateLocked(repoID, inst)
	}
	m.mu.Unlock()
	return carried, true, nil
}
