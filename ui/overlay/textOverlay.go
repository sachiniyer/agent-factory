package overlay

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
)

// TextOverlay represents a text screen overlay
type TextOverlay struct {
	// Whether the overlay has been dismissed
	Dismissed bool
	// OnDismiss is invoked once when the user dismisses the overlay. It may
	// return a tea.Cmd that callers should feed back into the bubbletea event
	// loop — used by the attach-overlay path (#579) so the post-detach
	// goroutine can dispatch an immediate repaintAfterDetachMsg{} instead of
	// waiting up to one previewTickMsg cycle (~100ms) for the next paint.
	OnDismiss func() tea.Cmd
	// Content to display in the overlay
	content string

	width int
}

// NewTextOverlay creates a new text screen overlay with the given title and content
func NewTextOverlay(content string) *TextOverlay {
	return &TextOverlay{
		Dismissed: false,
		content:   content,
		// Default width so PlaceOverlay can center/fade on narrow terminals.
		// Callers should invoke SetWidth once the actual terminal size is known.
		width: 60,
	}
}

// HandleKeyPress processes a key press and updates the state. Returns the
// caller-supplied OnDismiss cmd (if any) so the bubbletea Update path can
// feed it into tea.Batch, plus true to indicate the overlay should close.
func (t *TextOverlay) HandleKeyPress(msg tea.KeyMsg) (tea.Cmd, bool) {
	// Close on any key
	t.Dismissed = true
	var cmd tea.Cmd
	if t.OnDismiss != nil {
		cmd = t.OnDismiss()
	}
	return cmd, true
}

// Render renders the text overlay
func (t *TextOverlay) Render(opts ...WhitespaceOption) string {
	// Create styles
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ui.AccentColor).
		Padding(1, 2).
		Width(t.width)

	// Apply the border style and return
	return style.Render(t.content)
}

func (t *TextOverlay) SetWidth(width int) {
	t.width = width
}
