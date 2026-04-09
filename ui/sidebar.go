package ui

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
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

// Sidebar is the unified left navigation pane with collapsible sections.
type Sidebar struct {
	sections     []SidebarSection
	visibleItems []SidebarItem
	selectedIdx  int

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

// SetSessionPreviewSize sets the tmux session preview sizes.
func (s *Sidebar) SetSessionPreviewSize(width, height int) error {
	var err error
	for i, item := range s.instances {
		if !item.Started() {
			continue
		}
		if innerErr := item.SetPreviewSize(width, height); innerErr != nil {
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
		return
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

// AddInstance adds a new instance. Returns a finalizer to register the repo.
func (s *Sidebar) AddInstance(instance *session.Instance) (finalize func()) {
	s.instances = append(s.instances, instance)
	s.rebuildVisibleItems()
	return func() {
		repoName, err := instance.RepoName()
		if err != nil {
			log.ErrorLog.Printf("could not get repo name: %v", err)
			return
		}
		s.addRepo(repoName)
	}
}

// Kill kills the selected instance. It returns an error if the underlying
// kill fails, in which case the instance is NOT removed from the sidebar
// so the user can retry.
func (s *Sidebar) Kill() error {
	sel := s.GetSelection()
	if sel.Kind != SectionInstances || sel.IsHeader {
		return nil
	}
	idx := sel.ItemIndex
	if idx < 0 || idx >= len(s.instances) {
		return nil
	}
	target := s.instances[idx]
	if err := target.Kill(); err != nil {
		return fmt.Errorf("could not kill instance: %w", err)
	}
	repoName, err := target.RepoName()
	if err != nil {
		log.ErrorLog.Printf("could not get repo name: %v", err)
	} else {
		s.rmRepo(repoName)
	}
	s.instances = append(s.instances[:idx], s.instances[idx+1:]...)
	s.rebuildVisibleItems()
	return nil
}

// Attach attaches to the selected instance.
func (s *Sidebar) Attach() (chan struct{}, error) {
	inst := s.GetSelectedInstance()
	if inst == nil {
		return nil, fmt.Errorf("no instance selected")
	}
	return inst.Attach()
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
func (s *Sidebar) SetSelectedInstance(idx int) {
	if idx >= len(s.instances) {
		return
	}
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

// GetInstances returns all instances.
func (s *Sidebar) GetInstances() []*session.Instance {
	return s.instances
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
func (s *Sidebar) RemoveInstanceByTitle(title string) bool {
	for i, inst := range s.instances {
		if inst.Title == title {
			repoName, err := inst.RepoName()
			if err != nil {
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

// String renders the sidebar.
func (s *Sidebar) String() string {
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
	b.WriteString("\n")

	for i, item := range s.visibleItems {
		isSelected := i == s.selectedIdx
		if item.IsHeader {
			b.WriteString(s.renderHeader(item.Kind, isSelected))
		} else {
			switch item.Kind {
			case SectionInstances:
				b.WriteString(s.renderInstance(item.ItemIndex, isSelected))
			}
		}
		b.WriteString("\n")
	}

	return lipgloss.Place(s.width, s.height, lipgloss.Left, lipgloss.Top, b.String())
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
		text = runewidth.Truncate(text, w, "...")
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
