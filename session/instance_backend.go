package session

import (
	"fmt"
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
// side-effect free (#970).
func (i *Instance) Recover() error {
	return i.currentBackend().Recover(i)
}

// Respawn re-establishes the instance's backing session in place without a
// liveness precondition — the guard-free core of Recover. The usage-limit
// manual-retry (#1146, resumeFromLimit) uses it to re-spawn an agent that exited
// while blocked at a limit wall: that session is LiveLimitReached, which Recover's
// !Lost guard rejects, but the re-spawn mechanics are identical. The caller owns
// the precondition.
func (i *Instance) Respawn() error {
	return i.currentBackend().Respawn(i)
}

// SwapAgent replaces the running agent process with the instance's current
// program (#2013). Rewrite Instance.Program first (SwapAgentProgram); this only
// performs the runtime half, and the caller owns every precondition. Refuses on
// a backend whose workspace is off-box.
func (i *Instance) SwapAgent() error {
	if !i.Capabilities().Handoff {
		return ErrHandoffUnsupported
	}
	return i.currentBackend().SwapAgent(i)
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
	return i.teardownTabs(teardownArchive{dest: dest})
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
	i.noteStateChangeLocked(lv, op, resetAt)
}

// RestoreArchivedWorktree moves this instance's archived worktree back to dest
// and re-registers it against the origin repo (#1028). Surfaces git.ErrRepoGone
// when the repo has been deleted so the caller can leave the archive intact.
func (i *Instance) RestoreArchivedWorktree(dest string) error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot restore %q: instance has no worktree", i.Title)
	}
	return gw.RestoreWorktreeTo(dest)
}

// RenameArchived atomically relocates an archived instance's worktree to dest (a
// new title-keyed archive dir) and updates its Title, so a fresh session can reuse
// the archived session's name (feat: reuse archived name). Both mutations happen
// under i.mu so a concurrent Snapshot/ToInstanceData never observes a torn state
// (new title paired with the old worktree path, or vice versa). Archived instances
// are inert — no async Start/Recover goroutine touches them — so holding i.mu
// across the git move only blocks a brief Snapshot RLock, never a live operation.
//
// The stable id, git branch, and worktree contents are preserved: only the on-disk
// directory + git's two-way registration move, and only the display title changes.
// On a relocation failure the worktree and title are left untouched and the error
// is surfaced, so the caller can abort the reuse without having half-renamed the
// archived session.
func (i *Instance) RenameArchived(newTitle, dest string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.liveness != LiveArchived {
		return fmt.Errorf("cannot rename session %q: it is not archived", i.Title)
	}
	gw := i.gitWorktree
	if gw == nil {
		return fmt.Errorf("cannot rename archived session %q: it has no worktree to relocate", i.Title)
	}
	// MoveWorktree relocates the bytes + repairs git's registration and, on success,
	// updates gw's stored worktree path — all under i.mu here, matching how
	// ToInstanceData reads the worktree path under i.mu.RLock.
	if err := gw.MoveWorktree(dest); err != nil {
		return err
	}
	i.Title = newTitle
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

// CloseAttachOnly releases the resources this instance opened to view or drive
// its session (a tmux attach PTY, a remote preview process) without destroying
// the session, worktree, or remote record. Use it — never Kill — to discard a
// duplicate Instance built from disk that lost a race to the canonical tracked
// Instance (#867); see Backend.CloseAttachOnly.
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
