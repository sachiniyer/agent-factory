package overlay

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Project is one selectable entry in the project picker: a repo af has seen,
// with its display name (repo basename), absolute main-worktree root, and the
// number of sessions currently tracked for it.
type Project struct {
	Name         string
	Root         string
	SessionCount int
}

// ProjectPickerOverlay is the searchable project switcher (#1461). It mirrors
// SearchOverlay's idiom — a hand-rolled query buffer, fuzzy filter, windowed
// list, Enter/Esc handling — and adds an "+ Add project…" affordance: selecting
// the trailing add row switches the overlay into add mode, where the query line
// becomes a repo-path input. Path validation and registration happen app-side
// (this package must not shell out to git), so add mode reports the entered path
// back to the caller, which validates and either switches or feeds an inline
// error back via SetAddError.
type ProjectPickerOverlay struct {
	all     []Project
	results []Project
	query   string

	selectedIdx int
	width       int
	maxWidth    int
	maxHeight   int

	// submitted is set when Enter chose an existing project row (a switch).
	submitted bool
	// canceled is set when Esc dismissed the picker from list mode.
	canceled bool

	// adding is true while the add-project path input is active.
	adding    bool
	pathInput string
	addErr    string
	// addRequested carries a submitted add-mode path for the caller to validate;
	// TakeAddRequest consumes it. Kept separate from submitted so the caller can
	// reject an invalid path (SetAddError) and keep the overlay open.
	addRequested bool
}

// NewProjectPickerOverlay creates a picker over the given projects (already
// sorted by the caller). currentRoot, when non-empty, pre-selects the row for
// the active project so the highlight starts on "where you are".
func NewProjectPickerOverlay(projects []Project, currentRoot string) *ProjectPickerOverlay {
	p := &ProjectPickerOverlay{
		all:   projects,
		width: 60,
	}
	p.updateResults()
	for i, proj := range p.results {
		if proj.Root == currentRoot {
			p.selectedIdx = i
			break
		}
	}
	return p
}

// SetWidth sets the overlay width.
func (p *ProjectPickerOverlay) SetWidth(width int) { p.width = width }

// SetMaxSize sets the maximum outer size the rendered overlay may occupy.
func (p *ProjectPickerOverlay) SetMaxSize(width, height int) {
	p.maxWidth = width
	p.maxHeight = height
}

// IsSubmitted reports whether the user chose an existing project (a switch).
func (p *ProjectPickerOverlay) IsSubmitted() bool { return p.submitted }

// SelectedProject returns the project the user chose, or false when none was.
func (p *ProjectPickerOverlay) SelectedProject() (Project, bool) {
	if p.submitted && p.selectedIdx >= 0 && p.selectedIdx < len(p.results) {
		return p.results[p.selectedIdx], true
	}
	return Project{}, false
}

// rowCount is the number of navigable rows: the filtered projects plus the
// trailing "+ Add project…" row, which is always present.
func (p *ProjectPickerOverlay) rowCount() int { return len(p.results) + 1 }

// addRowSelected reports whether the cursor is on the "+ Add project…" row.
func (p *ProjectPickerOverlay) addRowSelected() bool { return p.selectedIdx == len(p.results) }

// TakeAddRequest returns a submitted add-mode path once, clearing the pending
// flag so the caller validates it exactly once. The caller registers + switches
// on success, or calls SetAddError to surface an inline error and keep the
// overlay open.
func (p *ProjectPickerOverlay) TakeAddRequest() (string, bool) {
	if !p.addRequested {
		return "", false
	}
	p.addRequested = false
	return strings.TrimSpace(p.pathInput), true
}

// SetAddError shows an inline error under the add-mode input and keeps the
// overlay open so the user can correct the path.
func (p *ProjectPickerOverlay) SetAddError(msg string) { p.addErr = msg }

// HandleKeyPress processes input. Returns true if the overlay should close.
func (p *ProjectPickerOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	if p.adding {
		return p.handleAddKey(msg)
	}
	return p.handleListKey(msg)
}

func (p *ProjectPickerOverlay) handleListKey(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		p.canceled = true
		return true
	case tea.KeyEnter:
		if p.addRowSelected() {
			p.adding = true
			p.addErr = ""
			return false
		}
		if len(p.results) > 0 {
			p.submitted = true
			return true
		}
	case tea.KeyUp:
		if p.selectedIdx > 0 {
			p.selectedIdx--
		}
	case tea.KeyDown:
		if p.selectedIdx < p.rowCount()-1 {
			p.selectedIdx++
		}
	case tea.KeyBackspace:
		if len(p.query) > 0 {
			runes := []rune(p.query)
			p.query = string(runes[:len(runes)-1])
			p.updateResults()
		}
	case tea.KeySpace:
		p.query += " "
		p.updateResults()
	case tea.KeyRunes:
		p.query += string(msg.Runes)
		p.updateResults()
	}
	return false
}

func (p *ProjectPickerOverlay) handleAddKey(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		// Back out of add mode to the list rather than closing the picker.
		p.adding = false
		p.pathInput = ""
		p.addErr = ""
		return false
	case tea.KeyEnter:
		if strings.TrimSpace(p.pathInput) != "" {
			p.addRequested = true
		}
		return false
	case tea.KeyBackspace:
		if len(p.pathInput) > 0 {
			runes := []rune(p.pathInput)
			p.pathInput = string(runes[:len(runes)-1])
			p.addErr = ""
		}
	case tea.KeySpace:
		p.pathInput += " "
		p.addErr = ""
	case tea.KeyRunes:
		p.pathInput += string(msg.Runes)
		p.addErr = ""
	}
	return false
}

func (p *ProjectPickerOverlay) updateResults() {
	p.results = nil
	for _, proj := range p.all {
		if p.matches(proj, p.query) {
			p.results = append(p.results, proj)
		}
	}
	// Clamp selection into the row range (projects + add row).
	if p.selectedIdx >= p.rowCount() {
		p.selectedIdx = p.rowCount() - 1
	}
	if p.selectedIdx < 0 {
		p.selectedIdx = 0
	}
}

func (p *ProjectPickerOverlay) matches(proj Project, query string) bool {
	if query == "" {
		return true
	}
	return fuzzyMatch(query, proj.Name) || fuzzyMatch(query, proj.Root)
}

// visibleWindow returns the [start, end) window over the navigable rows Render
// shows: at most maxVisible rows, slid so the selected row is always included.
func (p *ProjectPickerOverlay) visibleWindow(maxVisible int) (startIdx, endIdx int) {
	if maxVisible < 1 {
		maxVisible = 1
	}
	total := p.rowCount()
	if p.selectedIdx >= maxVisible {
		startIdx = p.selectedIdx - maxVisible + 1
	}
	endIdx = startIdx + maxVisible
	if endIdx > total {
		endIdx = total
	}
	return startIdx, endIdx
}

// Render renders the project picker overlay.
func (p *ProjectPickerOverlay) Render() string {
	t := ui.CurrentTheme()
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Warning)
	normalStyle := lipgloss.NewStyle().Foreground(t.ForegroundMuted)
	hintStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	queryStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Purple)
	countStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	addStyle := lipgloss.NewStyle().Foreground(t.Success)
	errStyle := lipgloss.NewStyle().Foreground(t.Error)

	style := searchOverlayStyle()
	fit := fitOverlayContent(p.width, 0, p.maxWidth, p.maxHeight, style)
	if fit.W <= 0 {
		fit.W = p.width
	}
	if fit.W <= 0 {
		fit.W = 1
	}
	textRect := overlayTextRect(fit, style)
	cw := textRect.W

	var lines []string
	lines = append(lines, truncateOverlayLine(titleStyle.Render("Switch Project"), cw))
	lines = append(lines, "")

	if p.adding {
		lines = append(lines, truncateOverlayLine(normalStyle.Render("Add project — enter a repo path:"), cw))
		lines = append(lines, truncateOverlayLine("  "+queryStyle.Render(p.pathInput+"_"), cw))
		if p.addErr != "" {
			lines = append(lines, truncateOverlayLine(errStyle.Render("  "+p.addErr), cw))
		}
		lines = append(lines, "")
		hint := "enter add • esc back"
		lines = append(lines, truncateOverlayLine(hintStyle.Render(hint), cw))
		return finishRender(style, fit, textRect, lines)
	}

	lines = append(lines, truncateOverlayLine("/ "+queryStyle.Render(p.query+"_"), cw))
	lines = append(lines, "")

	// Reserve rows for the fixed chrome (title, blank, query, blank, blank,
	// hint) and window the navigable rows into what remains.
	avail := textRect.H - 6
	if avail < 1 {
		avail = 1
	}
	start, end := p.visibleWindow(avail)
	if start > 0 {
		lines = append(lines, truncateOverlayLine(normalStyle.Render(fmt.Sprintf("    ... %d more above", start)), cw))
	}
	for i := start; i < end; i++ {
		lines = append(lines, truncateOverlayLine(p.renderRow(i, selectedStyle, normalStyle, countStyle, addStyle), cw))
	}
	if end < p.rowCount() {
		lines = append(lines, truncateOverlayLine(normalStyle.Render(fmt.Sprintf("    ... and %d more below", p.rowCount()-end)), cw))
	}

	lines = append(lines, "")
	hint := "↑/↓ navigate • enter switch • esc close"
	if lipgloss.Width(hint) > cw {
		hint = "↑/↓ • enter • esc"
	}
	lines = append(lines, truncateOverlayLine(hintStyle.Render(hint), cw))

	return finishRender(style, fit, textRect, lines)
}

// renderRow renders one navigable row: a project ("name (N)") or the trailing
// "+ Add project…" affordance, highlighting the selected one.
func (p *ProjectPickerOverlay) renderRow(i int, selectedStyle, normalStyle, countStyle, addStyle lipgloss.Style) string {
	selected := i == p.selectedIdx
	if i == len(p.results) {
		text := "+ Add project…"
		if selected {
			return "  " + selectedStyle.Render("▸ "+text)
		}
		return "    " + addStyle.Render(text)
	}
	proj := p.results[i]
	count := countStyle.Render(fmt.Sprintf(" (%d)", proj.SessionCount))
	if selected {
		return "  " + selectedStyle.Render("▸ "+proj.Name) + count
	}
	return "    " + normalStyle.Render(proj.Name) + count
}

// finishRender sizes the style box and joins the content lines, matching the
// search overlay's sizing so both modals size identically.
func finishRender(style lipgloss.Style, fit, textRect layout.Rect, lines []string) string {
	style = style.Width(fit.W)
	if fit.H > 0 && len(lines) >= textRect.H {
		style = style.Height(fit.H)
	}
	return style.Render(strings.Join(lines, "\n"))
}
