package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// SidebarSectionKind identifies the type of sidebar section. Since the layout
// cutover (#1024 PR 4) the left rail is the instances tree only — tasks moved
// to the automations strip and hooks behind an overlay — so Instances is the
// sole section; the kind survives on SidebarItem so selection reads stay
// structured.
type SidebarSectionKind int

const (
	SectionInstances SidebarSectionKind = iota
	// SectionArchived holds archived sessions (#1028) in a folder pinned at the
	// bottom of the rail, collapsed by default. Its rows are instance rows like
	// the Instances section's (ItemIndex is a projection index), but flat — an
	// archived session has no live tabs and is not attachable. The header is
	// hidden entirely when no session is archived, so the folder only appears
	// once there is something in it.
	SectionArchived
)

// SidebarItem represents one visible row in the sidebar. Within the Instances
// section a non-header row is either an instance row or — the tree dimension
// added by #1024 PR 3 — one of the instance's tab children (IsTab set,
// TabIndex the 0-based tab slot). For tab rows ItemIndex still identifies the
// parent instance, so "which instance is selected" reads uniformly off any
// non-header row.
type SidebarItem struct {
	Kind      SidebarSectionKind
	IsHeader  bool
	ItemIndex int // index within the section's children (instances/tasks)
	IsTab     bool
	TabIndex  int // 0-based tab slot; meaningful only when IsTab
}

// SidebarSection holds state for one collapsible section.
type SidebarSection struct {
	Kind     SidebarSectionKind
	Title    string
	Expanded bool
}

// isInstanceRow reports whether item is a selectable instance row — a non-header
// row in either the Instances or Archived section (#1028). Both carry a
// projection ItemIndex; the difference is only which folder they render under.
func isInstanceRow(item SidebarItem) bool {
	return !item.IsHeader && (item.Kind == SectionInstances || item.Kind == SectionArchived)
}

// partitionByArchived splits projection indices into live and archived, in the
// projection's stable order (#1028).
func partitionByArchived(instances []*session.Instance) (live, archived []int) {
	for i, inst := range instances {
		if inst != nil && inst.GetStatus() == session.Archived {
			archived = append(archived, i)
		} else {
			live = append(live, i)
		}
	}
	return live, archived
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

var mainTitle = lipgloss.NewStyle().
	Background(AccentColor).
	Foreground(lipgloss.Color("230"))

// blurredTitle is the title chip with tree focus elsewhere (#1024 PR 4 focus
// ring): same shape, receded color.
var blurredTitle = lipgloss.NewStyle().
	Background(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#555555"}).
	Foreground(lipgloss.Color("230"))

var autoYesStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#dde4f0")).
	Foreground(lipgloss.Color("#1a1a1a"))

// Sidebar is the unified left navigation pane with collapsible sections. It is
// a VIEW over the store.Projection (#1024 PR 2): the instance/task/hook data —
// and the cross-pane selection — live in the store, written by the snapshot
// reconcile and the session-control handlers. The sidebar owns only its local
// UI state: the flattened row list, the cursor over it, expansion state, and
// the scroll window.
//
// Since #1024 PR 3 the Instances section is a TREE: each instance row carries
// its tabs as expandable children (the same slots the tab bar shows, via
// tree.TabLabels), and the cursor can rest on an instance row OR a tab row.
// Expansion policy: the display-selected instance auto-expands and every other
// instance collapses, keeping the row count ≈ instances + selected-instance
// tabs; h/← collapses the selected instance explicitly (treeCollapsed) until
// l/→ re-expands it or the selection moves on. Landing the cursor on a tab row
// drives the store's active tab, which is what retargets the content pane.
type Sidebar struct {
	proj *store.Projection

	sections     []SidebarSection
	visibleItems []SidebarItem
	selectedIdx  int

	// seenSig is the structure signature (structureSig) visibleItems was last
	// derived from. The store is written directly by event-loop handlers
	// without notifying the sidebar, so every public method lazily re-derives
	// via syncFromStore before touching the row list. A plain store version is
	// no longer enough (pre-PR 3 seenVersion): the tree's row STRUCTURE also
	// depends on per-instance state mutated in place without a version bump —
	// the selected instance's tab slots (an out-of-band tab create/close
	// reconciles tabs onto the same pointer) and its transient-ness (a kill
	// flips it to Deleting, force-collapsing it) — so those inputs are folded
	// into the signature. seenSelSeq tracks the store's SelectInstance
	// assertions the cursor has honored.
	seenSig    string
	seenSelSeq uint64

	// treeCollapsed is the title of the display-selected instance whose tab
	// children the user explicitly collapsed (h/←). Cleared when the selection
	// moves to a different instance, so every newly selected instance starts
	// auto-expanded (collapse-by-default applies to non-selected instances).
	treeCollapsed string

	// lastCursorTitle/lastCursorTab remember the identity of the last
	// instance-section row the cursor rested on (tab -1 = the instance row
	// itself). The reconcile's #969 re-pin asserts only the instance; this memo
	// restores the tab SUB-selection too, so a snapshot arriving while the
	// cursor sits on a tab row doesn't yank it up to the instance row.
	lastCursorTitle string
	lastCursorTab   int

	// scrollOffset is the index into visibleItems of the first rendered row
	// when the list is too tall for the allocation. It is adjusted lazily in
	// String() (rebuilds can shift indices between renders), scrolling
	// minimally so the selected row stays visible.
	scrollOffset int

	// Rendering
	renderer *tree.InstanceRenderer
	autoyes  bool
	height   int
	width    int
	focused  bool

	// rect is the pane's screen rect (SetRect); zone registration needs the
	// absolute origin, not just the size.
	rect layout.Rect
	// zones is the shared mouse hit-test registry (#1024 R4). String()
	// registers a rect per interactive row every frame; nil (ui unit tests
	// that never wire a registry) skips registration.
	zones *zones.Registry
}

// NewSidebar creates a new sidebar rendering the given projection.
func NewSidebar(spin *spinner.Model, autoYes bool, proj *store.Projection) *Sidebar {
	s := &Sidebar{
		proj: proj,
		sections: []SidebarSection{
			{Kind: SectionInstances, Title: "Instances", Expanded: true},
			// Archived (#1028): pinned last, collapsed by default. Rendered only
			// when it holds archived sessions (rebuildVisibleItems skips the empty
			// folder).
			{Kind: SectionArchived, Title: "Archived", Expanded: false},
		},
		renderer:      tree.NewInstanceRenderer(spin),
		autoyes:       autoYes,
		lastCursorTab: -1,
	}
	s.rebuildVisibleItems()
	s.seenSig = s.structureSig()
	return s
}

// SetSize sets the display dimensions.
func (s *Sidebar) SetSize(width, height int) {
	s.width = width
	s.height = height
	s.renderer.SetWidth(s.contentWidth())
}

// contentWidth is the effective row width inside the sidebar allocation: the
// full width minus the 2-cell row padding the row styles add. This replaces
// the pre-cutover AdjustPreviewWidth 0.9 buffer (#1024 PR 4) — the tree gets
// its whole rect.
func (s *Sidebar) contentWidth() int {
	w := s.width - 2
	if w < 0 {
		w = 0
	}
	return w
}

// SetRect implements layout.Pane.
func (s *Sidebar) SetRect(r layout.Rect) {
	s.rect = r
	s.SetSize(r.W, r.H)
}

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 R4).
func (s *Sidebar) SetZoneRegistry(reg *zones.Registry) {
	s.zones = reg
}

// Focused implements layout.Pane.
func (s *Sidebar) Focused() bool { return s.focused }

// Focus implements layout.Pane.
func (s *Sidebar) Focus() { s.focused = true }

// Blur implements layout.Pane.
func (s *Sidebar) Blur() { s.focused = false }

// HandleKey implements layout.Pane. Tree navigation stays routed through the
// root model's global bindings in PR 4 (the keys also work when the workspace
// pane has focus); per-pane routing arrives with the split (#1024 PR 5).
func (s *Sidebar) HandleKey(tea.KeyMsg) (tea.Cmd, bool) { return nil, false }

// HandleMouse implements layout.Pane. Mouse dispatch is zone-id-based at the
// root (#1024 R4): String() registers per-row zones and the root router
// calls the click primitives (SelectTabRow, ToggleInstanceTree, …) directly,
// so the pane-local fallback consumes nothing.
func (s *Sidebar) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// View implements layout.Pane.
func (s *Sidebar) View() string { return s.String() }

// structureSig fingerprints every input the flattened row list is derived
// from: the store version (instance list, tasks, selection binding) plus the
// tree-structural per-instance state that mutates in place without a version
// bump — the selected instance's identity, tab-slot count, and expandability —
// and the explicit collapse override. See the seenSig field.
func (s *Sidebar) structureSig() string {
	selTitle := ""
	slots := 0
	expandable := false
	if inst := s.proj.GetSelectedInstance(); inst != nil {
		selTitle = inst.Title
		slots = len(tree.TabLabels(inst))
		expandable = tree.Expandable(inst)
	}
	// Fold in the archived partition (#1028): a status flip to/from Archived
	// changes which folder a row belongs to but does NOT bump the projection
	// Version (it mutates an existing instance in place), so without this the
	// sidebar would not re-partition until the next structural change. The
	// archived index set — stable within a projection order — uniquely
	// identifies the partition, so it catches both a single archive/restore and
	// a same-count swap.
	_, archived := partitionByArchived(s.proj.GetInstances())
	return fmt.Sprintf("%d|%s|%d|%t|%s|%v",
		s.proj.Version(), selTitle, slots, expandable, s.treeCollapsed, archived)
}

// instanceExpanded reports whether inst's tab children are currently shown:
// the display-selected instance auto-expands (matched by title so a #765
// same-title swap keeps the subtree open) unless it is transient or the user
// explicitly collapsed it. Everything else is collapsed — the keep-row-count-
// manageable default for non-selected instances.
func (s *Sidebar) instanceExpanded(inst *session.Instance) bool {
	sel := s.proj.GetSelectedInstance()
	if sel == nil || inst == nil || sel.Title != inst.Title {
		return false
	}
	return tree.Expandable(inst) && s.treeCollapsed != inst.Title
}

// syncFromStore re-derives the flattened row list when the projection (or the
// tree-structural inputs, see structureSig) changed since the last derivation,
// then re-pins the cursor. The cursor rules preserve the pre-store mutation
// behavior exactly:
//   - by default the cursor keeps its flat index, clamped by the rebuild —
//     the same drift-then-clamp the old mutation-time rebuild applied (a
//     removal can legitimately drift the cursor, e.g. onto a header while
//     naming an instance — the #717 reality the cancel path defends against);
//   - a pending SelectInstance assertion (the reconcile's #969 re-pin, the
//     search overlay, an explicit re-select) moves the cursor onto the
//     asserted instance's row — restoring the tab sub-selection when the
//     cursor was on one of its tab rows (lastCursorTab); a dangling or nil
//     assertion leaves the clamped cursor as-is, matching the old "selected
//     title gone → sidebar clamp behavior" rule.
//
// Afterwards the cursor's row (if it rests on an instance or tab row) is
// pushed back into the store so the display binding, the active tab, and the
// cursor can never disagree at a read boundary.
func (s *Sidebar) syncFromStore() {
	if s.structureSig() == s.seenSig && s.proj.SelectionSeq() == s.seenSelSeq {
		return
	}
	s.rebuildVisibleItems()
	if s.proj.SelectionSeq() != s.seenSelSeq {
		if inst := s.proj.GetSelectedInstance(); inst != nil {
			s.moveCursorToInstance(inst)
		}
	}
	s.afterCursorMove()
}

// afterCursorMove pushes the cursor's selection into the store and folds the
// resulting signature/selection movement into the seen markers (safe on the
// single-threaded event loop; nothing else can write between).
func (s *Sidebar) afterCursorMove() {
	s.pushSelection()
	s.seenSig = s.structureSig()
	s.seenSelSeq = s.proj.SelectionSeq()
}

// moveCursorToInstance places the cursor on the row of target, if it is in
// the projection and its row is visible. When the cursor last rested on one of
// target's tab rows (lastCursorTab), the same tab row is preferred so the #969
// re-pin keeps the tab dimension; a vanished tab slot falls back to the
// instance row. No-op otherwise.
func (s *Sidebar) moveCursorToInstance(target *session.Instance) {
	instIdx := -1
	for i, inst := range s.proj.GetInstances() {
		if inst == target {
			instIdx = i
			break
		}
	}
	if instIdx < 0 {
		return
	}
	wantTab := -1
	if s.lastCursorTitle == target.Title {
		wantTab = s.lastCursorTab
	}
	instRow := -1
	for j, item := range s.visibleItems {
		if item.Kind != SectionInstances || item.IsHeader || item.ItemIndex != instIdx {
			continue
		}
		if item.IsTab {
			if item.TabIndex == wantTab {
				s.selectedIdx = j
				return
			}
			continue
		}
		instRow = j
	}
	if instRow >= 0 {
		s.selectedIdx = instRow
	}
}

// pushSelection records the cursor's instance (when it rests on an instance or
// tab row) as the store's display binding, and — the tab dimension — a tab
// row's slot as the store's active tab, which is what makes tree tab selection
// drive the content pane's tabbed window. Header rows deliberately do NOT
// clear the binding: the workspace panes keep displaying the last selected
// instance while the cursor tours headers, exactly as the pre-store
// TabbedWindow kept its instance pointer.
//
// When the binding moves to a DIFFERENT instance the explicit collapse
// override resets and the row list is re-derived in place (the new selection
// auto-expands, the old one folds), preserving the cursor by row identity —
// the fold can shift flat indices, but never which row the user is on.
func (s *Sidebar) pushSelection() {
	sel := s.rawSelection()
	if sel.Kind != SectionInstances || sel.IsHeader {
		return
	}
	instances := s.proj.GetInstances()
	if sel.ItemIndex < 0 || sel.ItemIndex >= len(instances) {
		return
	}
	inst := instances[sel.ItemIndex]
	prev := s.proj.GetSelectedInstance()
	s.proj.SetSelectedInstance(inst)
	if sel.IsTab {
		s.proj.SetActiveTab(sel.TabIndex)
		s.lastCursorTab = sel.TabIndex
	} else {
		s.lastCursorTab = -1
	}
	s.lastCursorTitle = inst.Title
	if prev != inst {
		s.treeCollapsed = ""
		s.rebuildPreservingCursor()
	}
}

// rowIdentity is the rebuild-stable identity of a visible row: instances are
// keyed by title (stable across index shifts and #765 same-title swaps), tab
// rows by title + slot.
type rowIdentity struct {
	kind   SidebarSectionKind
	header bool
	title  string
	isTab  bool
	tab    int
}

func (s *Sidebar) rowIdentityAt(item SidebarItem) rowIdentity {
	id := rowIdentity{kind: item.Kind, header: item.IsHeader, isTab: item.IsTab, tab: item.TabIndex}
	if item.Kind == SectionInstances && !item.IsHeader {
		instances := s.proj.GetInstances()
		if item.ItemIndex >= 0 && item.ItemIndex < len(instances) {
			id.title = instances[item.ItemIndex].Title
		}
	}
	return id
}

// rebuildPreservingCursor re-derives the row list and keeps the cursor on the
// same logical row (by rowIdentity) when it still exists; otherwise the
// rebuild's index clamp applies.
func (s *Sidebar) rebuildPreservingCursor() {
	id := s.rowIdentityAt(s.rawSelection())
	s.rebuildVisibleItems()
	instances := s.proj.GetInstances()
	for j, item := range s.visibleItems {
		if item.Kind != id.kind || item.IsHeader != id.header || item.IsTab != id.isTab {
			continue
		}
		if item.IsHeader {
			s.selectedIdx = j
			return
		}
		if item.Kind == SectionInstances {
			if item.ItemIndex < 0 || item.ItemIndex >= len(instances) ||
				instances[item.ItemIndex].Title != id.title {
				continue
			}
			if item.IsTab && item.TabIndex != id.tab {
				continue
			}
		}
		s.selectedIdx = j
		return
	}
}

// rebuildVisibleItems rebuilds the flat list of visible items based on
// expand/collapse state: section headers, then — for the expanded Instances
// section — the tree rows (instances with tab children for expanded ones).
func (s *Sidebar) rebuildVisibleItems() {
	instances := s.proj.GetInstances()

	// Partition projection indices into live and archived (#1028), preserving
	// order. ItemIndex stays a projection index in both sections so every
	// downstream resolve (GetSelectedInstance, render) indexes GetInstances()
	// uniformly.
	live, archived := partitionByArchived(instances)

	var items []SidebarItem
	for _, sec := range s.sections {
		// The Archived folder only appears once it holds something: no empty
		// "Archived (0)" header cluttering the rail.
		if sec.Kind == SectionArchived && len(archived) == 0 {
			continue
		}
		items = append(items, SidebarItem{Kind: sec.Kind, IsHeader: true})
		if !sec.Expanded {
			continue
		}
		switch sec.Kind {
		case SectionInstances:
			rows := tree.Flatten(len(live),
				func(i int) bool { return s.instanceExpanded(instances[live[i]]) },
				func(i int) int { return len(tree.TabLabels(instances[live[i]])) })
			for _, r := range rows {
				item := SidebarItem{Kind: SectionInstances, ItemIndex: live[r.InstanceIndex]}
				if r.IsTab() {
					item.IsTab = true
					item.TabIndex = r.TabIndex
				}
				items = append(items, item)
			}
		case SectionArchived:
			// Flat rows — archived sessions have no live tabs.
			for _, idx := range archived {
				items = append(items, SidebarItem{Kind: SectionArchived, ItemIndex: idx})
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

// Down moves the cursor down, walking through expanded tab children.
func (s *Sidebar) Down() {
	s.syncFromStore()
	if s.selectedIdx < len(s.visibleItems)-1 {
		s.selectedIdx++
	}
	s.afterCursorMove()
}

// Up moves the cursor up, walking through expanded tab children.
func (s *Sidebar) Up() {
	s.syncFromStore()
	if s.selectedIdx > 0 {
		s.selectedIdx--
	}
	s.afterCursorMove()
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
	s.afterCursorMove()
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

// ExpandSection expands the tree node under the cursor: on a section header
// the whole section; on an instance row the instance's tab children (clearing
// an explicit h/← collapse). Tab rows are already inside an expanded node, so
// they no-op.
func (s *Sidebar) ExpandSection() {
	s.syncFromStore()
	sel := s.rawSelection()
	if sel.Kind == SectionInstances && !sel.IsHeader && !sel.IsTab {
		if s.treeCollapsed != "" {
			s.treeCollapsed = ""
			s.rebuildPreservingCursor()
		}
		s.afterCursorMove()
		return
	}
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
	s.afterCursorMove()
}

// CollapseSection collapses the tree node under the cursor: a tab row folds
// its parent instance (cursor moves up onto it); an expanded instance row
// folds its tab children in place; an already-collapsed instance row falls
// back to the pre-tree behavior — jump to the section header and collapse the
// whole section. Headers collapse their section as before.
func (s *Sidebar) CollapseSection() {
	s.syncFromStore()
	sel := s.rawSelection()
	if sel.Kind == SectionInstances && !sel.IsHeader {
		instances := s.proj.GetInstances()
		if sel.ItemIndex >= 0 && sel.ItemIndex < len(instances) {
			inst := instances[sel.ItemIndex]
			if s.instanceExpanded(inst) {
				s.treeCollapsed = inst.Title
				if sel.IsTab {
					// Fold to the parent: the cursor lands on the instance row.
					s.rebuildVisibleItems()
					s.moveCursorToInstanceRow(sel.ItemIndex)
				} else {
					s.rebuildPreservingCursor()
				}
				s.afterCursorMove()
				return
			}
		}
	}
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
	s.afterCursorMove()
}

// moveCursorToInstanceRow places the cursor on the instance row (not a tab
// child) of the instance at instIdx, if visible.
func (s *Sidebar) moveCursorToInstanceRow(instIdx int) {
	for j, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && !item.IsTab && item.ItemIndex == instIdx {
			s.selectedIdx = j
			return
		}
	}
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

// GetSelectedInstance returns the instance under the cursor — including when
// the cursor rests on one of its tab rows — or nil when the cursor rests on a
// section header. Note this is the cursor-derived selection; the store's
// GetSelectedInstance is the sticky display selection the workspace panes
// render.
func (s *Sidebar) GetSelectedInstance() *session.Instance {
	s.syncFromStore()
	sel := s.rawSelection()
	// An archived row (#1028) is a real instance row too, so it resolves here —
	// this is what lets the restore action and the Enter "restore it first"
	// fence read the selected archived session.
	if !isInstanceRow(sel) {
		return nil
	}
	instances := s.proj.GetInstances()
	if sel.ItemIndex < 0 || sel.ItemIndex >= len(instances) {
		return nil
	}
	return instances[sel.ItemIndex]
}

// SetSelectedInstance sets the cursor to point at the given instance index.
// The section holding the target (Instances, or the Archived folder for an
// archived session, #1028) is expanded first so the target row is always
// reachable.
func (s *Sidebar) SetSelectedInstance(idx int) {
	s.syncFromStore()
	instances := s.proj.GetInstances()
	if idx < 0 || idx >= len(instances) {
		return
	}
	// Expand whichever section holds the target so its row is part of
	// visibleItems; otherwise the search below would silently no-op.
	if instances[idx].GetStatus() == session.Archived {
		s.expandSectionKind(SectionArchived)
	} else {
		s.ExpandInstancesSection()
	}
	// Find the visible instance row (either section) for this projection index.
	for i, item := range s.visibleItems {
		if isInstanceRow(item) && !item.IsTab && item.ItemIndex == idx {
			s.selectedIdx = i
			break
		}
	}
	s.afterCursorMove()
}

// expandSectionKind expands the given section and rebuilds. Shared by
// SetSelectedInstance's Instances/Archived branches.
func (s *Sidebar) expandSectionKind(kind SidebarSectionKind) {
	for i, sec := range s.sections {
		if sec.Kind == kind {
			s.sections[i].Expanded = true
			break
		}
	}
	s.rebuildVisibleItems()
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

// SyncCursorToActiveTab moves the cursor onto the selected instance's
// active-tab row after a tab jump/cycle/create/close changed the active tab —
// but only when the cursor was already resting on one of its tab rows, so
// tab-key muscle memory with the cursor on the instance row behaves exactly as
// before the tree (the cursor stays put; the tree's "*" marker moves).
//
// The intended target MUST be captured before the row list is rebuilt: when
// the tab-slot count changed (t create / w close), the structure rebuild fires
// and its blind index clamp drifts the stale tab-row flat index. Two failures
// follow if that drift is allowed to reach the store: pushSelection re-asserts
// the drifted row's tab index, clobbering the active tab the caller just set
// (t didn't select the new tab and a following w silently closed the wrong tab
// — PR #1081 play-test); and, when a TRAILING instance sits just below, closing
// the last tab shrinks the list so the drift lands on that neighbor's row —
// pushSelection then commits it as the display selection, folding the acting
// instance's subtree so the re-pin can no longer find its tab rows (#1084).
//
// Both are avoided by rebuilding here — while the store still selects the
// acting instance, so its subtree stays expanded — and re-pinning the cursor
// by captured title BEFORE any selection is pushed, rather than routing through
// syncFromStore and trying to undo the drift afterward. Bare tab jumps/cycles
// don't change the structure, so the rebuild is a no-op for them; both paths go
// through the same capture-first flow. Any pending #969 SelectInstance
// assertion has already been consumed by the GetSelectedInstance sync at the
// top of the tab handler, so bypassing syncFromStore here drops nothing.
func (s *Sidebar) SyncCursorToActiveTab() {
	want := s.proj.ActiveTab()
	pre := s.rawSelection()
	if pre.Kind != SectionInstances || pre.IsHeader || !pre.IsTab {
		return
	}
	instances := s.proj.GetInstances()
	if pre.ItemIndex < 0 || pre.ItemIndex >= len(instances) {
		return
	}
	// Key the parent instance by title: the rebuild below re-derives the row
	// list and flat/instance indices are only stable across it by identity.
	title := instances[pre.ItemIndex].Title
	s.rebuildVisibleItems()
	instances = s.proj.GetInstances()
	found := false
	for j, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && item.IsTab &&
			item.ItemIndex >= 0 && item.ItemIndex < len(instances) &&
			instances[item.ItemIndex].Title == title && item.TabIndex == want {
			s.selectedIdx = j
			found = true
			break
		}
	}
	if found {
		// Re-assert the captured target: the rebuild's clamp may have moved the
		// cursor, and afterCursorMove's pushSelection reads it back.
		s.proj.SetActiveTab(want)
	} else {
		// The wanted tab row is gone (e.g. a concurrent reconcile shrank the tab
		// set). Fall back to the acting instance's own row so the cursor stays
		// within its subtree instead of drifting onto a neighbor; if the
		// instance itself vanished, no row matches and the clamp position wins.
		for j, item := range s.visibleItems {
			if item.Kind == SectionInstances && !item.IsHeader && !item.IsTab &&
				item.ItemIndex >= 0 && item.ItemIndex < len(instances) &&
				instances[item.ItemIndex].Title == title {
				s.selectedIdx = j
				break
			}
		}
	}
	s.afterCursorMove()
}

// String renders the sidebar. The item list is windowed around the selection
// (#787): lipgloss.Place pads short content but never truncates, so rendering
// every row would overflow the allocation once enough instances exist and push
// the menu/error box below the fold. A blind clamp (the #700 fix for the
// content pane) would be wrong here because the selected row can sit below the
// fold while navigating — instead the rendered slice scrolls so the selection
// is always visible, with "▲/▼ N more" rows marking hidden items.
// chromeLines is the fixed chrome above the sidebar's item list: one leading
// blank line plus the title row. Shared by String()'s window budget and the
// zone registration's y origin so the two can't drift.
const chromeLines = 2

func (s *Sidebar) String() string {
	s.syncFromStore()

	// Render every visible row up front and measure real heights: instance
	// rows are multi-line (title + branch, plus an optional PR line) while tab
	// rows are single-line, so the window math cannot assume one line per item.
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
				if item.IsTab {
					rows[i] = s.renderTabRow(item, isSelected)
				} else {
					rows[i] = s.renderInstance(item.ItemIndex, isSelected)
				}
			case SectionArchived:
				// Archived rows are flat instance rows (#1028) — no tab children.
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

	s.registerZones(heights, start, end, hiddenAbove > 0)

	var b strings.Builder
	b.WriteString("\n")

	// Title bar. Clamp the chip width to the allocation and truncate the text
	// to the chip: lipgloss.Place pads but never clips, so at ultra-narrow
	// widths the 15-cell " Agent Factory " (or the padded chip itself) would
	// otherwise push the row past s.width — the same #646 overflow class the
	// section headers hit. The chip doubles as the tree's focus-ring indicator:
	// the accent background recedes to gray when focus is elsewhere.
	titleWidth := s.contentWidth() + 2
	if s.width > 0 && titleWidth > s.width {
		titleWidth = s.width
	}
	titleChip := mainTitle
	if !s.focused {
		titleChip = blurredTitle
	}
	if !s.autoyes {
		b.WriteString(lipgloss.Place(
			titleWidth, 1, lipgloss.Left, lipgloss.Bottom,
			titleChip.Render(fitTitleText(" Agent Factory ", titleWidth))))
	} else {
		title := lipgloss.Place(
			titleWidth/2, 1, lipgloss.Left, lipgloss.Bottom,
			titleChip.Render(fitTitleText(" Agent Factory ", titleWidth/2)))
		autoYes := lipgloss.Place(
			titleWidth-(titleWidth/2), 1, lipgloss.Right, lipgloss.Bottom,
			autoYesStyle.Render(fitTitleText(" auto-yes ", titleWidth-(titleWidth/2))))
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
	// Safety clamp to the exact allocation via PR 1's shared helper:
	// lipgloss.Place pads but never truncates in either dimension, fitWindow
	// force-includes the first windowed row even when it alone exceeds the
	// height budget (e.g. a tall PR row at a tiny height), and rare row shapes
	// (double-digit index prefixes at ultra-narrow widths) can still exceed
	// the width after the per-row truncation (#646/#700 class).
	if s.width > 0 && s.height > 0 {
		out = layout.ClampToRect(out, layout.Rect{W: s.width, H: s.height})
	} else if s.height > 0 {
		if lines := strings.Split(out, "\n"); len(lines) > s.height {
			out = strings.Join(lines[:s.height], "\n")
		}
	}
	return out
}

// registerZones records the mouse hit-test rects for this frame (#1024 R4):
// the pane background, the section header, and — for every row inside the
// rendered window — the instance/tab row plus the instance's ▸/▾ arrow cell.
// Coordinates mirror String()'s exact line budget (the leading blank + title
// chrome, then the "▲ N more" indicator when the window is scrolled), and
// every zone is clipped to the pane rect since the final ClampToRect clips
// any partially fitting last row the same way.
func (s *Sidebar) registerZones(heights []int, start, end int, topIndicator bool) {
	if s.zones == nil || s.rect.Empty() {
		return
	}
	s.zones.Register(zones.TreeBG, s.rect)

	instances := s.proj.GetInstances()
	y := s.rect.Y + chromeLines
	if topIndicator {
		y++
	}
	bottom := s.rect.Bottom()
	for i := start; i < end && y < bottom; i++ {
		item := s.visibleItems[i]
		h := heights[i]
		if y+h > bottom {
			h = bottom - y
		}
		row := layout.Rect{X: s.rect.X, Y: y, W: s.rect.W, H: h}
		switch {
		case item.IsHeader:
			s.zones.Register(zones.TreeHeader, row)
		case item.Kind == SectionInstances && item.ItemIndex >= 0 && item.ItemIndex < len(instances):
			inst := instances[item.ItemIndex]
			if item.IsTab {
				s.zones.Register(zones.TreeTab(inst.Title, item.TabIndex), row)
			} else {
				s.zones.Register(zones.TreeInstance(inst.Title), row)
				// The arrow cell registers ON TOP of the row (later wins) so
				// clicking it expands/collapses instead of selecting.
				if ax, ay, ok := tree.ArrowCell(s.contentWidth()); ok && tree.Expandable(inst) && ay < h {
					s.zones.Register(zones.TreeArrow(inst.Title),
						layout.Rect{X: s.rect.X + ax, Y: y + ay, W: 1, H: 1})
				}
			}
		}
		y += heights[i]
	}
}

// ClickHeader moves the cursor onto the Instances section header and toggles
// its expansion — the click equivalent of Enter on the header row.
func (s *Sidebar) ClickHeader() {
	s.syncFromStore()
	for i, item := range s.visibleItems {
		if item.IsHeader {
			s.selectedIdx = i
			break
		}
	}
	s.ToggleSection()
}

// SelectTabRow moves the cursor onto tab slot idx of the instance titled
// title — the click action for a tree tab row, which (via pushSelection)
// retargets the workspace onto that tab. Tab rows only render for the
// expanded instance, so a click that races a structure change and finds no
// such row no-ops rather than guessing.
func (s *Sidebar) SelectTabRow(title string, idx int) {
	s.syncFromStore()
	instances := s.proj.GetInstances()
	for j, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && item.IsTab &&
			item.ItemIndex >= 0 && item.ItemIndex < len(instances) &&
			instances[item.ItemIndex].Title == title && item.TabIndex == idx {
			s.selectedIdx = j
			s.afterCursorMove()
			return
		}
	}
}

// ToggleInstanceTree is the click action for an instance row's ▸/▾ arrow: the
// cursor lands on the clicked row (a click always selects), and the arrow
// meaning follows the tree's expansion policy — selecting a different
// instance auto-expands it (that IS the ▸ action), while the already-selected
// instance toggles its explicit h/← collapse override.
func (s *Sidebar) ToggleInstanceTree(title string) {
	s.syncFromStore()
	instances := s.proj.GetInstances()
	instIdx := -1
	for i, inst := range instances {
		if inst.Title == title {
			instIdx = i
			break
		}
	}
	if instIdx < 0 {
		return
	}
	sel := s.proj.GetSelectedInstance()
	wasSelected := sel != nil && sel.Title == title
	wasExpanded := s.instanceExpanded(instances[instIdx])
	s.SetSelectedInstance(instIdx)
	if !wasSelected {
		return
	}
	if wasExpanded {
		s.treeCollapsed = title
	} else {
		s.treeCollapsed = ""
	}
	s.rebuildVisibleItems()
	s.moveCursorToInstanceRow(instIdx)
	s.afterCursorMove()
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
	w := s.contentWidth()
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
	return windowIndicatorStyle.Padding(0, narrowAwarePad(w)).Render(
		lipgloss.Place(w, 1, lipgloss.Left, lipgloss.Center, text))
}

// narrowAwarePad returns the horizontal padding for a one-line sidebar row
// rendered at effective content width w. The 2-cell Padding(0,1) sits OUTSIDE
// the width the row text is truncated to, so at ultra-narrow allocations the
// padded row overflows the sidebar (the #646 overflow class; reproduced by
// T-Rex at SetSize(9,18) on the section header). Drop the padding at the same
// w <= 9 threshold the instance and tab rows already use.
func narrowAwarePad(w int) int {
	if w <= 9 {
		return 0
	}
	return 1
}

// fitTitleText truncates a title-bar chip's text to w cells so lipgloss.Place
// (which pads but never clips) can't push the row past the sidebar allocation
// at ultra-narrow widths.
func fitTitleText(text string, w int) string {
	if w > 0 && runewidth.StringWidth(text) > w {
		return runewidth.Truncate(text, w, "")
	}
	return text
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
	live, archived := partitionByArchived(s.proj.GetInstances())
	switch kind {
	case SectionInstances:
		// Count live sessions only — archived ones live under their own folder.
		label = fmt.Sprintf("Instances (%d)", len(live))
	case SectionArchived:
		label = fmt.Sprintf("Archived (%d)", len(archived))
	}

	style := sectionHeaderStyle
	if selected {
		style = sectionHeaderSelectedStyle
	}

	w := s.contentWidth()
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
	return style.Padding(0, narrowAwarePad(w)).Render(
		lipgloss.Place(w, 1, lipgloss.Left, lipgloss.Center, text))
}

func (s *Sidebar) renderInstance(idx int, selected bool) string {
	instances := s.proj.GetInstances()
	if idx < 0 || idx >= len(instances) {
		return ""
	}
	inst := instances[idx]
	// Pad the index to the digit width of the largest 1-based index in the list
	// so every row's prefix is the same width and the branch/PR lines align,
	// without widening the common small-list case (#871, #923, #939).
	s.renderer.SetIndexWidth(len(strconv.Itoa(len(instances))))
	return s.renderer.Render(inst, idx+1, selected, s.proj.NumRepos() > 1, s.instanceExpanded(inst))
}

// renderTabRow renders one tab child row of an expanded instance. The label
// set comes from tree.TabLabels — the same slots the tab bar shows — and the
// row whose slot matches the store's active tab carries the "*" marker.
func (s *Sidebar) renderTabRow(item SidebarItem, selected bool) string {
	instances := s.proj.GetInstances()
	if item.ItemIndex < 0 || item.ItemIndex >= len(instances) {
		return ""
	}
	labels := tree.TabLabels(instances[item.ItemIndex])
	if item.TabIndex < 0 || item.TabIndex >= len(labels) {
		return ""
	}
	s.renderer.SetIndexWidth(len(strconv.Itoa(len(instances))))
	return s.renderer.RenderTab(labels[item.TabIndex], item.TabIndex+1,
		item.TabIndex == len(labels)-1, selected, s.proj.ActiveTab() == item.TabIndex)
}
