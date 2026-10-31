package session

import (
	"context"
	"fmt"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// CloseAttachOnly releases this instance's hold on its tmux sessions — the
// attach PTYs and the `tmux attach-session` child processes — WITHOUT running
// `tmux kill-session`. The server-side tmux sessions and the git worktree
// behind them are left untouched. The daemon uses this to discard a duplicate
// Instance built from disk that turned out to already be tracked in memory
// (#867): the duplicate must surrender the PTYs it opened during restore
// without tearing down the live sessions the canonical Instance shares.
func (b *LocalBackend) CloseAttachOnly(i *Instance) error {
	// Since #930 an instance holds N tabs (agent + shell/process), and restoring
	// the duplicate opened an attach PTY for EVERY tab (Start restores the agent
	// tab, setupTabs the rest), so releasing only the agent tab's PTY leaked one
	// fd per extra tab each time the daemon discarded a duplicate — eventually
	// EMFILE in the long-running daemon (#1065). The release-PTY mode drops every
	// tab's attach without kill-session and leaves the worktree intact — see
	// teardownTabs — returning the joined per-tab close errors.
	return i.teardownTabs(teardownReleasePTY{})
}

func (b *LocalBackend) Preview(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContent()
}

// PreviewContext is Preview bound to ctx: the pane capture is cancellable, so a
// cancelled readiness wait tears down the in-flight `tmux capture-pane` subprocess
// instead of letting it run to completion (task.WaitForReady's capturePreview).
func (b *LocalBackend) PreviewContext(ctx context.Context, i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentContext(ctx)
}

func (b *LocalBackend) PreviewFullHistory(i *Instance) (string, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentWithOptions("-", "-")
}

func (b *LocalBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool, content string) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return false, false, ""
	}
	return ts.HasUpdatedExpectingTeardown(i.teardownExpected)
}

func (b *LocalBackend) SendPromptCommand(i *Instance, prompt string) error {
	_, err := b.SendPromptCommandWithStatus(i, prompt)
	return err
}

func (b *LocalBackend) SendPromptCommandWithStatus(i *Instance, prompt string) (PromptDeliveryStatus, error) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s {
		return PromptCouldNotConfirm, fmt.Errorf("instance not started")
	}
	if ts == nil {
		return PromptCouldNotConfirm, fmt.Errorf("tmux session not initialized")
	}
	return ts.SendKeysCommandObserved(prompt)
}

func (b *LocalBackend) IsAlive(i *Instance) (bool, error) {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if ts == nil {
		// No binding at all: an answer, not a guess.
		return false, nil
	}
	// ProbeSession, not ExistsOrUnknown (#1917 round 8): this result is EVIDENCE —
	// the daemon's poll turns it into probeAlive, which clears a session's restore
	// history and marks it Ready. ExistsOrUnknown reports a timed-out probe as
	// "exists", so taking it here would report a wedged server as a live agent.
	// ProbeSessionLiveness: an unlinked socket with a live server is unknown,
	// not gone, so a live agent is never respawned over (#4135).
	exists, known := ts.ProbeSessionLiveness()
	if !known {
		return false, fmt.Errorf("%w: has-session while probing liveness", tmux.ErrTmuxTimeout)
	}
	return exists, nil
}

func (b *LocalBackend) CheckAndHandleTrustPrompt(i *Instance) bool {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return false
	}
	// Dispatch on the agent the pane actually runs (ResolvedAgent) so a
	// program_overrides entry pointing an agent name at a non-agent binary
	// never gets an agent's trust-prompt handling (#1116/#1131 defect class),
	// while restored sessions with legacy free-form Program values (e.g.
	// "/home/foo/bin/claude") still get it — same persisted-state class of
	// regression as #677.
	//
	// Which agents are in the set, and why, lives with the gate itself in
	// tmux.ProgramNeedsTrustDismissal — the daemon's config-agent spawn calls the
	// same function. This site used to hold the list and the daemon's a copy of
	// it; the copy drifted and lost opencode (#2416).
	if tmux.ProgramNeedsTrustDismissal(i.ResolvedAgent()) {
		return ts.CheckAndHandleTrustPrompt()
	}
	return false
}

func (b *LocalBackend) AgentModelChange(i *Instance) *AgentModelChange {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return nil
	}
	before, after, changed := ts.CodexSafetyModelChange()
	if !changed {
		return nil
	}
	return NewAgentModelChange(before, after)
}

// newWorktreeHandle derives this instance's worktree handle — the in-place tree,
// or the placement the branch name and config determine — without touching the
// filesystem. It is the one derivation both a first-time create and the
