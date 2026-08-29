package layout

import tea "github.com/charmbracelet/bubbletea"

// Pane is the contract every workspace region implements (RFC §2.2). The
// root model owns layout and routing; a pane only renders its rect and
// reacts to the input the root dispatches to it.
type Pane interface {
	// SetRect tells the pane where it lives; View must render exactly this
	// size (ClampToRect is the shared enforcement helper).
	SetRect(r Rect)

	Focused() bool
	Focus()
	Blur()

	// HandleKey processes a key when the pane is focused. The bool reports
	// whether the key was consumed; unconsumed keys bubble to the root's
	// global bindings.
	HandleKey(msg tea.KeyMsg) (tea.Cmd, bool)

	// View renders the pane, exactly Rect-sized (hard-clamped).
	View() string
}
