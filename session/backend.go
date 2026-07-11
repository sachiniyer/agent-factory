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

	// Attach: an interactive controller can attach to the agent session.
	Attach bool
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
	// Each backend implements Start as two internal phases (#1592 Phase 1 PR4):
	// a PROVISION step that establishes WHERE the agent runs (the local git
	// worktree, or the remote workspace via launch_cmd) and a LAUNCH step that
	// starts WHAT runs in it (the tmux/PTY/agent process and its tabs). The
	// boundary is kept internal for now — Start is still the only interface
	// entry point — but it prepares the backend for the future agent-server
	// "provision-and-expose" model, where the two halves are driven separately.
	Start(instance *Instance, firstTimeSetup bool) error

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

	// Attach gives the user interactive terminal access to the AGENT session
	// (tab 0). The returned channel is closed when the user detaches.
	Attach(instance *Instance) (chan struct{}, error)

	// AttachTerminal gives interactive access to a NON-agent terminal tab
	// (#1592 Phase 1 PR5): a local shell tab at tabIdx for the local runtime, or
	// the single terminal_cmd shell for the remote hook runtime (which ignores
	// tabIdx). Both attach over a PTYStream; the returned channel is closed on
	// detach. Errors when no such terminal exists (e.g. remote_hooks.terminal_cmd
	// unset). This is on the interface so callers never type-assert a concrete
	// backend to reach a remote terminal.
	AttachTerminal(instance *Instance, tabIdx int) (chan struct{}, error)

	// HasUpdated reports whether the session output changed since the last
	// check and whether the program is showing a prompt, and returns the raw
	// captured pane content so the daemon's usage-limit detector (#1146) can
	// inspect it without a second capture. content is "" for backends with no
	// live pane (remote/hook) or when the capture is unavailable.
	HasUpdated(instance *Instance) (updated bool, hasPrompt bool, content string)

	// SendPrompt sends a prompt string via PTY writes.
	SendPrompt(instance *Instance, prompt string) error

	// SendPromptCommand sends a prompt using a more reliable command-based
	// approach (e.g. tmux send-keys).
	SendPromptCommand(instance *Instance, prompt string) error

	// SendKeys sends raw keys to the session (without pressing Enter).
	SendKeys(instance *Instance, keys string) error

	// SetPreviewSize sets the terminal dimensions for the session preview.
	SetPreviewSize(instance *Instance, width, height int) error

	// IsAlive returns true if the underlying session is still running.
	IsAlive(instance *Instance) bool

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
	// in Start stays authoritative for loads). Remote backends return
	// ErrRecoverUnsupported in v1: a Lost remote session is flagged but
	// reconnect semantics are their own design.
	Recover(instance *Instance) error

	// Respawn re-establishes an instance's backing session in place — re-spawning
	// the agent program via the resume path (resumeProgram: claude --continue,
	// codex resume --last) — WITHOUT any liveness precondition. It is the
	// guard-free core Recover wraps with its Lost guard; the usage-limit
	// manual-retry (#1146) uses it directly because a LimitReached session (which
	// Recover's !Lost guard rejects) needs the identical re-spawn. Callers own the
	// precondition. Remote backends return ErrRecoverUnsupported.
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
