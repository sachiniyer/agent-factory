package overlay

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
)

// SelectionOverlay represents a selection list overlay for choosing from items
type SelectionOverlay struct {
	items       []string
	selectedIdx int
	title       string
	submitted   bool
	canceled    bool
	width       int
	maxWidth    int
	maxHeight   int
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

// SetMaxSize sets the maximum outer size the rendered selection overlay may occupy.
func (s *SelectionOverlay) SetMaxSize(width, height int) {
	s.maxWidth = width
	s.maxHeight = height
}

// Render renders the selection overlay
func (s *SelectionOverlay) Render() string {
	t := ui.CurrentTheme()
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Warning)
	normalStyle := lipgloss.NewStyle().Foreground(t.ForegroundMuted)
	hintStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Accent).
		Padding(1, 2)
	fit := fitOverlayContent(s.width, 0, s.maxWidth, s.maxHeight, style)
	if fit.W <= 0 {
		fit.W = s.width
	}
	if fit.W <= 0 {
		fit.W = 1
	}
	textRect := overlayTextRect(fit, style)
	compact := textRect.H > 0 && textRect.H <= 8

	lines := []string{truncateOverlayLine(titleStyle.Render(s.title), textRect.W)}
	if !compact {
		lines = append(lines, "")
	}

	availableItems := len(s.items)
	if textRect.H > 0 {
		base := 4
		if compact {
			base = 2
		}
		availableItems = textRect.H - base
		if availableItems < 1 {
			availableItems = 1
		}
	}
	start, end := selectionWindow(s.selectedIdx, len(s.items), availableItems)
	if start > 0 {
		lines = append(lines, truncateOverlayLine(normalStyle.Render("  … more above"), textRect.W))
	}
	for i := start; i < end; i++ {
		item := s.items[i]
		if i == s.selectedIdx {
			lines = append(lines, truncateOverlayLine(selectedStyle.Render("▸ "+item), textRect.W))
		} else {
			lines = append(lines, truncateOverlayLine(normalStyle.Render("  "+item), textRect.W))
		}
	}
	if end < len(s.items) {
		lines = append(lines, truncateOverlayLine(normalStyle.Render("  … more below"), textRect.W))
	}

	if !compact {
		lines = append(lines, "")
	}
	// Keep the escape hatch at the right edge by choosing a whole hint that fits,
	// never by truncating a longer one. At narrow widths navigation detail drops
	// first, then navigation itself; `esc cancel` is the load-bearing last item.
	hint := "esc cancel"
	candidates := []string{
		"↑/↓ navigate · enter select · esc cancel",
		"↑/↓ nav · enter · esc cancel",
		"enter · esc cancel",
	}
	if compact {
		candidates = candidates[1:]
	}
	for _, candidate := range candidates {
		if lipgloss.Width(candidate) <= textRect.W {
			hint = candidate
			break
		}
	}
	lines = append(lines, truncateOverlayLine(hintStyle.Render(hint), textRect.W))

	style = style.Width(fit.W)
	if fit.H > 0 && len(lines) >= textRect.H {
		style = style.Height(fit.H)
	}
	return style.Render(strings.Join(lines, "\n"))
}

func selectionWindow(selected, total, maxVisible int) (start, end int) {
	if maxVisible < 1 {
		maxVisible = 1
	}
	if total <= maxVisible {
		return 0, total
	}
	if selected >= maxVisible {
		start = selected - maxVisible + 1
	}
	end = start + maxVisible
	if end > total {
		end = total
		start = end - maxVisible
		if start < 0 {
			start = 0
		}
	}
	return start, end
}
