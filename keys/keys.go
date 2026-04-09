package keys

import (
	"github.com/charmbracelet/bubbles/key"
)

type KeyName int

const (
	KeyUp KeyName = iota
	KeyDown
	KeyEnter
	KeyNew
	KeyKill
	KeyQuit

	KeyTab        // Tab is a special keybinding for switching between panes.
	KeyShiftTab   // ShiftTab cycles panes in reverse.
	KeySubmitName // SubmitName is a special keybinding for submitting the name of a new instance.

	KeyNewRemote // Key for creating a new remote instance
	KeyHelp      // Key for showing help screen

	// Diff keybindings
	KeyShiftUp
	KeyShiftDown

	KeyTask
	KeyTaskList
	KeyAttach

	KeySearch      // Key for searching sessions
	KeyTriggerTask // Key for triggering a task immediately

	KeyOpenPR // Key for opening PR in browser
	KeyCopyPR // Key for copying PR URL to clipboard
	KeyHooks  // Key for editing post-worktree hooks

	KeyChangeProgram // Key for changing the program during new instance naming

	// Sidebar navigation
	KeyLeft        // Collapse section / move to parent
	KeyRight       // Expand section
	KeyNextSection // Jump to next section header
	KeyPrevSection // Jump to previous section header
)

// GlobalKeyStringsMap is a global, immutable map string to keybinding.
var GlobalKeyStringsMap = map[string]KeyName{
	"up":         KeyUp,
	"k":          KeyUp,
	"down":       KeyDown,
	"j":          KeyDown,
	"shift+up":   KeyShiftUp,
	"shift+down": KeyShiftDown,
	"N":          KeyNewRemote,
	"enter":      KeyEnter,
	"o":          KeyEnter,
	"n":          KeyNew,
	"D":          KeyKill,
	"q":          KeyQuit,
	"tab":        KeyTab,
	"shift+tab":  KeyShiftTab,
	"?":          KeyHelp,
	"s":          KeyTask,
	"S":          KeyTaskList,
	"a":          KeyAttach,
	"/":          KeySearch,
	"r":          KeyTriggerTask,
	"p":          KeyOpenPR,
	"P":          KeyCopyPR,
	"H":          KeyHooks,
	"h":          KeyLeft,
	"left":       KeyLeft,
	"l":          KeyRight,
	"right":      KeyRight,
	"]":          KeyNextSection,
	"[":          KeyPrevSection,
}

// GlobalKeyBindings is a global, immutable map of KeyName to keybinding.
var GlobalKeyBindings = map[KeyName]key.Binding{
	KeyUp: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	KeyDown: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	KeyShiftUp: key.NewBinding(
		key.WithKeys("shift+up"),
		key.WithHelp("shift+↑", "scroll"),
	),
	KeyShiftDown: key.NewBinding(
		key.WithKeys("shift+down"),
		key.WithHelp("shift+↓", "scroll"),
	),
	KeyEnter: key.NewBinding(
		key.WithKeys("enter", "o"),
		key.WithHelp("↵/o", "open"),
	),
	KeyNew: key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "new"),
	),
	KeyKill: key.NewBinding(
		key.WithKeys("D"),
		key.WithHelp("D", "kill"),
	),
	KeyHelp: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	KeyQuit: key.NewBinding(
		key.WithKeys("q"),
		key.WithHelp("q", "quit"),
	),
	KeyNewRemote: key.NewBinding(
		key.WithKeys("N"),
		key.WithHelp("N", "new remote"),
	),
	KeyTab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch tab"),
	),
	KeyShiftTab: key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "prev tab"),
	),
	KeyTask: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "task"),
	),
	KeyTaskList: key.NewBinding(
		key.WithKeys("S"),
		key.WithHelp("S", "list tasks"),
	),
	KeyAttach: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "attach worktree"),
	),
	KeySearch: key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "search"),
	),
	KeyTriggerTask: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "run now"),
	),
	KeyOpenPR: key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "open PR"),
	),
	KeyCopyPR: key.NewBinding(
		key.WithKeys("P"),
		key.WithHelp("P", "copy PR URL"),
	),
	KeyHooks: key.NewBinding(
		key.WithKeys("H"),
		key.WithHelp("H", "worktree hooks"),
	),
	KeyLeft: key.NewBinding(
		key.WithKeys("h", "left"),
		key.WithHelp("h/←", "collapse"),
	),
	KeyRight: key.NewBinding(
		key.WithKeys("l", "right"),
		key.WithHelp("l/→", "expand"),
	),
	KeyNextSection: key.NewBinding(
		key.WithKeys("]"),
		key.WithHelp("]", "next section"),
	),
	KeyPrevSection: key.NewBinding(
		key.WithKeys("["),
		key.WithHelp("[", "prev section"),
	),

	// -- Special keybindings --

	KeySubmitName: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "submit name"),
	),
	KeyChangeProgram: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "change program"),
	),
}
