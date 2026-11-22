package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// The agent-server half of the off-box account swap (#5571 W7.6): the scope
// rotation itself. Everything here runs ON THE SANDBOX under the swap's
// serialization — never over SSH from the daemon — so a rotation, a staged
// scope's discard and a status read can never tear a directory out from under
// one another (#7773 R2).

// swapAccountInPlace is swapAgentInPlace to the SAME agent, with the scope
// rotation between the confirmed stop and the start. The agent-server's own
// environment names the scope (the launch guard guaranteed it), so nothing a
// caller sends can redirect the rotation.
func (i *Instance) swapAccountInPlace(swapID string) (string, error) {
	agent := i.CurrentAgentName()
	rot, err := newScopeRotation(agent, swapID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errStageNotMine, err)
	}
	// The command that was proven and is running, byte for byte: never a
	// re-resolution of the agent name, and never one af cannot attribute.
	restart, err := i.provenRestartCommand(agent)
	if err != nil {
		return "", err
	}
	i.mu.Lock()
	i.restartingSameAgent = true
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		i.restartingSameAgent = false
		i.mu.Unlock()
	}()
	program, err := i.swapAgentInPlaceFrom(agent, restart, rot.rotateAfter(i.stopSiblingsForAccountSwap))
	if err != nil {
		// The scope may have been rotated before the start failed; leave it as A so
		// a retry or a relaunch finds the identity the row still records.
		if rbErr := rot.restore(); rbErr != nil {
			err = fmt.Errorf("%w (and the account scope could not be put back: %v; %s)", err, rbErr, accountScopeIndeterminate)
		}
		return "", err
	}
	return program, nil
}

// provenRestartCommand is the command the running agent was launched with — the
// runtime evidence the launch recorded — re-proven attributed. The local swap
// restarts the same evidence; an account-scoped agent-server proved it before the
// first start (RequireAttributedAccountProgram) and this proves it again, so the
// restart cannot launch anything else.
func (i *Instance) provenRestartCommand(agent string) (string, error) {
	command := i.RuntimeProgram()
	if command == "" {
		command = i.ResolvedPaneProgram()
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("session %q has no recorded launch command to restart; refusing to guess one for an account-scoped agent", i.Title)
	}
	if got := sessionenv.AgentForCommand(command); got != agent {
		return "", fmt.Errorf("session %q's launch command %q runs %q, not %q; refusing to restart a different agent under the account", i.Title, command, got, agent)
	}
	if err := RequireAttributedAccountProgram(command, agent); err != nil {
		return "", err
	}
	return command, nil
}

// prepareRestartSwap is PrepareAgentSwap for the account swap: the same runtime
// gates, with the launch command fixed to restart.
func (i *Instance) prepareRestartSwap(target, restart string) (AgentSwapPlan, error) {
	if err := i.ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		return AgentSwapPlan{}, err
	}
	if err := i.ValidateHandoffTarget(target); err != nil {
		return AgentSwapPlan{}, err
	}
	if !i.Capabilities().Handoff {
		return AgentSwapPlan{}, ErrHandoffUnsupported
	}
	return prepareAgentSwapFrom(i, target, restart)
}

// stopSiblingsForAccountSwap stops every other credential-bearing pane — shell and
// process tabs inherited the scope — exactly as the local swap does, so none
// survives the rotation holding the old account or a dangling scope. A pane that
// cannot be confirmed stopped aborts the swap before anything rotates.
func (i *Instance) stopSiblingsForAccountSwap() error {
	b, ok := i.currentBackend().(*LocalBackend)
	if !ok {
		return fmt.Errorf("session %q has no local runtime whose panes can be stopped", i.Title)
	}
	return b.stopForAccountSwap(i, true)
}

// rotateAfter is the swap's post-stop step: siblings are stopped first, and only
// when that is confirmed does the scope move.
func (r *scopeRotation) rotateAfter(stopSiblings func() error) func() error {
	return func() error {
		if err := stopSiblings(); err != nil {
			return err
		}
		return r.rotate()
	}
}

// scopeRotation is the directory dance, derived from the environment only.
type scopeRotation struct {
	live, next, prev string
	rotated          bool
}

// discardAccountNextInPlace removes the staged scope on the host itself — the
// agent-server half of the serialized discard (#7773 R2). It runs under the
// swap's own serialization, so it can never overlap a rotation the way an
// unordered remote rm could. And like the rotation it removes only the stage
// bound to ITS swap: the daemon's request may outlive the call that sent it —
// a timed-out request the server still completes — so a late discard checks
// the marker and leaves a stage that is not its swap's to delete.
func (i *Instance) discardAccountNextInPlace(swapID string) error {
	_, next, _, err := accountScopeDirs(i.CurrentAgentName())
	if err != nil {
		return err
	}
	if swapID != "" {
		marker, mErr := os.ReadFile(filepath.Join(next, accountSwapMarkerRel))
		if mErr != nil || strings.TrimSpace(string(marker)) != swapID {
			return nil // a different swap's stage, or none of this one's — leave it
		}
	}
	return os.RemoveAll(next)
}

// accountScopeDirs derives the live/staged/outgoing scope paths for an agent
// from the process environment — the derivation newScopeRotation and the
// serialized discard share.
func accountScopeDirs(agent string) (live, next, prev string, err error) {
	envName, ok := sessionenv.SupportsAccounts(agent)
	if !ok {
		return "", "", "", fmt.Errorf("agent %q has no account scope to rotate", agent)
	}
	live = filepath.Clean(os.Getenv(envName))
	if live == "." || filepath.Base(live) != sshAccountSubdir {
		return "", "", "", fmt.Errorf("this sandbox's %s is not a session account scope; refusing to rotate it", envName)
	}
	return live, live + accountNextSuffix, live + accountPrevSuffix, nil
}

// newScopeRotation resolves the live scope from the agent's own scope variable
// and requires the staged one beside it. wantID is the id the swap request
// carried: when non-empty the staged scope's marker must name the same id, or
// the scope belongs to a different swap and the request refuses rather than
// rotate another swap's stage in (#7773 R1). A request with no id is
// pre-identity and rotates whatever is staged — a commit that can never be
// attributed, which the daemon reconciles as indeterminate.
func newScopeRotation(agent, wantID string) (*scopeRotation, error) {
	live, next, prev, err := accountScopeDirs(agent)
	if err != nil {
		return nil, err
	}
	r := &scopeRotation{live: live, next: next, prev: prev}
	st, err := os.Lstat(r.next)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("no staged account at %s; nothing to rotate in", r.next)
	}
	if wantID != "" {
		marker, mErr := os.ReadFile(filepath.Join(r.next, accountSwapMarkerRel))
		if mErr != nil || strings.TrimSpace(string(marker)) != wantID {
			return nil, fmt.Errorf("the staged scope at %s was not staged for this swap; refusing to rotate another swap's stage in", r.next)
		}
	}
	hasCred := false
	for _, rel := range accountCredentialRel[agent] {
		if f, err := os.Lstat(filepath.Join(r.next, rel)); err == nil && f.Mode().IsRegular() {
			hasCred = true
			break
		}
	}
	if !hasCred {
		return nil, fmt.Errorf("the staged account at %s holds no credential file; refusing to rotate it in", r.next)
	}
	return r, nil
}

// scopeRename is the rename syscall the rotation and its undo use; os.Rename by
// default, and a seam for tests that must inject a rename failure (the
// double-failure arm of rotate that has to carry the indeterminate marker).
var scopeRename = os.Rename

// rotate moves the live scope aside and the staged one into place. It runs with
// the old agent confirmed stopped.
func (r *scopeRotation) rotate() error {
	if err := os.RemoveAll(r.prev); err != nil {
		return fmt.Errorf("clearing %s: %w", r.prev, err)
	}
	if err := scopeRename(r.live, r.prev); err != nil {
		return fmt.Errorf("moving the outgoing account aside: %w", err)
	}
	if err := scopeRename(r.next, r.live); err != nil {
		if rbErr := scopeRename(r.prev, r.live); rbErr != nil {
			return fmt.Errorf("moving the incoming account into place: %w (and the outgoing one could not be restored: %v; %s)", err, rbErr, accountScopeIndeterminate)
		}
		return fmt.Errorf("moving the incoming account into place: %w", err)
	}
	r.rotated = true
	// The staged scope's swap binding served its purpose; it is not part of
	// the account and must not ride the live scope into the next write-back.
	_ = os.Remove(filepath.Join(r.live, accountSwapMarkerRel))
	return nil
}

// restore undoes a completed rotation: the incoming scope goes back to staging,
// the outgoing one back to live.
func (r *scopeRotation) restore() error {
	if !r.rotated {
		return nil
	}
	if err := os.RemoveAll(r.next); err != nil {
		return err
	}
	if err := scopeRename(r.live, r.next); err != nil {
		return err
	}
	if err := scopeRename(r.prev, r.live); err != nil {
		return err
	}
	r.rotated = false
	return nil
}
