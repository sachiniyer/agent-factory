package ui

import "github.com/sachiniyer/agent-factory/ui/tree"

// builtCursorRow names the row the cursor rests on in the list as last built,
// resolving its instance through that same build's instance list
// (rowInstances), so it stays exact however the projection changed since.
// Read before a rebuild, it is the row the user last saw selected.
func (s *Sidebar) builtCursorRow() rowIdentity {
	item := s.rawSelection()
	id := rowIdentity{kind: item.Kind, header: item.IsHeader, isTab: item.IsTab, tab: item.TabIndex}
	if item.Kind == SectionInstances && !item.IsHeader &&
		item.ItemIndex >= 0 && item.ItemIndex < len(s.rowInstances) {
		id.inst = s.rowInstances[item.ItemIndex]
	}
	return id
}

// followCursorRow keeps the cursor on its own session when a rebuild moves the
// session's row out from under the cursor's flat index (#7229). The index is
// the right re-pin when rows appear or vanish around the cursor, and when the
// cursor's own row is gone (#717). It is wrong when the session is still here
// and only its row moved: the index then names some other row, pushSelection
// hands that row the selection, and where the cursor lands depends on what
// happened to sit below it. Two such moves happen to the cursor's own session:
//
//   - It was archived, so its row re-homed from Sessions into the Archived
//     folder. The cursor follows it there and the folder opens, so an archive
//     lands on the archived row whether or not an earlier visit had left the
//     folder open.
//   - An in-flight op (archive, kill, respawn…) made it unexpandable while the
//     cursor was on one of its tab rows, folding them away. The cursor moves up
//     onto the session's own row, the landing h/← gives a tab row.
//
// Every other move keeps the clamped index. A pending SelectInstance assertion
// runs after this and still wins.
func (s *Sidebar) followCursorRow(prev rowIdentity) {
	if prev.inst == nil || prev.kind != SectionInstances || prev.header {
		return
	}
	if s.rowIdentityAt(s.rawSelection()) == prev {
		return
	}
	idx := -1
	for i, inst := range s.proj.GetInstances() {
		if inst == prev.inst {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	if prev.inst.ShownArchived() {
		s.expandSectionKind(SectionArchived)
		for j, item := range s.visibleItems {
			if item.Kind == SectionArchived && !item.IsHeader && item.ItemIndex == idx {
				s.selectedIdx = j
				return
			}
		}
		return
	}
	if !prev.isTab || tree.Expandable(prev.inst) {
		return
	}
	for j, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && !item.IsTab && item.ItemIndex == idx {
			s.selectedIdx = j
			return
		}
	}
}
