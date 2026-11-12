package ui

// OverlayRows is a pane hosted in a modal overlay whose list rows take clicks
// (#7175): the task manager, the hooks editor and the config editor. The app
// registers one zone per row from RowLines against the overlay's origin, the
// way every sibling picker registers its rows, and a press resolves back to a
// row index through SelectedRow/SelectRow.
type OverlayRows interface {
	// RowLines reports, for each line of the pane's String(), the index of the
	// row drawn there, or -1 for chrome (titles, notes, hints, overflow cues).
	// It is nil while a form or text field owns the pane: its only interactive
	// surface is the field, which a row click could not drive.
	RowLines() []int
	// SelectedRow is the index of the row under the cursor, or -1.
	SelectedRow() int
	// SelectRow moves the cursor onto row idx, like j/k would, and reports
	// whether it could: a row that left the list since the frame that drew it,
	// or one the cursor may not land on, leaves the cursor where it was.
	SelectRow(idx int) bool
}

var (
	_ OverlayRows = (*HooksPane)(nil)
	_ OverlayRows = (*TaskPane)(nil)
	_ OverlayRows = (*ConfigPane)(nil)
)

// tagLine records that source line `line` draws row idx, growing rows with -1
// (chrome) up to it.
func tagLine(rows []int, line, idx int) []int {
	for len(rows) <= line {
		rows = append(rows, -1)
	}
	rows[line] = idx
	return rows
}

// RowLines implements OverlayRows. Nil while a command is being typed.
func (h *HooksPane) RowLines() []int {
	if h.editing || h.adding {
		return nil
	}
	_, rows := h.render()
	return rows
}

// SelectedRow implements OverlayRows.
func (h *HooksPane) SelectedRow() int {
	if len(h.commands) == 0 {
		return -1
	}
	return h.selectedIdx
}

// SelectRow implements OverlayRows.
func (h *HooksPane) SelectRow(idx int) bool {
	if h.editing || h.adding || idx < 0 || idx >= len(h.commands) {
		return false
	}
	h.selectedIdx = idx
	return true
}

// RowLines implements OverlayRows: the task list's rows, each spanning its
// header and the detail lines under it. Nil in the create/edit form, and while
// the tasks are unavailable — recovery mode renders no selection, and every
// selection key is inert there too.
func (s *TaskPane) RowLines() []int {
	if s.editing || s.creating || s.unavailable != "" {
		return nil
	}
	_, rows := s.renderListModeRows()
	return rows
}

// SelectedRow implements OverlayRows.
func (s *TaskPane) SelectedRow() int {
	if !s.selectedTaskInRange() {
		return -1
	}
	return s.selectedIdx
}

// SelectRow implements OverlayRows.
func (s *TaskPane) SelectRow(idx int) bool {
	if s.editing || s.creating || s.unavailable != "" || idx < 0 || idx >= len(s.tasks) {
		return false
	}
	s.selectedIdx = idx
	return true
}

// RowLines implements OverlayRows: each selectable config row across the lines
// it draws inside the scrolled window. Nil while a value, an account name or a
// host form is being typed, and in the explain reader.
func (c *ConfigPane) RowLines() []int {
	if c.IsEditing() || c.explaining {
		return nil
	}
	_, rows := c.renderRows()
	return rows
}

// SelectedRow implements OverlayRows.
func (c *ConfigPane) SelectedRow() int {
	if c.selectedIdx < 0 || c.selectedIdx >= len(c.rows) {
		return -1
	}
	return c.selectedIdx
}

// SelectRow implements OverlayRows. Headings and other non-selectable rows
// refuse, exactly as j/k skip them.
func (c *ConfigPane) SelectRow(idx int) bool {
	if c.IsEditing() || c.explaining || idx < 0 || idx >= len(c.rows) || !c.rows[idx].isSelectable() {
		return false
	}
	c.selectedIdx = idx
	return true
}
