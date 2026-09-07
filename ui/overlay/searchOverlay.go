package overlay

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui/layout"
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
	maxWidth    int
	maxHeight   int
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

// SetMaxSize sets the maximum outer size the rendered search overlay may occupy.
func (s *SearchOverlay) SetMaxSize(width, height int) {
	s.maxWidth = width
	s.maxHeight = height
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

// visibleWindowForRows returns the [start, end) result window Render shows:
// at most maxVisible rows, slid so the selected item is always included.
// Shared with the mouse zone registration (zones.go) so the rows registered
// are exactly the rows rendered.
func (s *SearchOverlay) visibleWindowForRows(maxVisible int) (startIdx, endIdx int) {
	return selectionWindow(s.selectedIdx, len(s.results), maxVisible)
}

type searchRenderPlan struct {
	styleWidth    int
	styleHeight   int
	contentWidth  int
	contentHeight int
	compact       bool
	startIdx      int
	endIdx        int
	showAbove     bool
	showBelow     bool
}

func searchOverlayStyle() lipgloss.Style {
	return ui.DialogStyle()
}

func (s *SearchOverlay) renderPlan(style lipgloss.Style) searchRenderPlan {
	fit := fitOverlayContent(s.width, 0, s.maxWidth, s.maxHeight, style)
	if fit.W <= 0 {
		fit.W = s.width
	}
	if fit.W <= 0 {
		fit.W = 1
	}
	textRect := overlayTextRect(fit, style)
	compact := textRect.H > 0 && textRect.H <= 8
	baseRows := 6
	if compact {
		baseRows = 3
	}
	availableRows := 10
	if textRect.H > 0 {
		availableRows = textRect.H - baseRows
		if len(s.results) == 0 {
			availableRows = 0
		} else if availableRows < 1 {
			availableRows = 1
		}
		startIdx, endIdx, showAbove, showBelow := budgetedSelectionWindow(s.selectedIdx, len(s.results), availableRows, 10)
		return searchRenderPlan{
			styleWidth:    fit.W,
			styleHeight:   fit.H,
			contentWidth:  textRect.W,
			contentHeight: textRect.H,
			compact:       compact,
			startIdx:      startIdx,
			endIdx:        endIdx,
			showAbove:     showAbove,
			showBelow:     showBelow,
		}
	}

	startIdx, endIdx := s.visibleWindowForRows(availableRows)
	return searchRenderPlan{
		styleWidth:    fit.W,
		styleHeight:   fit.H,
		contentWidth:  textRect.W,
		contentHeight: textRect.H,
		compact:       compact,
		startIdx:      startIdx,
		endIdx:        endIdx,
		showAbove:     startIdx > 0,
		showBelow:     endIdx < len(s.results),
	}
}

// Render renders the search overlay.
func (s *SearchOverlay) Render() string {
	frame, _, _ := s.renderFrame()
	return frame
}

// renderFrame returns the exact first result row along with the same plan that
// painted it; pointer zones never infer row identity from user-controlled text.
func (s *SearchOverlay) renderFrame() (string, int, searchRenderPlan) {
	t := ui.CurrentTheme()
	titleStyle := ui.DialogTitleStyle()
	selectedStyle := lipgloss.NewStyle().Bold(true).Background(t.SurfaceRaised).Foreground(t.Ink)
	normalStyle := lipgloss.NewStyle().Foreground(t.Ink)
	hintStyle := ui.DialogHintStyle()
	overflowStyle := lipgloss.NewStyle().Foreground(t.InkMuted)
	queryStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Ink)

	style := searchOverlayStyle()
	plan := s.renderPlan(style)

	var lines []string
	lines = append(lines, truncateOverlayLine(titleStyle.Render("Search sessions"), plan.contentWidth))
	if !plan.compact {
		lines = append(lines, "")
	}
	lines = append(lines, truncateOverlayLine("/ "+queryStyle.Render(s.query)+ui.InputCaret(), plan.contentWidth))
	if !plan.compact {
		lines = append(lines, "")
	}

	if len(s.results) == 0 {
		if s.query == "" {
			lines = append(lines, truncateOverlayLine(normalStyle.Render("  type to search…"), plan.contentWidth))
		} else {
			lines = append(lines, truncateOverlayLine(normalStyle.Render("  No matches found"), plan.contentWidth))
		}
	}

	if plan.showAbove {
		lines = append(lines, truncateOverlayLine(overflowStyle.Render(
			fmt.Sprintf("    … %d more above", plan.startIdx)), plan.contentWidth))
	}

	firstResultLine := len(lines)
	for i := plan.startIdx; i < plan.endIdx; i++ {
		r := s.results[i]

		// Working, in-flight and unset states reserve a blank status cell.
		// Otherwise use the fixed liveness glyph, independent of colour.
		statusStyle := lipgloss.NewStyle().Foreground(t.Ink)
		glyph := " "
		if r.Instance.GetInFlightOp() == session.OpNone {
			switch r.Instance.GetLiveness() {
			case session.LiveRunning, session.LivenessUnset:
			case session.LiveReady:
				glyph, statusStyle = "●", statusStyle.Foreground(t.Ready)
			case session.LiveLimitReached:
				glyph, statusStyle = "◆", statusStyle.Foreground(t.LimitReached)
			case session.LiveLost:
				glyph, statusStyle = "◌", statusStyle.Foreground(t.Lost)
			case session.LiveDead:
				glyph, statusStyle = "○", statusStyle.Foreground(t.Dead)
			case session.LiveArchived:
				glyph, statusStyle = "▧", statusStyle.Foreground(t.Archived)
			}
		}
		if i == s.selectedIdx {
			statusStyle = statusStyle.Background(t.SurfaceRaised)
		}
		statusStr := statusStyle.Render(glyph + " ")

		label := r.Instance.Title
		branch := r.Instance.GetBranch()
		if branch != "" {
			label += hintStyle.Render(" (" + branch + ")")
		}

		if i == s.selectedIdx {
			line := "  " + statusStr + ui.SelectionMarker("▸ ") + selectedStyle.Render(r.Instance.Title)
			if branch != "" {
				line += selectedStyle.Render(" (" + branch + ")")
			}
			lines = append(lines, truncateOverlayLine(line, plan.contentWidth))
		} else {
			lines = append(lines, truncateOverlayLine("  "+statusStr+normalStyle.Render("  "+label), plan.contentWidth))
		}
	}

	if plan.showBelow {
		remaining := len(s.results) - plan.endIdx
		lines = append(lines, truncateOverlayLine(overflowStyle.Render(
			fmt.Sprintf("    … and %d more below", remaining)), plan.contentWidth))
	}

	if !plan.compact {
		lines = append(lines, "")
	}
	hint := "↑/↓ select · enter open · esc close"
	if plan.compact || layout.Cells(hint) > plan.contentWidth {
		hint = "↑/↓ nav · enter · esc close"
	}
	lines = append(lines, truncateOverlayLine(ui.ActionHint(hint), plan.contentWidth))

	style = style.Width(plan.styleWidth)
	if plan.styleHeight > 0 && len(lines) >= plan.contentHeight {
		style = style.Height(plan.styleHeight)
	}
	return ui.RenderDialog(style, strings.Join(lines, "\n")), style.GetBorderTopSize() + style.GetPaddingTop() + firstResultLine, plan
}
