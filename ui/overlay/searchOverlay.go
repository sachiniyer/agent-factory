package overlay

import (
	"fmt"
	"strings"
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
	if maxVisible < 1 {
		maxVisible = 1
	}
	if s.selectedIdx >= maxVisible {
		startIdx = s.selectedIdx - maxVisible + 1
	}
	endIdx = startIdx + maxVisible
	if endIdx > len(s.results) {
		endIdx = len(s.results)
	}
	return startIdx, endIdx
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
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ui.CurrentTheme().Accent).
		Padding(1, 2)
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
		startIdx, endIdx, showAbove, showBelow := s.windowForAvailableRows(availableRows)
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

func (s *SearchOverlay) windowForAvailableRows(available int) (startIdx, endIdx int, showAbove, showBelow bool) {
	if len(s.results) == 0 {
		return 0, 0, false, false
	}
	if available <= 0 {
		available = 10
	}
	rows := available
	if rows > 10 {
		rows = 10
	}
	for rows > 0 {
		startIdx, endIdx = s.visibleWindowForRows(rows)
		showAbove = startIdx > 0
		showBelow = endIdx < len(s.results)
		need := endIdx - startIdx
		if showAbove {
			need++
		}
		if showBelow {
			need++
		}
		if need <= available {
			return startIdx, endIdx, showAbove, showBelow
		}
		rows--
	}
	startIdx, endIdx = s.visibleWindowForRows(1)
	return startIdx, endIdx, false, false
}

// Render renders the search overlay.
func (s *SearchOverlay) Render() string {
	t := ui.CurrentTheme()
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Warning)
	normalStyle := lipgloss.NewStyle().Foreground(t.ForegroundMuted)
	hintStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	queryStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Purple)
	statusRunning := lipgloss.NewStyle().Foreground(t.Success)
	statusReady := lipgloss.NewStyle().Foreground(t.Warning)
	statusLoading := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	// statusLimit marks a usage-limit-blocked result (#1146) with a distinct
	// warning red + diamond glyph so it never reads as a live Running/Ready dot.
	statusLimit := lipgloss.NewStyle().Foreground(t.Error)

	style := searchOverlayStyle()
	plan := s.renderPlan(style)

	var lines []string
	lines = append(lines, truncateOverlayLine(titleStyle.Render("Search Sessions"), plan.contentWidth))
	if !plan.compact {
		lines = append(lines, "")
	}
	lines = append(lines, truncateOverlayLine("/ "+queryStyle.Render(s.query+"_"), plan.contentWidth))
	if !plan.compact {
		lines = append(lines, "")
	}

	if len(s.results) == 0 {
		if s.query == "" {
			lines = append(lines, truncateOverlayLine(normalStyle.Render("  Type to search..."), plan.contentWidth))
		} else {
			lines = append(lines, truncateOverlayLine(normalStyle.Render("  No matches found."), plan.contentWidth))
		}
	}

	if plan.showAbove {
		lines = append(lines, truncateOverlayLine(normalStyle.Render(
			fmt.Sprintf("    ... %d more above", plan.startIdx)), plan.contentWidth))
	}

	for i := plan.startIdx; i < plan.endIdx; i++ {
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
			line := "  " + statusStr + " " + selectedStyle.Render("▸ "+r.Instance.Title)
			if branch != "" {
				line += normalStyle.Render(" (" + branch + ")")
			}
			lines = append(lines, truncateOverlayLine(line, plan.contentWidth))
		} else {
			lines = append(lines, truncateOverlayLine("  "+statusStr+" "+normalStyle.Render("  "+label), plan.contentWidth))
		}
	}

	if plan.showBelow {
		remaining := len(s.results) - plan.endIdx
		lines = append(lines, truncateOverlayLine(normalStyle.Render(
			fmt.Sprintf("    ... and %d more below", remaining)), plan.contentWidth))
	}

	if !plan.compact {
		lines = append(lines, "")
	}
	hint := "↑/↓ navigate • enter select • esc close"
	if plan.compact || lipgloss.Width(hint) > plan.contentWidth {
		hint = "↑/↓ nav • enter • esc close"
	}
	lines = append(lines, truncateOverlayLine(hintStyle.Render(hint), plan.contentWidth))

	style = style.Width(plan.styleWidth)
	if plan.styleHeight > 0 && len(lines) >= plan.contentHeight {
		style = style.Height(plan.styleHeight)
	}
	return style.Render(strings.Join(lines, "\n"))
}
