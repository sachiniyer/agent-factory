package session

// WorkspaceKind describes where a backend's workspace physically lives, so
// callers reason about locality without asking "is this the remote type"
// (#1592 Phase 1). New runtimes (ssh/container) pick the kind that matches
// where the git worktree lands.
type WorkspaceKind int

const (
	// WorkspaceLocalWorktree: a git worktree on the daemon's own machine, driven
	// by tmux (today's LocalBackend). Zero value — a backend-less instance reads
	// as a local workspace.
	WorkspaceLocalWorktree WorkspaceKind = iota
	// WorkspaceRemote: the workspace lives off-box; there is no local worktree or
	// tmux to drive (today's HookBackend; tomorrow's ssh/container runtimes).
	WorkspaceRemote
)

// Capabilities is a backend's self-description: which optional session
// operations it can service. The daemon and UI branch on these instead of on
// Type()=="remote" (#1592 Phase 1), so a NEW backend declares what it supports
// rather than every call-site learning its name. The end state is full parity —
// every backend implements every capability — but the descriptor stays so a
// surface can gray out an op a given runtime hasn't wired up yet.
type Capabilities struct {
	// Workspace records where the workspace lives (local worktree vs off-box).
	Workspace WorkspaceKind

	// There is deliberately no Attach bit (#1860). Attach is not an optional
	// capability any more: every runtime attaches client-side over the WS PTY
	// stream, so the bit was unconditionally true for every backend and no
	// dispatch ever read it. A capability that cannot be false gates nothing —
	// it only invites a future attach gate to branch on a constant.

	// Archive: the session can be archived/restored (local-worktree relocation
	// today; push/pull the branch once every backend clones from GitHub).
	Archive bool
	// Recover: a Lost session can be reconnected / re-spawned in place.
	Recover bool
	// TabManagement: the user can add/close arbitrary tabs (new process tab).
	TabManagement bool
	// TerminalTab: an interactive terminal tab is available. May depend on
	// per-session config (remote_hooks.terminal_cmd), so it is computed per
	// instance rather than being a static per-type constant.
	TerminalTab bool
	// InteractiveInput: raw key / prompt injection works — SendKeys/TapEnter/
	// SendPrompt drive a live PTY rather than returning "not supported".
	InteractiveInput bool
}

// Backend abstracts the session lifecycle so that instances can be backed
// by local tmux+git worktrees (the default) or by user-provided remote
// hook scripts.
type Backend interface {
	// Start initialises the session. When firstTimeSetup is true a brand-new
	// session is created; otherwise an existing one is restored from storage.
	//
	// Each backend implements Start as two phases (#1592 Phase 1 PR4): a PROVISION
	// step that establishes WHERE the agent runs (the local git worktree, or the
	// remote workspace via launch_cmd) and a LAUNCH step that starts WHAT runs in
	// it (the tmux/PTY/agent process and its tabs). Start is Provision then Launch.
	// The two halves are on the interface (#1592 Phase 2 PR4) so the local
	// agent-server's provision-and-expose model can drive them separately; Start
	// stays as the combined lifecycle entry point its existing callers use.
	Start(instance *Instance, firstTimeSetup bool) error

	// Provision establishes WHERE the session runs without starting the agent
	// process — the local git worktree + tmux binding, or a remote/off-box
	// workspace. The first half of Start. See each backend's implementation for the
	// precise on-disk vs in-memory boundary.
	Provision(instance *Instance, firstTimeSetup bool) error

	// Launch starts (or restores) WHAT runs in the workspace Provision established
	// — the agent process and its tabs. The second half of Start; it owns the
	// failure-cleanup scope (a launch failure tears down Provision's worktree on a
	// fresh create).
	Launch(instance *Instance, firstTimeSetup bool) error

	// Kill terminates the session and cleans up all associated resources.
	Kill(instance *Instance) error

	// CloseAttachOnly releases the resources this Instance opened to view or
	// drive the session (a tmux attach PTY, a remote preview process) WITHOUT
	// destroying the underlying session, worktree, or remote record. It is the
	// non-destructive sibling of Kill, used to discard a duplicate Instance
	// built from disk that lost a race to the canonical, still-tracked
	// Instance — see the daemon's findSession (#867). Killing such a duplicate
	// would tear down state the canonical Instance shares; closing only its
	// attach resources reclaims the PTY without that collateral damage.
	CloseAttachOnly(instance *Instance) error

	// Preview returns the current visible output of the session.
	Preview(instance *Instance) (string, error)

	// PreviewFullHistory returns the full scrollback history.
	PreviewFullHistory(instance *Instance) (string, error)

	// There is deliberately no Attach/AttachTerminal here (#1852). Interactive
	// attach is CLIENT-side for every runtime: the client dials the daemon's WS
	// PTY stream (apiclient.AttachStream), and the daemon resolves locality via
	// instance.AgentServer() — a local broker, or a remoteAgentServer proxy for
	// docker/ssh/hook. A backend-routed attach is therefore not a thing a caller
	// can express, which is what keeps #1837 (remote attach aimed at an erroring
	// backend stub) from recurring.

	// HasUpdated reports whether the session output changed since the last
	// check and whether the program is showing a prompt, and returns the raw
	// captured pane content so the daemon's usage-limit detector (#1146) can
	// inspect it without a second capture. content is "" for backends with no
	// live pane (remote/hook) or when the capture is unavailable.
	HasUpdated(instance *Instance) (updated bool, hasPrompt bool, content string)

	// SendPromptCommand sends a prompt using a reliable command-based approach
	// (tmux send-keys for the local runtime). This is the SOLE prompt-delivery
	// primitive: AgentServer.SendPrompt delegates here, and it lands whether or
	// not a PTY is currently attached — the raw PTY-write SendPrompt (a 100ms
	// send-then-Enter) was deleted as dead post-migration (#1626).
	SendPromptCommand(instance *Instance, prompt string) error

	// IsAlive returns true if the underlying session is still running.
	// IsAlive reports whether the instance's agent is running, and returns an
	// error when the runtime could not be ASKED (#1917 round 8). The error is the
	// tri-state: a bool alone forces a timed-out probe to pick yes or no, and the
	// convenient pick — "yes" — is what let a wedged tmux server be counted as
	// affirmative proof of life all the way up in the daemon's poll. An
	// implementation that cannot answer must say so rather than guess.
	IsAlive(instance *Instance) (bool, error)

	// CheckAndHandleTrustPrompt auto-dismisses trust/permission prompts
	// for supported programs.
	CheckAndHandleTrustPrompt(instance *Instance) bool

	// TapEnter sends an Enter keystroke (used with AutoYes).
	TapEnter(instance *Instance)

	// Recover re-establishes a Lost session's backing resources — the tmux
	// session vanished out from under a live record with no kill on record
	// (#1108) — re-spawning the program in the instance's worktree. It is
	// invoked by the daemon's restore loop and by user-initiated restore
	// (af sessions restore), never as a load-time side effect (the #970 guard
	// in Start stays authoritative for loads). Every backend services it at full
	// parity since #1592 Phase 4: the sandbox runtimes (docker/ssh/hook)
	// re-provision a fresh sandbox that clones the durable branch back from
	// origin (recoverSandbox, §5.1) — there is no ErrRecoverUnsupported anymore.
	Recover(instance *Instance) error

	// Respawn re-establishes an instance's backing session in place — re-spawning
	// the agent program via the resume path (resumeProgram: claude --continue,
	// codex resume --last) — WITHOUT any liveness precondition. It is the
	// guard-free core Recover wraps with its Lost guard; the usage-limit
	// manual-retry (#1146) uses it directly because a LimitReached session (which
	// Recover's !Lost guard rejects) needs the identical re-spawn. Callers own the
	// precondition. The sandbox runtimes (docker/ssh/hook) service it through the
	// same recoverSandbox re-provision-and-clone path as Recover — no backend
	// returns an unsupported sentinel.
	Respawn(instance *Instance) error

	// Type returns the backend type identifier ("local" or "remote"). Since
	// #1592 Phase 1 this is the persisted serialization discriminator only (the
	// load-time factory in instance_data.go) — runtime branching goes through
	// Capabilities, never Type().
	Type() string

	// Capabilities reports which optional operations this backend can service,
	// replacing Type()-based special-casing (#1592 Phase 1). Computed per
	// instance because some capabilities (TerminalTab) depend on per-session
	// config.
	Capabilities() Capabilities
}
