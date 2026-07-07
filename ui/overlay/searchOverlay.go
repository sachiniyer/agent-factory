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

// SearchResult holds a matched instance.
type SearchResult struct {
	Instance *session.Instance
}

// SearchOverlay provides fuzzy search across sessions.
type SearchOverlay struct {
	query   string
	results []SearchResult
	all     []*session.Instance

	selectedIdx int
	submitted   bool
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

// IsSubmitted returns true if the user selected a result.
func (s *SearchOverlay) IsSubmitted() bool {
	return s.submitted
}

// ResultInstances returns the instances currently matching the query, in
// display order. Exposed for tests that assert the overlay's list stays a
// stable copy independent of later sidebar mutations (#1008).
func (s *SearchOverlay) ResultInstances() []*session.Instance {
	out := make([]*session.Instance, len(s.results))
	for i, r := range s.results {
		out[i] = r.Instance
	}
	return out
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
		return true
	case tea.KeyCtrlC:
		return true
	case tea.KeyEnter:
		if len(s.results) > 0 {
			s.submitted = true
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

	for _, inst := range s.all {
		if s.matches(inst, s.query) {
			s.results = append(s.results, SearchResult{Instance: inst})
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

// visibleWindow returns the [start, end) result window Render shows: at most
// maxVisible rows, slid so the selected item is always included. Shared with
// the mouse zone registration (zones.go) so the rows registered are exactly
// the rows rendered.
func (s *SearchOverlay) visibleWindow() (startIdx, endIdx int) {
	const maxVisible = 10
	if s.selectedIdx >= maxVisible {
		startIdx = s.selectedIdx - maxVisible + 1
	}
	endIdx = startIdx + maxVisible
	if endIdx > len(s.results) {
		endIdx = len(s.results)
	}
	return startIdx, endIdx
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
	// statusLimit marks a usage-limit-blocked result (#1146) with a distinct
	// warning red + diamond glyph so it never reads as a live Running/Ready dot.
	statusLimit := lipgloss.NewStyle().Foreground(lipgloss.Color("#E06C75"))

	content := titleStyle.Render("Search Sessions") + "\n\n"
	content += "/ " + queryStyle.Render(s.query+"_") + "\n\n"

	if len(s.results) == 0 {
		if s.query == "" {
			content += normalStyle.Render("  Type to search...") + "\n"
		} else {
			content += normalStyle.Render("  No matches found.") + "\n"
		}
	}

	startIdx, endIdx := s.visibleWindow()

	if startIdx > 0 {
		content += normalStyle.Render(
			fmt.Sprintf("    ... %d more above", startIdx)) + "\n"
	}

	for i := startIdx; i < endIdx; i++ {
		r := s.results[i]

		// Status indicator. Two axes (#1195): a create in flight reads as loading;
		// otherwise a total switch over the liveness — every value explicit (incl.
		// LimitReached, #1146, which gets its own red diamond so it never reads as a
		// live dot), no silent default. Running/Ready get the filled dot; every
		// other liveness gets the hollow ○.
		var statusStr string
		switch {
		case r.Instance.GetInFlightOp() == session.OpCreating:
			statusStr = statusLoading.Render("○")
		case r.Instance.GetInFlightOp() != session.OpNone:
			// A kill/archive teardown in flight — going away.
			statusStr = normalStyle.Render("○")
		default:
			switch r.Instance.GetLiveness() {
			case session.LiveRunning:
				statusStr = statusRunning.Render("●")
			case session.LiveReady:
				statusStr = statusReady.Render("●")
			case session.LiveLimitReached:
				// A usage-limit-blocked session (#1146) gets a distinct red diamond
				// so "blocked on limit" never reads as a live/gone dot.
				statusStr = statusLimit.Render("◆")
			case session.LiveLost, session.LiveDead, session.LiveArchived,
				session.LivenessUnset:
				statusStr = normalStyle.Render("○")
			}
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
