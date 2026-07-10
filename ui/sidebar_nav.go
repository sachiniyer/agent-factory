package ui

import (
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

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
		if !isInstanceRow(item) || item.ItemIndex != instIdx {
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

type sidebarTabStop struct {
	itemIndex int
	tabIndex  int
}

type sidebarNavStop struct {
	isTab      bool
	tab        sidebarTabStop
	visibleIdx int
}

// liveTabStops returns the logical vertical-nav stops for the live Instances
// tree. Instance title rows remain visible structural rows, but j/k/up/down
// stop only on tab rows; archived rows are a separate flat section and keep
// their existing selection behavior.
func (s *Sidebar) liveTabStops() []sidebarTabStop {
	instances := s.proj.GetInstances()
	live, _ := partitionByArchived(instances)
	stops := make([]sidebarTabStop, 0, len(live))
	for _, idx := range live {
		inst := instances[idx]
		if !tree.Expandable(inst) {
			continue
		}
		for tab := range tree.TabLabels(inst) {
			stops = append(stops, sidebarTabStop{itemIndex: idx, tabIndex: tab})
		}
	}
	return stops
}

func (s *Sidebar) verticalNavStops() []sidebarNavStop {
	tabStops := s.liveTabStops()
	stops := make([]sidebarNavStop, 0, len(tabStops))
	for _, stop := range tabStops {
		stops = append(stops, sidebarNavStop{isTab: true, tab: stop})
	}
	for i, item := range s.visibleItems {
		if isSelectableNonTabNavStop(item) {
			stops = append(stops, sidebarNavStop{visibleIdx: i})
		}
	}
	return stops
}

func isSelectableNonTabNavStop(item SidebarItem) bool {
	return !item.IsHeader && !item.IsTab && item.Kind == SectionArchived
}

func indexOfNavStop(stops []sidebarNavStop, selectedIdx int, sel SidebarItem) int {
	for i, stop := range stops {
		if stop.isTab {
			if sel.Kind == SectionInstances && sel.IsTab &&
				stop.tab.itemIndex == sel.ItemIndex && stop.tab.tabIndex == sel.TabIndex {
				return i
			}
			continue
		}
		if stop.visibleIdx == selectedIdx && isSelectableNonTabNavStop(sel) {
			return i
		}
	}
	return -1
}

func firstNavStopAtOrAfterInstance(stops []sidebarNavStop, itemIndex int) int {
	for i, stop := range stops {
		if !stop.isTab || stop.tab.itemIndex >= itemIndex {
			return i
		}
	}
	return -1
}

func lastTabStopBeforeInstance(stops []sidebarNavStop, itemIndex int) int {
	last := -1
	for i, stop := range stops {
		if !stop.isTab {
			break
		}
		if stop.tab.itemIndex < itemIndex {
			last = i
		}
	}
	return last
}

func (s *Sidebar) selectTabStop(stop sidebarTabStop) bool {
	instances := s.proj.GetInstances()
	if stop.itemIndex < 0 || stop.itemIndex >= len(instances) {
		return false
	}
	inst := instances[stop.itemIndex]
	if !tree.Expandable(inst) {
		return false
	}
	labels := tree.TabLabels(inst)
	if stop.tabIndex < 0 || stop.tabIndex >= len(labels) {
		return false
	}

	s.proj.SetSelectedInstance(inst)
	s.proj.SetActiveTab(stop.tabIndex)
	s.treeCollapsed = ""
	for i, sec := range s.sections {
		if sec.Kind == SectionInstances {
			s.sections[i].Expanded = true
			break
		}
	}
	s.rebuildVisibleItems()
	for j, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && item.IsTab &&
			item.ItemIndex == stop.itemIndex && item.TabIndex == stop.tabIndex {
			s.selectedIdx = j
			return true
		}
	}
	return false
}

func (s *Sidebar) selectNavStop(stop sidebarNavStop) bool {
	if stop.isTab {
		return s.selectTabStop(stop.tab)
	}
	if stop.visibleIdx < 0 || stop.visibleIdx >= len(s.visibleItems) {
		return false
	}
	if !isSelectableNonTabNavStop(s.visibleItems[stop.visibleIdx]) {
		return false
	}
	s.selectedIdx = stop.visibleIdx
	return true
}

func (s *Sidebar) moveVerticalNavStop(dir int) {
	stops := s.verticalNavStops()
	if len(stops) == 0 {
		return
	}

	sel := s.rawSelection()
	if sel.IsHeader {
		switch sel.Kind {
		case SectionInstances:
			if dir > 0 {
				s.selectNavStop(stops[0])
			}
			return
		case SectionArchived:
			if dir < 0 {
				for i := len(stops) - 1; i >= 0; i-- {
					if stops[i].isTab {
						s.selectNavStop(stops[i])
						return
					}
				}
			} else {
				for _, stop := range stops {
					if !stop.isTab && stop.visibleIdx > s.selectedIdx {
						s.selectNavStop(stop)
						return
					}
				}
			}
		}
		return
	}

	target := -1
	if cur := indexOfNavStop(stops, s.selectedIdx, sel); cur >= 0 {
		target = cur + dir
	} else if sel.Kind == SectionInstances && !sel.IsTab {
		if dir > 0 {
			target = firstNavStopAtOrAfterInstance(stops, sel.ItemIndex)
		} else {
			target = lastTabStopBeforeInstance(stops, sel.ItemIndex)
		}
	}

	if target < 0 || target >= len(stops) {
		return
	}
	s.selectNavStop(stops[target])
}

// Down moves the cursor down through live tab stops, skipping instance titles.
func (s *Sidebar) Down() {
	s.syncFromStore()
	s.moveVerticalNavStop(1)
	s.afterCursorMove()
}

// Up moves the cursor up through live tab stops, skipping instance titles.
func (s *Sidebar) Up() {
	s.syncFromStore()
	s.moveVerticalNavStop(-1)
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
	// Expand whichever section holds the target so its row is part of visibleItems;
	// otherwise the search below would silently no-op (#1210: ShownArchived match).
	if instances[idx].ShownArchived() {
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

// ClickHeaderKind moves the cursor onto the header of the given section kind and
// toggles that section (#1028) — the click equivalent of Enter on that header.
// Unlike ClickHeader (which targets the first/Instances header), this lets a
// click on the Archived folder header toggle the Archived folder specifically.
func (s *Sidebar) ClickHeaderKind(kind SidebarSectionKind) {
	s.syncFromStore()
	for i, item := range s.visibleItems {
		if item.IsHeader && item.Kind == kind {
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
