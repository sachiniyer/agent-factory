package ui

import (
	"claude-squad/log"
	"claude-squad/schedule"
	"claude-squad/session"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// SidebarSectionKind identifies the type of sidebar section.
type SidebarSectionKind int

const (
	SectionInstances SidebarSectionKind = iota
	SectionSchedules
	SectionTodos
	SectionMicroClaw
)

// SidebarItem represents one visible row in the sidebar.
type SidebarItem struct {
	Kind      SidebarSectionKind
	IsHeader  bool
	ItemIndex int // index within the section's children (instances/schedules)
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

var sidebarScheduleStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#9C9494"})

var sidebarScheduleSelectedStyle = lipgloss.NewStyle().
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
	schedules []schedule.Schedule
	taskCount int
	hasMC     bool // microclaw available

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
			{Kind: SectionSchedules, Title: "Schedules", Expanded: false},
			{Kind: SectionTodos, Title: "Todos", Expanded: false},
			{Kind: SectionMicroClaw, Title: "MicroClaw", Expanded: false},
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
		if !item.Started() || item.Paused() {
			continue
		}
		if innerErr := item.SetPreviewSize(width, height); innerErr != nil {
			err = fmt.Errorf("could not set preview size for instance %d: %v", i, innerErr)
		}
	}
	return err
}

// SetSchedules updates the schedule data.
func (s *Sidebar) SetSchedules(schedules []schedule.Schedule) {
	s.schedules = schedules
	s.rebuildVisibleItems()
}

// SetTaskCount updates the displayed task count.
func (s *Sidebar) SetTaskCount(count int) {
	s.taskCount = count
	s.rebuildVisibleItems()
}

// SetMicroClawAvailable sets whether MicroClaw is available.
func (s *Sidebar) SetMicroClawAvailable(available bool) {
	s.hasMC = available
	s.rebuildVisibleItems()
}

// GetSchedules returns the current schedules.
func (s *Sidebar) GetSchedules() []schedule.Schedule {
	return s.schedules
}

// rebuildVisibleItems rebuilds the flat list of visible items based on expand/collapse state.
func (s *Sidebar) rebuildVisibleItems() {
	var items []SidebarItem
	for _, sec := range s.sections {
		// Skip MicroClaw section if not available
		if sec.Kind == SectionMicroClaw && !s.hasMC {
			continue
		}
		items = append(items, SidebarItem{Kind: sec.Kind, IsHeader: true})
		if sec.Expanded {
			switch sec.Kind {
			case SectionInstances:
				for i := range s.instances {
					items = append(items, SidebarItem{Kind: SectionInstances, ItemIndex: i})
				}
			case SectionSchedules:
				for i := range s.schedules {
					items = append(items, SidebarItem{Kind: SectionSchedules, ItemIndex: i})
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

// Kill kills the selected instance.
func (s *Sidebar) Kill() {
	sel := s.GetSelection()
	if sel.Kind != SectionInstances || sel.IsHeader {
		return
	}
	idx := sel.ItemIndex
	if idx < 0 || idx >= len(s.instances) {
		return
	}
	target := s.instances[idx]
	if err := target.Kill(); err != nil {
		log.ErrorLog.Printf("could not kill instance: %v", err)
	}
	repoName, err := target.RepoName()
	if err != nil {
		log.ErrorLog.Printf("could not get repo name: %v", err)
	} else {
		s.rmRepo(repoName)
	}
	s.instances = append(s.instances[:idx], s.instances[idx+1:]...)
	s.rebuildVisibleItems()
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
			titleWidth, 1, lipgloss.Left, lipgloss.Bottom, mainTitle.Render(" Claude Squad ")))
	} else {
		title := lipgloss.Place(
			titleWidth/2, 1, lipgloss.Left, lipgloss.Bottom, mainTitle.Render(" Claude Squad "))
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
			case SectionSchedules:
				b.WriteString(s.renderSchedule(item.ItemIndex, isSelected))
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
	case SectionSchedules:
		label = fmt.Sprintf("Schedules (%d)", len(s.schedules))
	case SectionTodos:
		if s.taskCount > 0 {
			label = fmt.Sprintf("Todos (%d)", s.taskCount)
		} else {
			label = "Todos"
		}
		arrow = "  " // no expand arrow for leaf sections
	case SectionMicroClaw:
		label = "MicroClaw"
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

func (s *Sidebar) renderSchedule(idx int, selected bool) string {
	if idx < 0 || idx >= len(s.schedules) {
		return ""
	}
	sched := s.schedules[idx]
	status := "[✓]"
	if !sched.Enabled {
		status = "[✗]"
	}

	prompt := sched.Prompt
	w := AdjustPreviewWidth(s.width) - 20
	if w > 0 && runewidth.StringWidth(prompt) > w {
		prompt = runewidth.Truncate(prompt, w, "...")
	}

	text := fmt.Sprintf("  %s %s %s", status, sched.CronExpr, prompt)

	style := sidebarScheduleStyle
	if selected {
		style = sidebarScheduleSelectedStyle
	}
	return style.Padding(0, 1).Render(text)
}
