package session

// Backend abstracts the session lifecycle so that instances can be backed
// by local tmux+git worktrees (the default) or by user-provided remote
// hook scripts.
type Backend interface {
	// Start initialises the session. When firstTimeSetup is true a brand-new
	// session is created; otherwise an existing one is restored from storage.
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

	// Attach gives the user interactive terminal access. The returned channel
	// is closed when the user detaches.
	Attach(instance *Instance) (chan struct{}, error)

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
	// invoked ONLY by the daemon's explicit restore loop, never as a load-time
	// side effect (the #970 guard in Start stays authoritative for loads).
	// Remote backends return ErrRecoverUnsupported in v1: a Lost remote
	// session is flagged but reconnect semantics are their own design.
	Recover(instance *Instance) error

	// Respawn re-establishes an instance's backing session in place — re-spawning
	// the agent program via the resume path (resumeProgram: claude --continue,
	// codex resume --last) — WITHOUT any liveness precondition. It is the
	// guard-free core Recover wraps with its Lost guard; the usage-limit
	// manual-retry (#1146) uses it directly because a LimitReached session (which
	// Recover's !Lost guard rejects) needs the identical re-spawn. Callers own the
	// precondition. Remote backends return ErrRecoverUnsupported.
	Respawn(instance *Instance) error

	// Type returns the backend type identifier ("local" or "remote").
	Type() string
}
