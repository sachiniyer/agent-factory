package overlay

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Project is one selectable entry in the project picker: a repo af has seen,
// with its display name (repo basename), absolute main-worktree root, and the
// number of sessions currently tracked for it.
type Project struct {
	// RepoID is the identity used to aggregate this row. Carrying it through to
	// the sidebar prevents destructive actions from re-resolving a stale display
	// root into an unrelated enclosing repository.
	RepoID       string
	Name         string
	Root         string
	SessionCount int
	// InPlaceCount is how many of SessionCount's live sessions sit on an
	// in-place/external worktree (`af sessions create --here`, the root agent).
	// Delete-project cannot archive those — it tears them down (#1973) — so the
	// confirmation must state the real archived-vs-torn-down split before the
	// user consents. Derived from the same cross-repo snapshot as SessionCount.
	InPlaceCount int
	// RegistryID is the durable registry id (prj_…) of the registration behind
	// this row, empty when no registry record claims it. It is what makes `b`
	// (rebind) meaningful: rebind moves a REGISTRATION's stable identity, so
	// the verb is offered only on registry-backed rows.
	RegistryID string
	// MissingPath marks a registry-backed row whose recorded root the registry
	// reports absent (path_exists=false) — the checkout moved or was recloned,
	// which is exactly what rebind repairs.
	MissingPath bool
}

// rebindTokens issues request tokens across every picker instance: a fresh
// picker must never reuse a token an older, closed one handed out.
var rebindTokens atomic.Uint64

// RebindRequest is one submitted rebind: the row it targets (whose RegistryID
// the daemon rebinds), the replacement path, and the token its reply must carry
// back to OwnsRebindReply.
type RebindRequest struct {
	Project Project
	Path    string
	Token   uint64
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

	// rebinding is true while the rebind-path input is active: like add mode,
	// but the request repairs an EXISTING registration — TakeRebindRequest
	// hands back the row so the caller can send its RegistryID — rather than
	// creating one.
	rebinding   bool
	rebindInput string
	rebindErr   string
	// rebindRequested carries a submitted rebind-mode path; TakeRebindRequest
	// consumes it, the same once-only contract as addRequested.
	rebindRequested bool
	rebindTarget    Project
	// rebindPending is true from the moment TakeRebindRequest hands a request
	// off until the caller answers it — SetRebindError on a rejection, and on
	// success the picker closes so nothing clears it. It makes the form inert
	// while the daemon decides: at most one rebind is in flight per picker, a
	// second Enter cannot race a second mutation, and the answer the user
	// sees (inline error or success) is always the one for the path still on
	// screen.
	rebindPending bool
	// rebindToken names the request rebindPending waits on. The daemon answers
	// asynchronously, so the reply can outlive the picker that asked — Ctrl+C
	// quits past it, and anything that closes and reopens the picker leaves a
	// fresh instance on screen. OwnsRebindReply matches a reply against this
	// token, and the token comes from a process-wide counter, so a reply can
	// only ever reach the instance and request that started it.
	rebindToken uint64
	// rebindDeny, when non-empty, refuses rebind before it can submit — the
	// refusal IS the message, pre-shown on entry and re-shown on Enter. The
	// caller sets it when the targeted daemon is remote: the picker's prj_…
	// ids come from the LOCAL config.ListProjects and the path resolves on
	// the client, so a remote rebind would fail on the remote's registry —
	// or worse, move a remote record that happens to share the id.
	rebindDeny string

	// degraded marks a failed project-registry read (#3298): the rows still
	// render from the other discovery sources, but every registered
	// sessionless project may be missing, and the picker must say so instead
	// of presenting the list as complete.
	degraded bool
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

// SetDegraded records whether the project registry read failed (#3298), so
// the picker warns that the list may be incomplete.
func (p *ProjectPickerOverlay) SetDegraded(degraded bool) { p.degraded = degraded }

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

// HighlightedProject names the existing row under the cursor, excluding the
// add-project and rebind-path fields so typing a path never dispatches a
// destructive shortcut.
func (p *ProjectPickerOverlay) HighlightedProject() (Project, bool) {
	if !p.adding && !p.rebinding && p.selectedIdx >= 0 && p.selectedIdx < len(p.all) {
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

// TakeRebindRequest returns a submitted rebind-mode request once — the row it
// was opened on (whose RegistryID is what the daemon rebinds), the entered
// replacement path, and a fresh token — clearing the pending flag so the caller
// sends it exactly once. The caller carries the token on the reply and routes
// the reply here only when OwnsRebindReply accepts it.
func (p *ProjectPickerOverlay) TakeRebindRequest() (RebindRequest, bool) {
	if !p.rebindRequested {
		return RebindRequest{}, false
	}
	p.rebindRequested = false
	// The request is now in flight: the form goes inert until the caller
	// answers (rejection → SetRebindError re-arms it; success closes the
	// picker), so at most one rebind per picker can be pending and a second
	// Enter cannot race a second mutation.
	p.rebindPending = true
	p.rebindToken = rebindTokens.Add(1)
	return RebindRequest{Project: p.rebindTarget, Path: strings.TrimSpace(p.rebindInput), Token: p.rebindToken}, true
}

// OwnsRebindReply reports whether a rebind reply carrying token answers the
// request this picker is waiting on. A reply for any other request — one from
// a picker that has since closed, or one this picker already settled — is not
// this picker's to show: it must neither close the picker nor paint its error.
func (p *ProjectPickerOverlay) OwnsRebindReply(token uint64) bool {
	return p.rebindPending && token != 0 && token == p.rebindToken
}

// RebindPending reports whether a submitted rebind is still waiting on the
// daemon. The caller keeps its always-on hard exit (Ctrl+C) reachable while it
// is, because the inert form consumes every other key.
func (p *ProjectPickerOverlay) RebindPending() bool { return p.rebindPending }

// SetRebindError shows an inline error under the rebind-mode input and keeps
// the overlay open so the user can correct the path.
func (p *ProjectPickerOverlay) SetRebindError(msg string) {
	// The in-flight request answered with a rejection: re-arm the form so the
	// user can correct the path and resubmit.
	p.rebindPending = false
	p.rebindErr = msg
}

// SetRebindDenied refuses rebind before it can submit, carrying the refusal
// message (shown on entry and re-shown on Enter). The caller sets it when a
// rebind could not act on the same daemon host the picker's records and the
// typed path resolve against — a remote target, where both halves come from
// the CLIENT but the mutation would land on the remote registry.
func (p *ProjectPickerOverlay) SetRebindDenied(msg string) { p.rebindDeny = msg }

// HandleKeyPress processes input. Returns true if the overlay should close.
func (p *ProjectPickerOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	if p.adding {
		return p.handleAddKey(msg)
	}
	if p.rebinding {
		return p.handleRebindKey(msg)
	}
	return p.handleListKey(msg)
}

func (p *ProjectPickerOverlay) handleListKey(msg tea.KeyMsg) bool {
	// The picker navigates like the instances rail: up/k and down/j move the
	// cursor over the full list (clamped, no wrap), Enter switches, Esc cancels.
	// There is no search — any other key (typed letters, etc.) is ignored.
	if key.Matches(msg, keys.GlobalKeyBindings[keys.KeyRebindProject]) {
		// b enters rebind mode, but only on a registry-backed row: the request
		// repairs a registration's stable identity, and a session-derived row
		// has none to move.
		if proj, ok := p.HighlightedProject(); ok && proj.RegistryID != "" {
			p.rebinding = true
			p.rebindTarget = proj
			p.rebindInput = ""
			// Pre-show the caller's refusal when rebind cannot run at all
			// (a remote target) — "" under the normal local path.
			p.rebindErr = p.rebindDeny
		}
		return false
	}
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

func (p *ProjectPickerOverlay) handleRebindKey(msg tea.KeyMsg) bool {
	if p.rebindPending {
		// The daemon is deciding: submission AND editing are inert so a second
		// Enter cannot race a second mutation, and the answer that lands is
		// unambiguously the one for the path still on screen.
		return false
	}
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		// Back out of rebind mode to the list rather than closing the picker.
		p.rebinding = false
		p.rebindInput = ""
		p.rebindErr = ""
		return false
	case tea.KeyEnter:
		if p.rebindDeny != "" {
			// Rebind cannot run at all (a remote target): re-show the refusal
			// rather than produce a request that would reach the wrong host.
			p.rebindErr = p.rebindDeny
			return false
		}
		if strings.TrimSpace(p.rebindInput) != "" {
			p.rebindRequested = true
		}
		return false
	case tea.KeyBackspace:
		if len(p.rebindInput) > 0 {
			runes := []rune(p.rebindInput)
			p.rebindInput = string(runes[:len(runes)-1])
			p.rebindErr = ""
		}
	case tea.KeySpace:
		p.rebindInput += " "
		p.rebindErr = ""
	case tea.KeyRunes:
		p.rebindInput += string(msg.Runes)
		p.rebindErr = ""
	}
	return false
}

// Render renders the project picker overlay.
func (p *ProjectPickerOverlay) Render() string {
	t := ui.CurrentTheme()
	titleStyle := ui.DialogTitleStyle()
	selectedStyle := lipgloss.NewStyle().Bold(true).Background(t.SurfaceRaised).Foreground(t.Ink)
	normalStyle := lipgloss.NewStyle().Foreground(t.Ink)
	overflowStyle := lipgloss.NewStyle().Foreground(t.InkMuted)
	queryStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Ink)
	countStyle := lipgloss.NewStyle().Foreground(t.InkMuted)
	addStyle := lipgloss.NewStyle().Foreground(t.Ink)
	errStyle := lipgloss.NewStyle().Foreground(t.Dead)

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

	title := truncateOverlayLine(titleStyle.Render("Switch project"), cw)

	if p.adding {
		errLine := ""
		if p.addErr != "" {
			errLine = truncateOverlayLine(errStyle.Render("  "+p.addErr), cw)
		}
		lines := formLines(textRect.H, title,
			truncateOverlayLine(normalStyle.Render("Enter a repo path:"), cw),
			truncateOverlayLine("  "+queryStyle.Render(p.pathInput)+ui.InputCaret(), cw),
			errLine,
			truncateOverlayLine(ui.ActionHint("enter add · esc back"), cw))
		return finishRender(style, fit, textRect, lines)
	}

	if p.rebinding {
		errLine := ""
		if p.rebindErr != "" {
			errLine = truncateOverlayLine(errStyle.Render("  "+p.rebindErr), cw)
		}
		// While the daemon decides, the inert form says so — a bare "enter
		// rebind" hint would invite the second Enter that must not dispatch.
		hint := "enter rebind · esc back"
		if p.rebindPending {
			hint = "rebinding… · ctrl+c quit"
		}
		lines := formLines(textRect.H, title,
			truncateOverlayLine(normalStyle.Render(
				fmt.Sprintf("New checkout path for %s:", p.rebindTarget.Name)), cw),
			truncateOverlayLine("  "+queryStyle.Render(p.rebindInput)+ui.InputCaret(), cw),
			errLine,
			truncateOverlayLine(ui.ActionHint(hint), cw))
		return finishRender(style, fit, textRect, lines)
	}

	lines := []string{title, ""}
	if p.degraded {
		// A failed registry read may hide every registered sessionless
		// project — say so rather than render the remainder as complete
		// (#3298). Only the list states it: the add/rebind forms show no
		// list, and their rows are budgeted without it.
		warnStyle := lipgloss.NewStyle().Foreground(t.Dead)
		lines = append(lines, truncateOverlayLine(warnStyle.Render("Cannot read registry · list may be incomplete"), cw))
	}

	// Reserve rows for the fixed chrome (title, blank, blank, hint — plus the
	// degraded notice when present) and window the navigable rows into what
	// remains.
	avail := textRect.H - 4
	if p.degraded {
		avail--
	}
	if avail < 1 {
		avail = 1
	}
	start, end, showAbove, showBelow := budgetedSelectionWindow(p.selectedIdx, p.rowCount(), avail, 0)
	if showAbove {
		lines = append(lines, truncateOverlayLine(overflowStyle.Render(fmt.Sprintf("    … %d more above", start)), cw))
	}
	for i := start; i < end; i++ {
		lines = append(lines, truncateOverlayLine(p.renderRow(i, selectedStyle, normalStyle, countStyle, addStyle), cw))
	}
	if showBelow {
		lines = append(lines, truncateOverlayLine(overflowStyle.Render(fmt.Sprintf("    … and %d more below", p.rowCount()-end)), cw))
	}

	lines = append(lines, "")
	hint := "j/k select · enter add · esc cancel"
	registryRow := false
	if proj, ok := p.HighlightedProject(); ok {
		hint = "j/k select · enter switch · D delete · esc cancel"
		registryRow = proj.RegistryID != ""
		if registryRow {
			hint = "j/k select · enter switch · D delete · b rebind · esc cancel"
		}
	}
	if layout.Cells(hint) > cw && registryRow {
		// A registry row's longer hint shrinks by dropping the prose, keeping the
		// verbs the row actually offers (an unadvertised verb is unreachable).
		hint = "j/k · enter · D delete · b rebind · esc"
	}
	if layout.Cells(hint) > cw {
		hint = "j/k · enter · esc"
	}
	lines = append(lines, truncateOverlayLine(ui.ActionHint(hint), cw))

	return finishRender(style, fit, textRect, lines)
}

// renderRow renders one navigable row: a project ("name (N)") or the trailing
// "+ Add project…" affordance, highlighting the selected one.
func (p *ProjectPickerOverlay) renderRow(i int, selectedStyle, normalStyle, countStyle, addStyle lipgloss.Style) string {
	selected := i == p.selectedIdx
	if i == len(p.all) {
		text := "+ Add project…"
		if selected {
			return "  " + ui.SelectionMarker("▸ ") + selectedStyle.Render(text)
		}
		return "    " + addStyle.Render(text)
	}
	proj := p.all[i]
	count := countStyle.Render(fmt.Sprintf(" (%d)", proj.SessionCount))
	// A registered row whose checkout is gone says so — the `b` verb this flag
	// gates is the repair for exactly that state.
	missing := ""
	if proj.MissingPath {
		missing = lipgloss.NewStyle().Foreground(ui.CurrentTheme().Dead).Render(" · missing")
	}
	if selected {
		return "  " + ui.SelectionMarker("▸ ") + selectedStyle.Render(proj.Name+fmt.Sprintf(" (%d)", proj.SessionCount)) + missing
	}
	return "    " + normalStyle.Render(proj.Name) + count + missing
}

// formLines lays out the add/rebind path form inside rows text rows (rows <= 0
// means unbounded). The full form is title, spacer, prompt, input, error (when
// set), spacer, hint. A short frame sheds the spacers first, then the title, then
// the prompt: the input, its error and the hint are what the user acts on, and
// the frame must never grow past its budget — PlaceOverlay drops the whole
// background for an oversized foreground.
func formLines(rows int, title, prompt, input, errLine, hint string) []string {
	type row struct {
		text string
		shed int // shed order when over budget; 0 is never shed
	}
	all := []row{{title, 3}, {"", 2}, {prompt, 4}, {input, 0}}
	if errLine != "" {
		all = append(all, row{errLine, 0})
	}
	all = append(all, row{"", 1}, row{hint, 0})
	n := len(all)
	shed := map[int]bool{}
	for next := 1; rows > 0 && n > rows && next <= 4; next++ {
		shed[next] = true
		n--
	}
	lines := make([]string, 0, n)
	for _, r := range all {
		if r.shed == 0 || !shed[r.shed] {
			lines = append(lines, r.text)
		}
	}
	return lines
}

// finishRender sizes the style box and joins the content lines, matching the
// search overlay's sizing so both modals size identically.
func finishRender(style lipgloss.Style, fit, textRect layout.Rect, lines []string) string {
	style = style.Width(fit.W)
	if fit.H > 0 && len(lines) >= textRect.H {
		style = style.Height(fit.H)
	}
	return ui.RenderDialog(style, strings.Join(lines, "\n"))
}
