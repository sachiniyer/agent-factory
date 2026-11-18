package ui

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
// the right re-pin only when the cursor's own row is gone (#717). It is wrong
// whenever the session is still here and only its row moved: the index then
// names some other row, pushSelection hands that row the selection, and a row
// verb read through syncFromStore (GetSelectedInstance/RowVerbTarget) lands on
// a row the user never selected — or on nothing, when the index lands on a
// header or the root separator (#7551). Three move shapes exist:
//
//   - The session was archived, so its row re-homed from Sessions into the
//     Archived folder. The cursor follows it there and the folder opens, so an
//     archive lands on the archived row whether or not an earlier visit had
//     left the folder open.
//   - The cursor's own row moved within the live section — rows folded or
//     vanished above it. The cursor re-pins to the row it was on: its tab row
//     when that row still renders, else the session's instance row.
//   - An in-flight op (archive, kill, respawn…) made it unexpandable while the
//     cursor was on one of its tab rows, folding them away — the tab-row search
//     then misses and the same instance-row fallback applies, which is the
//     landing h/← gives a tab row.
//
// A pending SelectInstance assertion runs after this and still wins.
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
	if prev.isTab {
		for j, item := range s.visibleItems {
			if item.Kind == SectionInstances && !item.IsHeader && item.IsTab &&
				item.ItemIndex == idx && item.TabIndex == prev.tab {
				s.selectedIdx = j
				return
			}
		}
	}
	for j, item := range s.visibleItems {
		if item.Kind == SectionInstances && !item.IsHeader && !item.IsTab && item.ItemIndex == idx {
			s.selectedIdx = j
			return
		}
	}
}
