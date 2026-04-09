package overlay

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SelectionOverlay represents a selection list overlay for choosing from items
type SelectionOverlay struct {
	items       []string
	selectedIdx int
	title       string
	submitted   bool
	canceled    bool
	width       int
}

// NewSelectionOverlay creates a new selection overlay with the given title and items
func NewSelectionOverlay(title string, items []string) *SelectionOverlay {
	return &SelectionOverlay{
		items:       items,
		selectedIdx: 0,
		title:       title,
		width:       50,
	}
}

// HandleKeyPress processes a key press and updates the state.
// Returns true if the overlay should close.
func (s *SelectionOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "up", "k":
		if s.selectedIdx > 0 {
			s.selectedIdx--
		}
	case "down", "j":
		if s.selectedIdx < len(s.items)-1 {
			s.selectedIdx++
		}
	case "enter":
		s.submitted = true
		return true
	case "esc", "ctrl+c":
		s.canceled = true
		return true
	}
	return false
}

// GetSelectedIndex returns the index of the selected item
func (s *SelectionOverlay) GetSelectedIndex() int {
	return s.selectedIdx
}

// IsSubmitted returns true if the user confirmed a selection
func (s *SelectionOverlay) IsSubmitted() bool {
	return s.submitted
}

// IsCanceled returns true if the user canceled the selection
func (s *SelectionOverlay) IsCanceled() bool {
	return s.canceled
}

// SetSelectedIndex sets the initially selected item index
func (s *SelectionOverlay) SetSelectedIndex(idx int) {
	if idx >= 0 && idx < len(s.items) {
		s.selectedIdx = idx
	}
}

// SetWidth sets the width of the selection overlay
func (s *SelectionOverlay) SetWidth(width int) {
	s.width = width
}

// Render renders the selection overlay
func (s *SelectionOverlay) Render(opts ...WhitespaceOption) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))

	content := titleStyle.Render(s.title) + "\n\n"

	for i, item := range s.items {
		if i == s.selectedIdx {
			content += selectedStyle.Render("▸ "+item) + "\n"
		} else {
			content += normalStyle.Render("  "+item) + "\n"
		}
	}

	content += "\n" + hintStyle.Render("↑/↓ navigate • enter select • esc cancel")

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(1, 2).
		Width(s.width)

	return style.Render(content)
}
