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

	KeyCheckout
	KeyResume
	KeyPrompt // New key for entering a prompt
	KeyHelp   // Key for showing help screen

	// Diff keybindings
	KeyShiftUp
	KeyShiftDown

	KeySchedule
	KeyScheduleList
	KeyAttach
	KeyTasks
	KeyMicroClaw // Key for sending a message to microclaw

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
	"N":          KeyPrompt,
	"enter":      KeyEnter,
	"o":          KeyEnter,
	"n":          KeyNew,
	"D":          KeyKill,
	"q":          KeyQuit,
	"tab":        KeyTab,
	"shift+tab":  KeyShiftTab,
	"c":          KeyCheckout,
	"r":          KeyResume,
	"?":          KeyHelp,
	"s":          KeySchedule,
	"S":          KeyScheduleList,
	"a":          KeyAttach,
	"t":          KeyTasks,
	"m":          KeyMicroClaw,
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
	KeyPrompt: key.NewBinding(
		key.WithKeys("N"),
		key.WithHelp("N", "new with prompt"),
	),
	KeyCheckout: key.NewBinding(
		key.WithKeys("c"),
		key.WithHelp("c", "checkout"),
	),
	KeyTab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch tab"),
	),
	KeyShiftTab: key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "prev tab"),
	),
	KeyResume: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "resume"),
	),

	KeySchedule: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "schedule"),
	),
	KeyScheduleList: key.NewBinding(
		key.WithKeys("S"),
		key.WithHelp("S", "list schedules"),
	),
	KeyAttach: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "attach worktree"),
	),
	KeyTasks: key.NewBinding(
		key.WithKeys("t"),
		key.WithHelp("t", "tasks"),
	),
	KeyMicroClaw: key.NewBinding(
		key.WithKeys("m"),
		key.WithHelp("m", "microclaw"),
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
}
