package session

import "strings"

// remoteAgentBackend is the common backend behavior for workspaces whose data
// plane is an AgentServer reached through the daemon. Docker, SSH, and hook
// runtimes differ only in how they provision and reap that workspace; after
// provisioning, their lifecycle and agent-facing operations must stay identical.
type remoteAgentBackend struct {
	// resolvedProgram is the override-resolved command the provisioned
	// agent-server is bound to launch — the --program value the runtime handed
	// it (or handed the launch/provision hook that started it). The launch RPC
	// records it on the instance as runtime evidence, the remote twin of
	// LocalBackend's setRuntimeLaunch (#5067), so agent detection keys off the
	// command the session ACTUALLY runs rather than the configured enum
	// (#5108). Empty on an inert backend rebuilt from disk — which Launch can
	// never succeed on anyway (deadRemoteAgentServer refuses).
	resolvedProgram string
}

// Capabilities reports the common off-box runtime contract. Tab management is
// false because the AgentServer's tab API is data-plane only: it can drive an
// existing tab but cannot create the daemon-side git worktree required for a
// new one (#1874).
func (b *remoteAgentBackend) Capabilities() Capabilities {
	return Capabilities{
		Workspace:        WorkspaceRemote,
		Archive:          true,
		Recover:          true,
		TabManagement:    false,
		TerminalTab:      true,
		InteractiveInput: true,
		Handoff:          false,
	}
}

// SwapAgent is not serviced off-box (#2013). Swapping the agent inside a
// provisioned sandbox means re-launching a different process INSIDE it while
// keeping the workspace — but every re-spawn path these runtimes have
// (recoverSandbox) re-provisions the sandbox and re-clones the branch from
// origin, which would discard unpushed work rather than hand it over. Wiring a
// genuine in-sandbox relaunch is its own change; until then this says so instead
// of quietly doing the destructive thing.
func (b *remoteAgentBackend) PrepareAgentSwap(*Instance, string) (AgentSwapPlan, error) {
	return AgentSwapPlan{}, ErrHandoffUnsupported
}

func (b *remoteAgentBackend) SwapAgent(*Instance, AgentSwapPlan) error {
	return ErrHandoffUnsupported
}

// Start provisions then launches the remote workspace through its AgentServer.
func (b *remoteAgentBackend) Start(i *Instance, firstTimeSetup bool) error {
	if err := b.Provision(i, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(i, firstTimeSetup)
}

func (b *remoteAgentBackend) Provision(i *Instance, firstTimeSetup bool) error {
	return i.AgentServer().Provision(firstTimeSetup)
}

// Launch starts the remote agent and seeds the daemon-side mirror with its
// agent tab, if it does not already have one.
func (b *remoteAgentBackend) Launch(i *Instance, firstTimeSetup bool) error {
	if err := i.AgentServer().Launch(firstTimeSetup); err != nil {
		return err
	}
	i.mu.Lock()
	// The launch RPC's success is the boundary at which the bound command
	// positively established this runtime — record it here rather than at
	// provision so a failed launch leaves no claim about a command that never
	// ran. A remote session has no pane to pin a (pid, startID) identity to,
	// so the command alone is the evidence; it is the record
	// resolvedAgentLocked reads when no local tmux binding exists (#5108).
	if program := strings.TrimSpace(b.resolvedProgram); program != "" {
		i.setRuntimeProgramLocked(program)
	}
	if !i.started {
		i.started = true
		i.touchLocked()
	}
	if len(i.Tabs) == 0 {
		i.Tabs = []*Tab{newRemoteAgentTab()}
		i.touchLocked()
	}
	// AFTER the agent tab, never before: index 0 is the agent everywhere — it is
	// unclosable and it is what the PTY stream targets — so a restored web tab that
	// landed there would be read as the agent by every consumer (#3062). Draining
	// here rather than at load is what keeps that ordering true.
	i.appendPendingMetadataTabsLocked()
	i.mu.Unlock()
	return nil
}

// trustLiveGeneration is unused: no tmux, no generation cohort (#3413).
func (b *remoteAgentBackend) Kill(i *Instance, _ bool) error {
	i.mu.Lock()
	if i.started {
		i.started = false
		i.touchLocked()
	}
	i.mu.Unlock()
	return nil
}

// CloseAttachOnly discards a duplicate instance's local view without reaping
// the remote workspace its canonical instance still owns.
func (b *remoteAgentBackend) CloseAttachOnly(i *Instance) error {
	i.mu.Lock()
	if i.started {
		i.started = false
		i.touchLocked()
	}
	i.mu.Unlock()
	return nil
}

func (b *remoteAgentBackend) Preview(i *Instance) (string, error) {
	snapshot, err := i.AgentServer().Preview(0, false)
	return snapshot.Content, err
}

func (b *remoteAgentBackend) PreviewFullHistory(i *Instance) (string, error) {
	snapshot, err := i.AgentServer().Preview(0, true)
	return snapshot.Content, err
}

func (b *remoteAgentBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool, content string) {
	obs, err := i.AgentServer().Snapshot()
	if err != nil {
		return false, false, ""
	}
	return obs.Updated, obs.HasPrompt, obs.Content
}

func (b *remoteAgentBackend) SendPromptCommand(i *Instance, prompt string) error {
	return i.AgentServer().SendPrompt(prompt)
}

// IsAlive intentionally collapses an unanswerable AgentServer probe to false:
// its callers only use it for non-destructive TUI affordances. Destructive
// recovery paths call AgentServer.Alive directly so they can distinguish an
// unreachable remote from a dead one (#1794).
func (b *remoteAgentBackend) IsAlive(i *Instance) (bool, error) {
	// The error is now carried, not discarded: an unanswerable sandbox is unknown,
	// and the debounce (#1794) is what turns a run of those into a conclusion.
	return i.AgentServer().Alive()
}

// CheckAndHandleTrustPrompt is a daemon-side no-op: each remote AgentServer
// handles it before returning a snapshot.
func (b *remoteAgentBackend) CheckAndHandleTrustPrompt(*Instance) bool { return false }

// AgentModelChange is carried by the remote AgentServer's Snapshot; asking the
// daemon-side backend would recurse through that same server.
func (b *remoteAgentBackend) AgentModelChange(*Instance) *AgentModelChange { return nil }

// Recover and Respawn both re-provision a disposable remote workspace from the
// session branch, then launch it again.
func (b *remoteAgentBackend) Recover(i *Instance) error { return recoverSandbox(i) }
func (b *remoteAgentBackend) Respawn(i *Instance) error { return recoverSandbox(i) }
