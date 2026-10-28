package ui

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
		if s.treeCollapsed != nil {
			s.treeCollapsed = nil
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
				s.treeCollapsed = inst
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
