package session

import (
	"fmt"
)

// firstTimeSetup is true if this is a new instance. Otherwise, it's one loaded from storage.
func (i *Instance) Start(firstTimeSetup bool) error {
	return i.backend.Start(i, firstTimeSetup)
}

// Kill terminates the instance and cleans up all resources
func (i *Instance) Kill() error {
	return i.backend.Kill(i)
}

// Recover re-establishes a Lost instance's backing session (#1108). Called by
// the daemon's restore loop and by user-initiated restore (#1300); loads stay
// side-effect free (#970).
func (i *Instance) Recover() error {
	return i.backend.Recover(i)
}

// Respawn re-establishes the instance's backing session in place without a
// liveness precondition — the guard-free core of Recover. The usage-limit
// manual-retry (#1146, resumeFromLimit) uses it to re-spawn an agent that exited
// while blocked at a limit wall: that session is LiveLimitReached, which Recover's
// !Lost guard rejects, but the re-spawn mechanics are identical. The caller owns
// the precondition.
func (i *Instance) Respawn() error {
	return i.backend.Respawn(i)
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
//   - drops the shell/process tabs entirely — only the agent session is brought
//     back on un-archive (Sachin's #1028 requirement);
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
	i.started = false
	i.liveness = LiveArchived
	i.inFlightOp = OpNone
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

// RestoreFromArchive re-spawns an archived instance's agent after its worktree
// has been moved back into place (#1028), flipping it live. It marks the
// instance started + Lost so the Recover re-spawn path is eligible (the same
// re-spawn the #1108 Lost-restore loop drives), then Recover brings the agent
// session up and sets Running (markLive clears the OpRestoring fence). On a
// Recover failure the instance is dropped to a plain Lost (op cleared), so the
// daemon's Lost-restore loop keeps retrying — the worktree is already back in
// place, so the session self-heals rather than stranding as Archived with no
// tmux. Only the agent tab is restored (shell/process tabs were dropped at
// archive time, per #1028).
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
	if err := i.backend.Recover(i); err != nil {
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
	return i.backend.CloseAttachOnly(i)
}

func (i *Instance) Preview() (string, error) {
	return i.backend.Preview(i)
}

func (i *Instance) HasUpdated() (updated bool, hasPrompt bool, content string) {
	return i.backend.HasUpdated(i)
}

// CheckAndHandleTrustPrompt checks for and dismisses the trust prompt for supported programs.
func (i *Instance) CheckAndHandleTrustPrompt() bool {
	return i.backend.CheckAndHandleTrustPrompt(i)
}

// TapEnter sends an enter key press to the tmux session if AutoYes is enabled.
func (i *Instance) TapEnter() {
	i.backend.TapEnter(i)
}

func (i *Instance) Attach() (chan struct{}, error) {
	return i.backend.Attach(i)
}

// AttachTerminal opens an interactive terminal tab (a local shell tab at tabIdx,
// or the remote terminal_cmd shell) through the uniform Backend interface — no
// concrete-backend type assertion (#1592 Phase 1 PR5, replacing the former
// AttachRemoteTerminal *HookBackend special-case). The returned channel is
// closed when the user detaches.
func (i *Instance) AttachTerminal(tabIdx int) (chan struct{}, error) {
	return i.backend.AttachTerminal(i, tabIdx)
}

func (i *Instance) SetPreviewSize(width, height int) error {
	return i.backend.SetPreviewSize(i, width, height)
}

// SendPrompt sends a prompt to the session
func (i *Instance) SendPrompt(prompt string) error {
	return i.backend.SendPrompt(i, prompt)
}

// SendPromptCommand sends a prompt using a more reliable command-based approach.
// This is more reliable for headless/scheduled runs where the PTY may not persist.
func (i *Instance) SendPromptCommand(prompt string) error {
	return i.backend.SendPromptCommand(i, prompt)
}

// PreviewFullHistory captures the entire session output including full scrollback history
func (i *Instance) PreviewFullHistory() (string, error) {
	return i.backend.PreviewFullHistory(i)
}

// SendKeys sends keys to the underlying session. For remote backends this
// returns an explicit error since raw key injection is not supported.
func (i *Instance) SendKeys(keys string) error {
	return i.backend.SendKeys(i, keys)
}

// Capabilities returns the backing runtime's capability descriptor (#1592
// Phase 1). A nil backend (a not-yet-initialised instance) reports local
// full parity: the UI treats a backend-less instance as a capable local
// session, so returning the zero value instead would be an incoherent
// descriptor (local workspace but every capability off) and would regress
// e.g. the tab-management footer.
func (i *Instance) Capabilities() Capabilities {
	if i.backend == nil {
		return (&LocalBackend{}).Capabilities()
	}
	return i.backend.Capabilities()
}

// GetBackend returns the backend for the instance (mainly for testing).
func (i *Instance) GetBackend() Backend {
	return i.backend
}

// SetBackend sets the backend for the instance (mainly for testing).
func (i *Instance) SetBackend(b Backend) {
	i.backend = b
}
