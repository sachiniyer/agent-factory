package app

// The TUI's top-level input state. Split out of home_model.go (#1145 file-length
// limit, #3021): that file sat exactly at its 1000-line ceiling, so the next state
// added anywhere would have tripped the lint — and shaving a comment to make room is
// how a file stays at its ceiling forever. The enum is a self-contained unit and
// reads better on its own.

type state int

const (
	stateDefault state = iota
	// stateNew is the state when the user is creating a new instance.
	stateNew
	// stateHelp is the state when a help screen is displayed.
	stateHelp
	// stateConfirm is the state when a confirmation modal is displayed.
	stateConfirm
	// stateSearch is the state when the user is searching sessions.
	stateSearch
	// stateSwitchProject is the state when the project-picker overlay is open
	// (#1461): pick another repo af has seen and switch the TUI to it in place.
	stateSwitchProject
	// stateSelectProgram is the state when the user is selecting a program during naming.
	stateSelectProgram
	// stateSelectTabKind is the state when the user is choosing which kind of tab
	// the new-tab action should create.
	stateSelectTabKind
	// statePromptInput is the state when the initial-prompt field of the naming
	// form is open (#1936). Like stateSelectProgram it is a sub-state of
	// stateNew: closing it returns to naming rather than to stateDefault.
	statePromptInput
	// stateHooks is the state when the post-worktree hooks editor overlay is
	// open (#1024 PR 4: hooks lost their persistent sidebar slot and are
	// hosted as a modal overlay instead).
	stateHooks
	// stateTasks is the state when the task manager (list + create/edit form)
	// overlay is open. The in-rail automations section shows only the compact
	// summary (#1087 play-test): the full manager gets a centered overlay so
	// its form is never clamped into the narrow rail.
	stateTasks
	// stateConfigEditor is the state when the global config editor overlay is
	// open (","). Like the hooks and tasks overlays it owns the keyboard while
	// open, so its value field can take arbitrary text (a listen address, a
	// branch prefix) without the global key map eating the runes.
	stateConfigEditor
	// stateSelectHandoffAgent is the state when the user is picking which agent
	// to hand the selected session off to (#2013). It reuses the same selection
	// overlay stateSelectProgram uses at create time; the two differ only in what
	// happens on submit — create stashes the choice, handoff confirms and swaps.
	stateSelectHandoffAgent
	// stateSelectBackend is the state when the backend field of the naming form
	// is open (#1933): the runtime the session will be created on, listed from the
	// daemon's ListBackends catalog. Like stateSelectProgram and statePromptInput
	// it is a sub-state of stateNew — closing it returns to naming.
	stateSelectBackend
	// stateSelectAccount is the state when the account field of the naming form is
	// open (#3844): which of the agent's registered credential accounts the session
	// runs as, listed from the daemon's own ListAccounts registry. A sub-state of
	// stateNew like stateSelectBackend beside it — closing it returns to naming.
	stateSelectAccount
	// stateJumpTab is the unbounded jump-to-tab prompt (#3021). Its own state rather
	// than statePromptInput's because that one belongs to the naming form and returns
	// to stateNew when it closes; this returns to stateDefault. Both drive the same
	// promptOverlay field — one overlay, two owners, distinguished by who is asking.
	stateJumpTab
)
