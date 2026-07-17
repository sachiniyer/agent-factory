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
	// InPlaceCount is how many of SessionCount's live sessions sit on an
	// in-place/external worktree (`af sessions create --here`, the root agent).
	// Delete-project cannot archive those — it tears them down (#1973) — so the
	// confirmation must state the real archived-vs-torn-down split before the
	// user consents. Derived from the same cross-repo snapshot as SessionCount.
	InPlaceCount int
}

// ProjectPickerOverlay is the project switcher (#1461). It navigates like the
// instances rail — a windowed list over ALL projects with up/k and down/j
// cursor movement, Enter to switch, Esc to cancel; there is no search. It adds a
// trailing "+ Add project…" affordance: selecting the add row switches the
// overlay into add mode, where a repo-path input line appears. Path validation
// and registration happen app-side (this package must not shell out to git), so
// add mode reports the entered path back to the caller, which validates and
// either switches or feeds an inline error back via SetAddError.
type ProjectPickerOverlay struct {
	all []Project

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
	for i, proj := range p.all {
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
	if p.submitted && p.selectedIdx >= 0 && p.selectedIdx < len(p.all) {
		return p.all[p.selectedIdx], true
	}
	return Project{}, false
}

// rowCount is the number of navigable rows: every project plus the trailing
// "+ Add project…" row, which is always present.
func (p *ProjectPickerOverlay) rowCount() int { return len(p.all) + 1 }

// addRowSelected reports whether the cursor is on the "+ Add project…" row.
func (p *ProjectPickerOverlay) addRowSelected() bool { return p.selectedIdx == len(p.all) }

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
	// The picker navigates like the instances rail: up/k and down/j move the
	// cursor over the full list (clamped, no wrap), Enter switches, Esc cancels.
	// There is no search — any other key (typed letters, etc.) is ignored.
	switch msg.String() {
	case "esc", "ctrl+c":
		p.canceled = true
		return true
	case "enter":
		if p.addRowSelected() {
			p.adding = true
			p.addErr = ""
			return false
		}
		if len(p.all) > 0 {
			p.submitted = true
			return true
		}
	case "up", "k":
		if p.selectedIdx > 0 {
			p.selectedIdx--
		}
	case "down", "j":
		if p.selectedIdx < p.rowCount()-1 {
			p.selectedIdx++
		}
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
	lines = append(lines, truncateOverlayLine(titleStyle.Render("Switch project"), cw))
	lines = append(lines, "")

	if p.adding {
		lines = append(lines, truncateOverlayLine(normalStyle.Render("Add project — enter a repo path:"), cw))
		lines = append(lines, truncateOverlayLine("  "+queryStyle.Render(p.pathInput)+ui.InputCaret(), cw))
		if p.addErr != "" {
			lines = append(lines, truncateOverlayLine(errStyle.Render("  "+p.addErr), cw))
		}
		lines = append(lines, "")
		hint := "enter add • esc back"
		lines = append(lines, truncateOverlayLine(hintStyle.Render(hint), cw))
		return finishRender(style, fit, textRect, lines)
	}

	// Reserve rows for the fixed chrome (title, blank, blank, hint) and window
	// the navigable rows into what remains.
	avail := textRect.H - 4
	if avail < 1 {
		avail = 1
	}
	start, end := p.visibleWindow(avail)
	if start > 0 {
		lines = append(lines, truncateOverlayLine(normalStyle.Render(fmt.Sprintf("    … %d more above", start)), cw))
	}
	for i := start; i < end; i++ {
		lines = append(lines, truncateOverlayLine(p.renderRow(i, selectedStyle, normalStyle, countStyle, addStyle), cw))
	}
	if end < p.rowCount() {
		lines = append(lines, truncateOverlayLine(normalStyle.Render(fmt.Sprintf("    … and %d more below", p.rowCount()-end)), cw))
	}

	lines = append(lines, "")
	hint := "j/k navigate • enter switch • esc cancel"
	if lipgloss.Width(hint) > cw {
		hint = "j/k • enter • esc"
	}
	lines = append(lines, truncateOverlayLine(hintStyle.Render(hint), cw))

	return finishRender(style, fit, textRect, lines)
}

// renderRow renders one navigable row: a project ("name (N)") or the trailing
// "+ Add project…" affordance, highlighting the selected one.
func (p *ProjectPickerOverlay) renderRow(i int, selectedStyle, normalStyle, countStyle, addStyle lipgloss.Style) string {
	selected := i == p.selectedIdx
	if i == len(p.all) {
		text := "+ Add project…"
		if selected {
			return "  " + selectedStyle.Render("▸ "+text)
		}
		return "    " + addStyle.Render(text)
	}
	proj := p.all[i]
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
