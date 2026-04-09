package overlay

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/session"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SearchResult holds a matched instance and its index in the original list.
type SearchResult struct {
	Instance *session.Instance
	Index    int
}

// SearchOverlay provides fuzzy search across sessions.
type SearchOverlay struct {
	query   string
	results []SearchResult
	all     []*session.Instance

	selectedIdx int
	submitted   bool
	closed      bool
	width       int
}

// NewSearchOverlay creates a search overlay with the given instances.
func NewSearchOverlay(instances []*session.Instance) *SearchOverlay {
	s := &SearchOverlay{
		all:   instances,
		width: 60,
	}
	s.updateResults()
	return s
}

// SetWidth sets the overlay width.
func (s *SearchOverlay) SetWidth(width int) {
	s.width = width
}

// IsClosed returns true if the overlay should be dismissed.
func (s *SearchOverlay) IsClosed() bool {
	return s.closed
}

// IsSubmitted returns true if the user selected a result.
func (s *SearchOverlay) IsSubmitted() bool {
	return s.submitted
}

// GetSelectedInstance returns the instance the user selected, or nil.
func (s *SearchOverlay) GetSelectedInstance() *session.Instance {
	if s.submitted && len(s.results) > 0 && s.selectedIdx < len(s.results) {
		return s.results[s.selectedIdx].Instance
	}
	return nil
}

// HandleKeyPress processes input. Returns true if the overlay should close.
func (s *SearchOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc:
		s.closed = true
		return true
	case tea.KeyCtrlC:
		s.closed = true
		return true
	case tea.KeyEnter:
		if len(s.results) > 0 {
			s.submitted = true
			s.closed = true
			return true
		}
	case tea.KeyUp:
		if s.selectedIdx > 0 {
			s.selectedIdx--
		}
	case tea.KeyDown:
		if s.selectedIdx < len(s.results)-1 {
			s.selectedIdx++
		}
	case tea.KeyBackspace:
		if len(s.query) > 0 {
			runes := []rune(s.query)
			s.query = string(runes[:len(runes)-1])
			s.updateResults()
		}
	case tea.KeySpace:
		s.query += " "
		s.updateResults()
	case tea.KeyRunes:
		s.query += string(msg.Runes)
		s.updateResults()
	}
	return false
}

func (s *SearchOverlay) updateResults() {
	s.results = nil
	query := strings.ToLower(s.query)

	for i, inst := range s.all {
		if s.matches(inst, query) {
			s.results = append(s.results, SearchResult{Instance: inst, Index: i})
		}
	}

	// Clamp selection
	if s.selectedIdx >= len(s.results) {
		s.selectedIdx = len(s.results) - 1
	}
	if s.selectedIdx < 0 {
		s.selectedIdx = 0
	}
}

func (s *SearchOverlay) matches(inst *session.Instance, query string) bool {
	if query == "" {
		return true
	}
	title := strings.ToLower(inst.Title)
	branch := strings.ToLower(inst.Branch)

	// Simple fuzzy: check if all query chars appear in order in the title or branch
	return fuzzyMatch(query, title) || fuzzyMatch(query, branch)
}

// fuzzyMatch returns true if all characters in pattern appear in str in order.
func fuzzyMatch(pattern, str string) bool {
	pIdx := 0
	for sIdx := 0; sIdx < len(str) && pIdx < len(pattern); sIdx++ {
		if str[sIdx] == pattern[pIdx] {
			pIdx++
		}
	}
	return pIdx == len(pattern)
}

// Render renders the search overlay.
func (s *SearchOverlay) Render(opts ...WhitespaceOption) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	queryStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF79C6"))
	statusRunning := lipgloss.NewStyle().Foreground(lipgloss.Color("#51bd73"))
	statusReady := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFCC00"))
	statusLoading := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))

	content := titleStyle.Render("Search Sessions") + "\n\n"
	content += "/ " + queryStyle.Render(s.query+"_") + "\n\n"

	if len(s.results) == 0 {
		if s.query == "" {
			content += normalStyle.Render("  Type to search...") + "\n"
		} else {
			content += normalStyle.Render("  No matches found.") + "\n"
		}
	}

	maxVisible := 10

	// Compute the visible window so it always includes the selected item.
	startIdx := 0
	if s.selectedIdx >= maxVisible {
		startIdx = s.selectedIdx - maxVisible + 1
	}
	endIdx := startIdx + maxVisible
	if endIdx > len(s.results) {
		endIdx = len(s.results)
	}

	if startIdx > 0 {
		content += normalStyle.Render(
			fmt.Sprintf("    ... %d more above", startIdx)) + "\n"
	}

	for i := startIdx; i < endIdx; i++ {
		r := s.results[i]

		// Status indicator
		var statusStr string
		switch r.Instance.Status {
		case session.Running:
			statusStr = statusRunning.Render("●")
		case session.Ready:
			statusStr = statusReady.Render("●")
		case session.Loading:
			statusStr = statusLoading.Render("○")
		default:
			statusStr = normalStyle.Render("○")
		}

		label := r.Instance.Title
		if r.Instance.Branch != "" {
			label += normalStyle.Render(" (" + r.Instance.Branch + ")")
		}

		if i == s.selectedIdx {
			content += "  " + statusStr + " " + selectedStyle.Render("▸ "+r.Instance.Title)
			if r.Instance.Branch != "" {
				content += normalStyle.Render(" (" + r.Instance.Branch + ")")
			}
			content += "\n"
		} else {
			content += "  " + statusStr + " " + normalStyle.Render("  "+label) + "\n"
		}
	}

	if endIdx < len(s.results) {
		remaining := len(s.results) - endIdx
		content += normalStyle.Render(
			fmt.Sprintf("    ... and %d more below", remaining)) + "\n"
	}

	content += "\n"
	content += hintStyle.Render("↑/↓ navigate • enter select • esc close")

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(1, 2).
		Width(s.width)

	return style.Render(content)
}
