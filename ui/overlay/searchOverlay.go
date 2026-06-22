package overlay

import (
	"fmt"
	"unicode"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/text/unicode/norm"
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

	for i, inst := range s.all {
		if s.matches(inst, s.query) {
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

	// Simple fuzzy: check if all query chars appear in order in the title or branch
	return fuzzyMatch(query, inst.Title) || fuzzyMatch(query, inst.GetBranch())
}

// fuzzyMatch returns true if all runes in pattern appear in str in order,
// ignoring case. Both strings are NFC-normalized first so canonically
// equivalent input (e.g. a decomposed "é" typed on macOS vs a composed one
// from copy-paste) still matches.
func fuzzyMatch(pattern, str string) bool {
	patternRunes := []rune(norm.NFC.String(pattern))
	if len(patternRunes) == 0 {
		return true
	}
	pIdx := 0
	for _, r := range norm.NFC.String(str) {
		if runeEqualFold(r, patternRunes[pIdx]) {
			pIdx++
			if pIdx == len(patternRunes) {
				return true
			}
		}
	}
	return false
}

// runeEqualFold reports whether two runes are equal under Unicode simple
// case folding, mirroring strings.EqualFold one rune at a time.
func runeEqualFold(a, b rune) bool {
	if a == b {
		return true
	}
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

// Render renders the search overlay.
func (s *SearchOverlay) Render(opts ...WhitespaceOption) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ui.AccentColor)
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
		switch r.Instance.GetStatus() {
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
		branch := r.Instance.GetBranch()
		if branch != "" {
			label += normalStyle.Render(" (" + branch + ")")
		}

		if i == s.selectedIdx {
			content += "  " + statusStr + " " + selectedStyle.Render("▸ "+r.Instance.Title)
			if branch != "" {
				content += normalStyle.Render(" (" + branch + ")")
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
		BorderForeground(ui.AccentColor).
		Padding(1, 2).
		Width(s.width)

	return style.Render(content)
}
