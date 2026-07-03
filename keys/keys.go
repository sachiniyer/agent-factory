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

	KeyTab        // Tab cycles the workspace focus ring (tree → pane → automations).
	KeyShiftTab   // ShiftTab cycles the focus ring in reverse.
	KeySubmitName // SubmitName is a special keybinding for submitting the name of a new instance.

	KeyNewTab   // NewTab spawns a new shell tab in the selected instance (#930 PR 4).
	KeyCloseTab // CloseTab closes the active (non-agent) tab (#930 PR 4).
	KeyJumpTab  // JumpTab is the 1-9 number-key jump hint; dispatched manually, menu/help only.

	KeyNewRemote // Key for creating a new remote instance
	KeyHelp      // Key for showing help screen

	// Diff keybindings
	KeyShiftUp
	KeyShiftDown

	KeyTaskList

	KeySearch // Key for searching sessions

	KeyOpenPR // Key for opening PR in browser
	KeyCopyPR // Key for copying PR URL to clipboard
	KeyHooks  // Key for editing post-worktree hooks

	KeyChangeProgram // Key for changing the program during new instance naming

	// Sidebar navigation
	KeyLeft        // Collapse section / move to parent
	KeyRight       // Expand section
	KeyNextSection // Jump to next section header
	KeyPrevSection // Jump to previous section header

	// Split view (#1024 PR 5). One physical key, focus-dependent verbs: with
	// tree focus `s` opens/retargets the selection in pane B, with pane focus
	// it swaps A↔B. KeySwapPanes exists for menu/help display of the swap
	// meaning only (GlobalKeyStringsMap can map "s" to a single name);
	// dispatch always resolves "s" to KeySplit and the root model picks the
	// verb off the focus ring.
	KeySplit      // Split: open the tree selection in pane B / swap panes.
	KeySwapPanes  // SwapPanes is the pane-focused display alias of KeySplit; menu/help only.
	KeyCloseSplit // CloseSplit closes the split (pane B focused).

	// KeyManageAutomations is the automations-section display alias of Enter
	// (menu/help only): with the in-rail section focused, Enter opens the task
	// manager overlay. Dispatch is root-routed (handleAutomationsFocus), so
	// this name never appears in GlobalKeyStringsMap.
	KeyManageAutomations
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
	"t":          KeyNewTab,
	"w":          KeyCloseTab,
	"?":          KeyHelp,
	// "s" is the split verb since #1024 PR 5 (RFC §2.3); the task-create jump
	// it used to carry lives in the automations strip's own manager (S → n).
	"s":     KeySplit,
	"x":     KeyCloseSplit,
	"S":     KeyTaskList,
	"/":     KeySearch,
	"p":     KeyOpenPR,
	"P":     KeyCopyPR,
	"H":     KeyHooks,
	"h":     KeyLeft,
	"left":  KeyLeft,
	"l":     KeyRight,
	"right": KeyRight,
	"]":     KeyNextSection,
	"[":     KeyPrevSection,
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
		key.WithHelp("⇧↑", "scroll"),
	),
	KeyShiftDown: key.NewBinding(
		key.WithKeys("shift+down"),
		key.WithHelp("⇧↓", "scroll"),
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
		key.WithHelp("tab", "focus"),
	),
	KeyShiftTab: key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "focus prev"),
	),
	KeyNewTab: key.NewBinding(
		key.WithKeys("t"),
		key.WithHelp("t", "tab"),
	),
	KeyCloseTab: key.NewBinding(
		key.WithKeys("w"),
		key.WithHelp("w", "close"),
	),
	KeyJumpTab: key.NewBinding(
		key.WithKeys("1", "2", "3", "4", "5", "6", "7", "8", "9"),
		key.WithHelp("1-9", "jump"),
	),
	KeyTaskList: key.NewBinding(
		key.WithKeys("S"),
		key.WithHelp("S", "tasks"),
	),
	KeyManageAutomations: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("↵", "manage"),
	),
	KeySplit: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "split"),
	),
	KeySwapPanes: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "swap"),
	),
	KeyCloseSplit: key.NewBinding(
		key.WithKeys("x"),
		key.WithHelp("x", "close split"),
	),
	KeySearch: key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "search"),
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
