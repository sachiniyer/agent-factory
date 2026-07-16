package session

// remoteAgentBackend is the common backend behavior for workspaces whose data
// plane is an AgentServer reached through the daemon. Docker, SSH, and hook
// runtimes differ only in how they provision and reap that workspace; after
// provisioning, their lifecycle and agent-facing operations must stay identical.
type remoteAgentBackend struct {
	// reap tears down the provisioned workspace. It is shared with the runtime's
	// Teardown / AgentServer Kill path and is idempotent behind the closure. A nil
	// reap marks an inert backend rebuilt from disk.
	reap func() error
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
	}
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
	i.started = true
	if len(i.Tabs) == 0 {
		i.Tabs = []*Tab{newRemoteAgentTab()}
	}
	i.mu.Unlock()
	return nil
}

func (b *remoteAgentBackend) Kill(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	if b.reap != nil {
		return b.reap()
	}
	return nil
}

// CloseAttachOnly discards a duplicate instance's local view without reaping
// the remote workspace its canonical instance still owns.
func (b *remoteAgentBackend) CloseAttachOnly(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	return nil
}

func (b *remoteAgentBackend) Preview(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, false)
}

func (b *remoteAgentBackend) PreviewFullHistory(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, true)
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
func (b *remoteAgentBackend) IsAlive(i *Instance) bool {
	alive, _ := i.AgentServer().Alive()
	return alive
}

// CheckAndHandleTrustPrompt is a daemon-side no-op: each remote AgentServer
// handles it before returning a snapshot.
func (b *remoteAgentBackend) CheckAndHandleTrustPrompt(*Instance) bool { return false }

func (b *remoteAgentBackend) TapEnter(i *Instance) { i.AgentServer().TapEnter() }

// Recover and Respawn both re-provision a disposable remote workspace from the
// session branch, then launch it again.
func (b *remoteAgentBackend) Recover(i *Instance) error { return recoverSandbox(i) }
func (b *remoteAgentBackend) Respawn(i *Instance) error { return recoverSandbox(i) }
