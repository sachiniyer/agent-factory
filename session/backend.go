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

	// Preview returns the current visible output of the session.
	Preview(instance *Instance) (string, error)

	// PreviewFullHistory returns the full scrollback history.
	PreviewFullHistory(instance *Instance) (string, error)

	// Attach gives the user interactive terminal access. The returned channel
	// is closed when the user detaches.
	Attach(instance *Instance) (chan struct{}, error)

	// HasUpdated reports whether the session output changed since the last
	// check and whether the program is showing a prompt.
	HasUpdated(instance *Instance) (updated bool, hasPrompt bool)

	// SendPrompt sends a prompt string via PTY writes.
	SendPrompt(instance *Instance, prompt string) error

	// SendPromptCommand sends a prompt using a more reliable command-based
	// approach (e.g. tmux send-keys).
	SendPromptCommand(instance *Instance, prompt string) error

	// SetPreviewSize sets the terminal dimensions for the session preview.
	SetPreviewSize(instance *Instance, width, height int) error

	// IsAlive returns true if the underlying session is still running.
	IsAlive(instance *Instance) bool

	// CheckAndHandleTrustPrompt auto-dismisses trust/permission prompts
	// for supported programs.
	CheckAndHandleTrustPrompt(instance *Instance) bool

	// TapEnter sends an Enter keystroke (used with AutoYes).
	TapEnter(instance *Instance)

	// Type returns the backend type identifier ("local" or "remote").
	Type() string
}
