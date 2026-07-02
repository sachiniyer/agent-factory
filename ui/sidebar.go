package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// SidebarSectionKind identifies the type of sidebar section.
type SidebarSectionKind int

const (
	SectionInstances SidebarSectionKind = iota
	SectionTasks
	SectionHooks
)

// SidebarItem represents one visible row in the sidebar.
type SidebarItem struct {
	Kind      SidebarSectionKind
	IsHeader  bool
	ItemIndex int // index within the section's children (instances/tasks)
}

// SidebarSection holds state for one collapsible section.
type SidebarSection struct {
	Kind     SidebarSectionKind
	Title    string
	Expanded bool
}

var sectionHeaderStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

var sectionHeaderSelectedStyle = lipgloss.NewStyle().
	Bold(true).
	Background(lipgloss.Color("#dde4f0")).
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#1a1a1a"})

var windowIndicatorStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"})

// Sidebar is the unified left navigation pane with collapsible sections. It is
// a VIEW over the store.Projection (#1024 PR 2): the instance/task/hook data —
// and the cross-pane selection — live in the store, written by the snapshot
// reconcile and the session-control handlers. The sidebar owns only its local
// UI state: the flattened row list, the cursor over it, section expansion, and
// the scroll window.
type Sidebar struct {
	proj *store.Projection

	sections     []SidebarSection
	visibleItems []SidebarItem
	selectedIdx  int

	// seenVersion is the store version visibleItems (and the cursor re-pin)
	// were last derived from. The store is written directly by event-loop
	// handlers without notifying the sidebar, so every public method lazily
	// re-derives via syncFromStore before touching the row list. seenSelSeq
	// tracks the store's SelectInstance assertions the cursor has honored.
	seenVersion uint64
	seenSelSeq  uint64

	// scrollOffset is the index into visibleItems of the first rendered row
	// when the list is too tall for the allocation. It is adjusted lazily in
	// String() (rebuilds can shift indices between renders), scrolling
	// minimally so the selected row stays visible.
	scrollOffset int

	// Rendering
	renderer *InstanceRenderer
	autoyes  bool
	height   int
	width    int
}

// NewSidebar creates a new sidebar rendering the given projection.
func NewSidebar(spin *spinner.Model, autoYes bool, proj *store.Projection) *Sidebar {
	s := &Sidebar{
		proj: proj,
		sections: []SidebarSection{
			{Kind: SectionInstances, Title: "Instances", Expanded: true},
			{Kind: SectionTasks, Title: "Tasks", Expanded: false},
			{Kind: SectionHooks, Title: "Hooks", Expanded: false},
		},
		renderer: &InstanceRenderer{spinner: spin},
		autoyes:  autoYes,
	}
	s.rebuildVisibleItems()
	s.seenVersion = proj.Version()
	return s
}

// SetSize sets the display dimensions.
func (s *Sidebar) SetSize(width, height int) {
	s.width = width
	s.height = height
	s.renderer.setWidth(width)
}

// syncFromStore re-derives the flattened row list when the projection changed
// since the last derivation, then re-pins the cursor. The cursor rules
// preserve the pre-store mutation behavior exactly:
//   - by default the cursor keeps its flat index, clamped by the rebuild —
//     the same drift-then-clamp the old mutation-time rebuild applied (a
//     removal can legitimately drift the cursor, e.g. onto a header while
//     naming an instance — the #717 reality the cancel path defends against);
//   - a pending SelectInstance assertion (the reconcile's #969 re-pin, the
//     search overlay, an explicit re-select) moves the cursor onto the
//     asserted instance's row; a dangling or nil assertion leaves the clamped
//     cursor as-is, matching the old "selected title gone → sidebar clamp
//     behavior" rule.
//
// Afterwards the cursor's row (if it rests on an instance) is pushed back
// into the store so the display binding and the cursor can never disagree at
// a read boundary.
func (s *Sidebar) syncFromStore() {
	if s.proj.Version() == s.seenVersion {
		return
	}
	s.rebuildVisibleItems()
	if s.proj.SelectionSeq() != s.seenSelSeq {
		if inst := s.proj.GetSelectedInstance(); inst != nil {
			s.moveCursorToInstance(inst)
		}
		s.seenSelSeq = s.proj.SelectionSeq()
	}
	s.pushSelection()
	s.seenVersion = s.proj.Version()
}

// moveCursorToInstance places the cursor on the row of target, if it is in
// the projection and its row is visible. No-op otherwise.
func (s *Sidebar) moveCursorToInstance(target *session.Instance) {
	for i, inst := range s.proj.GetInstances() {
		if inst != target {
			continue
		}
		for j, item := range s.visibleItems {
			if item.Kind == SectionInstances && !item.IsHeader && item.ItemIndex == i {
				s.selectedIdx = j
				return
			}
		}
		return
	}
}

// pushSelection records the cursor's instance (when it rests on an instance
// row) as the store's display binding. Header rows deliberately do NOT clear
// the binding: the workspace panes keep displaying the last selected instance
// while the cursor tours headers, exactly as the pre-store TabbedWindow kept
// its instance pointer. Callers run this after a sync, so folding the
// resulting version move into the seen markers is safe (single-threaded event
// loop; nothing else can write between).
func (s *Sidebar) pushSelection() {
	sel := s.rawSelection()
	if sel.Kind != SectionInstances || sel.IsHeader {
		return
	}
	instances := s.proj.GetInstances()
	if sel.ItemIndex < 0 || sel.ItemIndex >= len(instances) {
		return
	}
	s.proj.SetSelectedInstance(instances[sel.ItemIndex])
	s.seenVersion = s.proj.Version()
	s.seenSelSeq = s.proj.SelectionSeq()
}

// rebuildVisibleItems rebuilds the flat list of visible items based on expand/collapse state.
func (s *Sidebar) rebuildVisibleItems() {
	var items []SidebarItem
	for _, sec := range s.sections {
		items = append(items, SidebarItem{Kind: sec.Kind, IsHeader: true})
		if sec.Expanded {
			switch sec.Kind {
			case SectionInstances:
				for i := 0; i < s.proj.NumInstances(); i++ {
					items = append(items, SidebarItem{Kind: SectionInstances, ItemIndex: i})
				}
			}
		}
	}
	s.visibleItems = items
	// Clamp selectedIdx
	if s.selectedIdx >= len(s.visibleItems) {
		s.selectedIdx = len(s.visibleItems) - 1
	}
	if s.selectedIdx < 0 {
		s.selectedIdx = 0
	}
}

// rawSelection returns the currently selected item without syncing against
// the store — for internal use where visibleItems is already current (or
// deliberately pre-sync, as in syncFromStore).
func (s *Sidebar) rawSelection() SidebarItem {
	if len(s.visibleItems) == 0 {
		return SidebarItem{Kind: SectionInstances, IsHeader: true}
	}
	return s.visibleItems[s.selectedIdx]
}

// GetSelection returns the currently selected sidebar item.
func (s *Sidebar) GetSelection() SidebarItem {
	s.syncFromStore()
	return s.rawSelection()
}

// Down moves the cursor down.
func (s *Sidebar) Down() {
	s.syncFromStore()
	if s.selectedIdx < len(s.visibleItems)-1 {
		s.selectedIdx++
	}
	s.pushSelection()
}

// Up moves the cursor up.
func (s *Sidebar) Up() {
	s.syncFromStore()
	if s.selectedIdx > 0 {
		s.selectedIdx--
	}
	s.pushSelection()
}

// ToggleSection expands or collapses the section of the current selection.
func (s *Sidebar) ToggleSection() {
	s.syncFromStore()
	sel := s.rawSelection()
	if !sel.IsHeader {
		return
	}
	for i, sec := range s.sections {
		if sec.Kind == sel.Kind {
			s.sections[i].Expanded = !s.sections[i].Expanded
			break
		}
	}
	s.rebuildVisibleItems()
	s.pushSelection()
}

// ExpandInstancesSection ensures the Instances section is expanded.
func (s *Sidebar) ExpandInstancesSection() {
	for i, sec := range s.sections {
		if sec.Kind == SectionInstances {
			s.sections[i].Expanded = true
			break
		}
	}
	s.rebuildVisibleItems()
}

// ExpandSection expands the section of the current selection.
func (s *Sidebar) ExpandSection() {
	s.syncFromStore()
	sel := s.rawSelection()
	if !sel.IsHeader {
		return
	}
	for i, sec := range s.sections {
		if sec.Kind == sel.Kind {
			s.sections[i].Expanded = true
			break
		}
	}
	s.rebuildVisibleItems()
	s.pushSelection()
}

// CollapseSection collapses the section of the current selection.
func (s *Sidebar) CollapseSection() {
	s.syncFromStore()
	sel := s.rawSelection()
	if !sel.IsHeader {
		// If on a child, jump to parent header
		for i, item := range s.visibleItems {
			if item.Kind == sel.Kind && item.IsHeader {
				s.selectedIdx = i
				break
			}
		}
	}
	for i, sec := range s.sections {
		if sec.Kind == sel.Kind {
			s.sections[i].Expanded = false
			break
		}
	}
	s.rebuildVisibleItems()
	s.pushSelection()
}

// JumpNextSection jumps to the next section header.
func (s *Sidebar) JumpNextSection() {
	s.syncFromStore()
	for i := s.selectedIdx + 1; i < len(s.visibleItems); i++ {
		if s.visibleItems[i].IsHeader {
			s.selectedIdx = i
			return
		}
	}
}

// JumpPrevSection jumps to the previous section header.
func (s *Sidebar) JumpPrevSection() {
	s.syncFromStore()
	for i := s.selectedIdx - 1; i >= 0; i-- {
		if s.visibleItems[i].IsHeader {
			s.selectedIdx = i
			return
		}
	}
}

// GetSelectedInstance returns the instance under the cursor, or nil when the
// cursor rests on a section header. Note this is the cursor-derived selection;
// the store's GetSelectedInstance is the sticky display selection the
// workspace panes render.
func (s *Sidebar) GetSelectedInstance() *session.Instance {
	s.syncFromStore()
	sel := s.rawSelection()
	if sel.Kind != SectionInstances || sel.IsHeader {
		return nil
	}
	instances := s.proj.GetInstances()
	if sel.ItemIndex < 0 || sel.ItemIndex >= len(instances) {
		return nil
	}
	return instances[sel.ItemIndex]
}

// SetSelectedInstance sets the cursor to point at the given instance index.
// If the Instances section is collapsed, it is expanded first so the target
// row is always reachable.
func (s *Sidebar) SetSelectedInstance(idx int) {
	s.syncFromStore()
	if idx >= s.proj.NumInstances() {
		return
	}
	// Ensure the Instances section is expanded so the target row is part of
	// visibleItems; otherwise the search below would silently no-op.
	s.ExpandInstancesSection()
	// Find the visible item that corresponds to this instance
	for i, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && item.ItemIndex == idx {
			s.selectedIdx = i
			break
		}
	}
	s.pushSelection()
}

// SelectInstance finds and selects the given instance.
func (s *Sidebar) SelectInstance(target *session.Instance) {
	s.syncFromStore()
	for i, inst := range s.proj.GetInstances() {
		if inst == target {
			s.SetSelectedInstance(i)
			return
		}
	}
}

// String renders the sidebar. The item list is windowed around the selection
// (#787): lipgloss.Place pads short content but never truncates, so rendering
// every row would overflow the allocation once enough instances exist and push
// the menu/error box below the fold. A blind clamp (the #700 fix for the
// content pane) would be wrong here because the selected row can sit below the
// fold while navigating — instead the rendered slice scrolls so the selection
// is always visible, with "▲/▼ N more" rows marking hidden items.
func (s *Sidebar) String() string {
	s.syncFromStore()

	// Chrome above the item list: one leading blank line plus the title row.
	const chromeLines = 2

	// Render every visible row up front and measure real heights: instance
	// rows are multi-line (title + branch, plus an optional PR line), so the
	// window math cannot assume one line per item.
	rows := make([]string, len(s.visibleItems))
	heights := make([]int, len(s.visibleItems))
	totalLines := 0
	for i, item := range s.visibleItems {
		isSelected := i == s.selectedIdx
		if item.IsHeader {
			rows[i] = s.renderHeader(item.Kind, isSelected)
		} else {
			switch item.Kind {
			case SectionInstances:
				rows[i] = s.renderInstance(item.ItemIndex, isSelected)
			}
		}
		heights[i] = lipgloss.Height(rows[i])
		totalLines += heights[i]
	}

	avail := s.height - chromeLines
	start, end := 0, len(rows)
	hiddenAbove, hiddenBelow := 0, 0
	if s.height > 0 && totalLines > avail {
		s.scrollToSelection(heights, avail)
		start = s.scrollOffset
		end, _, _ = fitWindow(heights, start, avail)
		hiddenAbove = start
		hiddenBelow = len(rows) - end
	} else {
		s.scrollOffset = 0
	}

	var b strings.Builder
	b.WriteString("\n")

	// Title bar
	titleWidth := AdjustPreviewWidth(s.width) + 2
	if !s.autoyes {
		b.WriteString(lipgloss.Place(
			titleWidth, 1, lipgloss.Left, lipgloss.Bottom, mainTitle.Render(" Agent Factory ")))
	} else {
		title := lipgloss.Place(
			titleWidth/2, 1, lipgloss.Left, lipgloss.Bottom, mainTitle.Render(" Agent Factory "))
		autoYes := lipgloss.Place(
			titleWidth-(titleWidth/2), 1, lipgloss.Right, lipgloss.Bottom, autoYesStyle.Render(" auto-yes "))
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, title, autoYes))
	}

	if hiddenAbove > 0 {
		b.WriteString("\n")
		b.WriteString(s.renderWindowIndicator("▲", hiddenAbove))
	}
	for i := start; i < end; i++ {
		b.WriteString("\n")
		b.WriteString(rows[i])
	}
	if hiddenBelow > 0 {
		b.WriteString("\n")
		b.WriteString(s.renderWindowIndicator("▼", hiddenBelow))
	}

	out := lipgloss.Place(s.width, s.height, lipgloss.Left, lipgloss.Top, b.String())
	// Safety clamp: fitWindow force-includes the first windowed row even when
	// it alone exceeds the budget (e.g. a tall PR row at a tiny height), and
	// lipgloss.Place will not truncate the overflow.
	if s.height > 0 {
		if lines := strings.Split(out, "\n"); len(lines) > s.height {
			out = strings.Join(lines[:s.height], "\n")
		}
	}
	return out
}

// scrollToSelection clamps scrollOffset and moves it minimally so the selected
// row is fully inside the rendered window. Only called when the list overflows
// the allocation.
func (s *Sidebar) scrollToSelection(heights []int, avail int) {
	n := len(heights)
	if s.scrollOffset > n-1 {
		s.scrollOffset = n - 1
	}
	if s.scrollOffset < 0 {
		s.scrollOffset = 0
	}
	if s.selectedIdx < s.scrollOffset {
		s.scrollOffset = s.selectedIdx
	}
	for s.scrollOffset < s.selectedIdx {
		end, _, _ := fitWindow(heights, s.scrollOffset, avail)
		if s.selectedIdx < end {
			break
		}
		s.scrollOffset++
	}
	// Fill from the bottom: when the tail of the list fits starting one row
	// earlier (items were removed or the selection sits at the end), scroll
	// up to use the slack rather than render dead space below the last item.
	for s.scrollOffset > 0 {
		if end, _, _ := fitWindow(heights, s.scrollOffset-1, avail); end < n {
			break
		}
		s.scrollOffset--
	}
}

// fitWindow returns the exclusive end index of the rows that fit when
// rendering starts at offset within avail lines, plus whether "▲"/"▼"
// indicator rows (one line each) are needed. heights[i] is the rendered line
// count of visibleItems[i].
func fitWindow(heights []int, offset, avail int) (end int, topInd, botInd bool) {
	n := len(heights)
	topInd = offset > 0
	for {
		budget := avail
		if topInd {
			budget--
		}
		if botInd {
			budget--
		}
		end = offset
		used := 0
		for end < n && used+heights[end] <= budget {
			used += heights[end]
			end++
		}
		if end == offset && offset < n {
			// Force at least one row so scrolling can always reach the
			// selection; String()'s final clamp bounds any overflow.
			end = offset + 1
		}
		if end == n || botInd {
			return end, topInd, botInd
		}
		// Rows remain below: reserve a line for the "▼" indicator and refit.
		// Shrinking the budget can only shrink end, so this converges in one
		// extra pass.
		botInd = true
	}
}

// renderWindowIndicator renders the one-line "▲ N more" / "▼ N more" marker
// shown in place of rows scrolled out of the window.
func (s *Sidebar) renderWindowIndicator(arrow string, hidden int) string {
	w := AdjustPreviewWidth(s.width)
	text := fmt.Sprintf("%s %d more", arrow, hidden)
	if w > 0 && runewidth.StringWidth(text) > w {
		// Same narrow-width handling as renderHeader: drop the "..." tail when
		// it would itself overflow, since lipgloss.Place won't clip oversize
		// content.
		tail := "..."
		if w < runewidth.StringWidth(tail) {
			tail = ""
		}
		text = runewidth.Truncate(text, w, tail)
	}
	return windowIndicatorStyle.Padding(0, 1).Render(
		lipgloss.Place(w, 1, lipgloss.Left, lipgloss.Center, text))
}

func (s *Sidebar) renderHeader(kind SidebarSectionKind, selected bool) string {
	var expanded bool
	for _, sec := range s.sections {
		if sec.Kind == kind {
			expanded = sec.Expanded
			break
		}
	}

	arrow := "▶ "
	if expanded {
		arrow = "▼ "
	}

	var label string
	switch kind {
	case SectionInstances:
		label = fmt.Sprintf("Instances (%d)", s.proj.NumInstances())
	case SectionTasks:
		label = fmt.Sprintf("Tasks (%d)", len(s.proj.GetTasks()))
		arrow = "  " // no expand arrow for leaf sections
	case SectionHooks:
		if s.proj.GetHookCount() > 0 {
			label = fmt.Sprintf("Hooks (%d)", s.proj.GetHookCount())
		} else {
			label = "Hooks"
		}
		arrow = "  " // no expand arrow for leaf sections
	}

	style := sectionHeaderStyle
	if selected {
		style = sectionHeaderSelectedStyle
	}

	w := AdjustPreviewWidth(s.width)
	text := arrow + label
	if w > 0 && runewidth.StringWidth(text) > w {
		// Drop the "..." tail when the container is too narrow to fit it,
		// otherwise runewidth.Truncate returns content wider than w and
		// lipgloss.Place won't clip the overflow.
		tail := "..."
		if w < runewidth.StringWidth(tail) {
			tail = ""
		}
		text = runewidth.Truncate(text, w, tail)
	}
	return style.Padding(0, 1).Render(
		lipgloss.Place(w, 1, lipgloss.Left, lipgloss.Center, text))
}

func (s *Sidebar) renderInstance(idx int, selected bool) string {
	instances := s.proj.GetInstances()
	if idx < 0 || idx >= len(instances) {
		return ""
	}
	// Pad the index to the digit width of the largest 1-based index in the list
	// so every row's prefix is the same width and the branch/PR lines align,
	// without widening the common small-list case (#871, #923, #939).
	s.renderer.indexWidth = len(strconv.Itoa(len(instances)))
	return s.renderer.Render(instances[idx], idx+1, selected, s.proj.NumRepos() > 1)
}
