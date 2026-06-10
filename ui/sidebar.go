package ui

import (
	"errors"
	"fmt"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"strings"

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

// Sidebar is the unified left navigation pane with collapsible sections.
type Sidebar struct {
	sections     []SidebarSection
	visibleItems []SidebarItem
	selectedIdx  int

	// scrollOffset is the index into visibleItems of the first rendered row
	// when the list is too tall for the allocation. It is adjusted lazily in
	// String() (rebuilds can shift indices between renders), scrolling
	// minimally so the selected row stays visible.
	scrollOffset int

	// Data
	instances []*session.Instance
	tasks     []task.Task
	hookCount int

	// Rendering
	renderer *InstanceRenderer
	autoyes  bool
	height   int
	width    int
	repos    map[string]int
}

// NewSidebar creates a new sidebar.
func NewSidebar(spin *spinner.Model, autoYes bool) *Sidebar {
	s := &Sidebar{
		sections: []SidebarSection{
			{Kind: SectionInstances, Title: "Instances", Expanded: true},
			{Kind: SectionTasks, Title: "Tasks", Expanded: false},
			{Kind: SectionHooks, Title: "Hooks", Expanded: false},
		},
		renderer: &InstanceRenderer{spinner: spin},
		repos:    make(map[string]int),
		autoyes:  autoYes,
	}
	s.rebuildVisibleItems()
	return s
}

// SetSize sets the display dimensions.
func (s *Sidebar) SetSize(width, height int) {
	s.width = width
	s.height = height
	s.renderer.setWidth(width)
}

// SetSessionPreviewSize sets the tmux session preview sizes. Instances whose
// underlying tmux session has vanished (ErrSessionGone) are skipped silently
// — the daemon-side latch already covers ongoing polling and the resize itself
// has no useful work to do on a dead session (#496).
func (s *Sidebar) SetSessionPreviewSize(width, height int) error {
	var err error
	for i, item := range s.instances {
		if !item.Started() {
			continue
		}
		if innerErr := item.SetPreviewSize(width, height); innerErr != nil {
			if errors.Is(innerErr, tmux.ErrSessionGone) {
				continue
			}
			err = fmt.Errorf("could not set preview size for instance %d: %v", i, innerErr)
		}
	}
	return err
}

// SetTasks updates the task data.
func (s *Sidebar) SetTasks(tasks []task.Task) {
	s.tasks = tasks
	s.rebuildVisibleItems()
}

// SetHookCount updates the displayed hook count.
func (s *Sidebar) SetHookCount(count int) {
	s.hookCount = count
	s.rebuildVisibleItems()
}

// GetTasks returns the current tasks.
func (s *Sidebar) GetTasks() []task.Task {
	return s.tasks
}

// rebuildVisibleItems rebuilds the flat list of visible items based on expand/collapse state.
func (s *Sidebar) rebuildVisibleItems() {
	var items []SidebarItem
	for _, sec := range s.sections {
		items = append(items, SidebarItem{Kind: sec.Kind, IsHeader: true})
		if sec.Expanded {
			switch sec.Kind {
			case SectionInstances:
				for i := range s.instances {
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

// GetSelection returns the currently selected sidebar item.
func (s *Sidebar) GetSelection() SidebarItem {
	if len(s.visibleItems) == 0 {
		return SidebarItem{Kind: SectionInstances, IsHeader: true}
	}
	return s.visibleItems[s.selectedIdx]
}

// Down moves the cursor down.
func (s *Sidebar) Down() {
	if s.selectedIdx < len(s.visibleItems)-1 {
		s.selectedIdx++
	}
}

// Up moves the cursor up.
func (s *Sidebar) Up() {
	if s.selectedIdx > 0 {
		s.selectedIdx--
	}
}

// ToggleSection expands or collapses the section of the current selection.
func (s *Sidebar) ToggleSection() {
	sel := s.GetSelection()
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
	sel := s.GetSelection()
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
}

// CollapseSection collapses the section of the current selection.
func (s *Sidebar) CollapseSection() {
	sel := s.GetSelection()
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
}

// JumpNextSection jumps to the next section header.
func (s *Sidebar) JumpNextSection() {
	for i := s.selectedIdx + 1; i < len(s.visibleItems); i++ {
		if s.visibleItems[i].IsHeader {
			s.selectedIdx = i
			return
		}
	}
}

// JumpPrevSection jumps to the previous section header.
func (s *Sidebar) JumpPrevSection() {
	for i := s.selectedIdx - 1; i >= 0; i-- {
		if s.visibleItems[i].IsHeader {
			s.selectedIdx = i
			return
		}
	}
}

// --- Instance management (delegates to underlying data) ---

// NumInstances returns the number of instances.
func (s *Sidebar) NumInstances() int {
	return len(s.instances)
}

// NumRepos returns the number of repositories represented in the sidebar.
func (s *Sidebar) NumRepos() int {
	return len(s.repos)
}

// AddInstance adds a new instance. Returns a finalizer to register the repo.
func (s *Sidebar) AddInstance(instance *session.Instance) (finalize func()) {
	s.instances = append(s.instances, instance)
	s.rebuildVisibleItems()
	return func() { s.RegisterRepoForInstance(instance) }
}

// RegisterRepoForInstance records the instance's repo after the instance has
// started and its worktree is available.
func (s *Sidebar) RegisterRepoForInstance(instance *session.Instance) {
	repoName, err := instance.RepoName()
	if err != nil {
		log.ErrorLog.Printf("could not get repo name: %v", err)
		return
	}
	s.addRepo(repoName)
}

// Kill kills the currently selected instance. It returns an error if the
// underlying kill fails, in which case the instance is NOT removed from the
// sidebar so the user can retry. See KillInstance for the pointer-based
// variant that deferred/cancel flows must use.
func (s *Sidebar) Kill() error {
	return s.KillInstance(s.GetSelectedInstance())
}

// KillInstance kills the given instance by pointer identity, independent of the
// current selection. Deferred flows — most notably canceling a new instance
// via Escape/ctrl+c — must use this rather than Kill(): background sync can
// rebuild visibleItems and drift the selection off the target row between the
// time the operation is initiated and the time it runs. Selection-based Kill()
// would then silently no-op (selection landed on a section header) and leave
// the naming instance behind as a "Loading" zombie (#717).
//
// A nil target or an instance no longer in the sidebar is a no-op, mirroring
// Kill()'s tolerance for a stale selection.
func (s *Sidebar) KillInstance(target *session.Instance) error {
	if target == nil {
		return nil
	}
	idx := -1
	for i, inst := range s.instances {
		if inst == target {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	// Capture repo name before Kill(), because Kill() sets started=false
	// which causes RepoName() to fail.
	repoName, repoErr := target.RepoName()
	if err := target.Kill(); err != nil {
		return fmt.Errorf("could not kill instance: %w", err)
	}
	if repoErr != nil {
		log.ErrorLog.Printf("could not get repo name: %v", repoErr)
	} else {
		s.rmRepo(repoName)
	}
	s.instances = append(s.instances[:idx], s.instances[idx+1:]...)
	s.rebuildVisibleItems()
	return nil
}

// AttachInstance attaches to the given instance by pointer identity, but only
// if it is still present in the sidebar. Deferred attach flows — the first-time
// attach help screen, whose onDismiss callback runs after the overlay is
// dismissed — must capture the instance at key-press time and attach through
// this method rather than re-reading the live selection: a background refresh
// can drift the selection onto a different instance while the help overlay is
// open, so Attach() would connect to the wrong session (#716).
func (s *Sidebar) AttachInstance(target *session.Instance) (chan struct{}, error) {
	if target == nil || !s.ContainsInstance(target) {
		return nil, fmt.Errorf("instance no longer exists")
	}
	return target.Attach()
}

// GetSelectedInstance returns the currently selected instance, or nil.
func (s *Sidebar) GetSelectedInstance() *session.Instance {
	sel := s.GetSelection()
	if sel.Kind != SectionInstances || sel.IsHeader {
		return nil
	}
	if sel.ItemIndex < 0 || sel.ItemIndex >= len(s.instances) {
		return nil
	}
	return s.instances[sel.ItemIndex]
}

// SetSelectedInstance sets the selected index to point at the given instance index.
// If the Instances section is collapsed, it is expanded first so the target row
// is always reachable.
func (s *Sidebar) SetSelectedInstance(idx int) {
	if idx >= len(s.instances) {
		return
	}
	// Ensure the Instances section is expanded so the target row is part of
	// visibleItems; otherwise the search below would silently no-op.
	s.ExpandInstancesSection()
	// Find the visible item that corresponds to this instance
	for i, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && item.ItemIndex == idx {
			s.selectedIdx = i
			return
		}
	}
}

// SelectInstance finds and selects the given instance.
func (s *Sidebar) SelectInstance(target *session.Instance) {
	for i, inst := range s.instances {
		if inst == target {
			s.SetSelectedInstance(i)
			return
		}
	}
}

// ContainsInstance reports whether target is currently in the sidebar.
func (s *Sidebar) ContainsInstance(target *session.Instance) bool {
	for _, inst := range s.instances {
		if inst == target {
			return true
		}
	}
	return false
}

// ReplaceInstance swaps an existing sidebar instance for replacement while
// preserving the selected row when the replaced instance was selected.
func (s *Sidebar) ReplaceInstance(target, replacement *session.Instance) bool {
	for i, inst := range s.instances {
		if inst != target {
			continue
		}
		wasSelected := s.GetSelectedInstance() == inst
		s.instances[i] = replacement
		s.rebuildVisibleItems()
		if wasSelected {
			s.SetSelectedInstance(i)
		}
		return true
	}
	return false
}

// ReplaceInstanceByTitle swaps the sidebar instance carrying the given title
// for replacement, preserving the selected row. Returns false when no
// instance has that title. Used by the instance-start handler when its
// pointer-based ReplaceInstance misses: a background sync may have swapped
// the Loading placeholder for a disk-built copy of the same session while
// the start RPC was in flight, and re-adding the started instance would
// leave two sidebar rows — and two persisted records — with one title (#808).
func (s *Sidebar) ReplaceInstanceByTitle(title string, replacement *session.Instance) bool {
	for _, inst := range s.instances {
		if inst.Title == title {
			return s.ReplaceInstance(inst, replacement)
		}
	}
	return false
}

// GetInstances returns all instances. The returned slice shares the sidebar's
// backing array, so callers must only read it on the bubbletea event loop —
// never hand it to a goroutine that outlives the call (see
// GetInstancesSnapshot for that).
func (s *Sidebar) GetInstances() []*session.Instance {
	return s.instances
}

// GetInstancesSnapshot returns a copy of the instances slice for handing off
// to a background goroutine. The metadata tick (#682) iterates the list on a
// separate goroutine while the event loop may append/remove instances; copying
// the slice header here (on the event loop, where mutations also happen) gives
// the goroutine a private backing array so the two cannot race on the same
// memory. The *session.Instance elements are shared, but each guards its own
// fields with a mutex, so reading them across goroutines is safe.
func (s *Sidebar) GetInstancesSnapshot() []*session.Instance {
	out := make([]*session.Instance, len(s.instances))
	copy(out, s.instances)
	return out
}

// GetInstanceTitles returns a set of all instance titles for quick comparison.
func (s *Sidebar) GetInstanceTitles() map[string]bool {
	titles := make(map[string]bool, len(s.instances))
	for _, inst := range s.instances {
		titles[inst.Title] = true
	}
	return titles
}

// RemoveInstanceByTitle removes an instance from the sidebar by title without
// killing it (the external process already cleaned up tmux/worktree).
// Note: the instance may already have been killed (started=false), so we fall
// back to looking up the repo name from the gitWorktree directly.
func (s *Sidebar) RemoveInstanceByTitle(title string) bool {
	for i, inst := range s.instances {
		if inst.Title == title {
			repoName, err := inst.RepoName()
			if err != nil {
				// If RepoName() fails (e.g. instance already killed/not started),
				// try to find and remove the repo by scanning remaining instances.
				// We cannot decrement the repo count without the name, so log and
				// continue — the repo map will be corrected on next full rebuild.
				log.ErrorLog.Printf("could not get repo name: %v", err)
			} else {
				s.rmRepo(repoName)
			}
			s.instances = append(s.instances[:i], s.instances[i+1:]...)
			s.rebuildVisibleItems()
			return true
		}
	}
	return false
}

// RemoveInstanceByTitleWithRepo removes an instance from the sidebar by title
// using the supplied repoName instead of calling RepoName() on the instance.
// This is useful when the caller has already killed the instance (which causes
// RepoName() to fail) but captured the repo name beforehand, ensuring the repo
// count is still decremented correctly.
func (s *Sidebar) RemoveInstanceByTitleWithRepo(title, repoName string) bool {
	for i, inst := range s.instances {
		if inst.Title == title {
			s.rmRepo(repoName)
			s.instances = append(s.instances[:i], s.instances[i+1:]...)
			s.rebuildVisibleItems()
			return true
		}
	}
	return false
}

func (s *Sidebar) addRepo(repo string) {
	if _, ok := s.repos[repo]; !ok {
		s.repos[repo] = 0
	}
	s.repos[repo]++
}

func (s *Sidebar) rmRepo(repo string) {
	if _, ok := s.repos[repo]; !ok {
		log.ErrorLog.Printf("repo %s not found", repo)
		return
	}
	s.repos[repo]--
	if s.repos[repo] == 0 {
		delete(s.repos, repo)
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
		label = fmt.Sprintf("Instances (%d)", len(s.instances))
	case SectionTasks:
		label = fmt.Sprintf("Tasks (%d)", len(s.tasks))
		arrow = "  " // no expand arrow for leaf sections
	case SectionHooks:
		if s.hookCount > 0 {
			label = fmt.Sprintf("Hooks (%d)", s.hookCount)
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
	if idx < 0 || idx >= len(s.instances) {
		return ""
	}
	return s.renderer.Render(s.instances[idx], idx+1, selected, len(s.repos) > 1)
}
