package ui

import (
	"claude-squad/task"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// flatItem represents a single renderable row in the kanban view.
type flatItem struct {
	isHeader bool
	column   string
	taskIdx  int // index into the column's task slice
}

// KanbanPane renders an interactive kanban board inline in the right pane.
type KanbanPane struct {
	board       *task.Board
	flat        []flatItem
	selectedIdx int
	editing     bool
	editBuffer  string
	adding      bool
	carrying    bool
	carriedTask *task.Task
	carriedFrom string
	width       int
	height      int
	scrollOff   int
	dirty       bool
	hasFocus    bool
}

func NewKanbanPane() *KanbanPane { return &KanbanPane{} }

func (k *KanbanPane) SetSize(width, height int) { k.width = width; k.height = height }
func (k *KanbanPane) GetBoard() *task.Board      { return k.board }
func (k *KanbanPane) IsDirty() bool              { return k.dirty }
func (k *KanbanPane) HasFocus() bool             { return k.hasFocus }

func (k *KanbanPane) SetBoard(board *task.Board) {
	k.board = board
	k.dirty = false
	k.rebuildFlat()
}

func (k *KanbanPane) SetFocus(focus bool) {
	k.hasFocus = focus
	if !focus {
		k.editing = false
		k.adding = false
		k.editBuffer = ""
		if k.carrying {
			k.cancelCarry()
		}
	}
}

func (k *KanbanPane) HandleKeyPress(msg tea.KeyMsg) bool {
	if !k.hasFocus {
		return false
	}
	if k.editing || k.adding {
		return k.handleEditMode(msg)
	}

	// Navigation keys shared between normal and carry modes
	switch msg.String() {
	case "up", "k":
		k.moveUp()
		return true
	case "down", "j":
		k.moveDown()
		return true
	case "left", "h":
		k.jumpPrevColumn()
		return true
	case "right", "l":
		k.jumpNextColumn()
		return true
	case "M":
		if k.carrying {
			k.dropCarry()
		} else {
			k.startCarry()
		}
		return true
	case "esc":
		if k.carrying {
			k.cancelCarry()
		} else {
			k.hasFocus = false
		}
		return true
	}

	// Keys only available when NOT carrying
	if !k.carrying {
		switch msg.String() {
		case "n":
			k.adding = true
			k.editBuffer = ""
			return true
		case "enter":
			if t := k.getTaskAtFlat(k.selectedIdx); t != nil {
				k.editing = true
				k.editBuffer = t.Title
			}
			return true
		case "D":
			k.deleteSelected()
			return true
		}
	}

	return true // consume all keys when focused
}

func (k *KanbanPane) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEnter:
		if k.editBuffer != "" {
			if k.adding {
				col := k.currentColumn()
				k.board.AddTask(k.editBuffer, col)
				k.dirty = true
				k.rebuildFlat()
				k.selectLastTaskInColumn(col)
			} else if t := k.getTaskAtFlat(k.selectedIdx); t != nil {
				t.Title = k.editBuffer
				k.dirty = true
			}
		}
		k.adding = false
		k.editing = false
		k.editBuffer = ""
	case tea.KeyEsc:
		k.adding = false
		k.editing = false
		k.editBuffer = ""
	case tea.KeyBackspace:
		if len(k.editBuffer) > 0 {
			runes := []rune(k.editBuffer)
			k.editBuffer = string(runes[:len(runes)-1])
		}
	case tea.KeySpace:
		k.editBuffer += " "
	case tea.KeyRunes:
		k.editBuffer += string(msg.Runes)
	}
	return true
}

// --- Flat list management ---

func (k *KanbanPane) rebuildFlat() {
	if k.board == nil {
		k.flat = nil
		return
	}
	var items []flatItem
	for _, col := range k.board.Columns {
		items = append(items, flatItem{isHeader: true, column: col})
		for i := range k.board.GetTasksByStatus(col) {
			items = append(items, flatItem{column: col, taskIdx: i})
		}
	}
	k.flat = items
	if k.selectedIdx >= len(k.flat) && len(k.flat) > 0 {
		k.selectedIdx = len(k.flat) - 1
	}
	if k.selectedIdx < 0 {
		k.selectedIdx = 0
	}
}

func (k *KanbanPane) getTaskAtFlat(idx int) *task.Task {
	if idx < 0 || idx >= len(k.flat) || k.flat[idx].isHeader {
		return nil
	}
	fi := k.flat[idx]
	tasks := k.board.GetTasksByStatus(fi.column)
	if fi.taskIdx < 0 || fi.taskIdx >= len(tasks) {
		return nil
	}
	id := tasks[fi.taskIdx].ID
	for i := range k.board.Tasks {
		if k.board.Tasks[i].ID == id {
			return &k.board.Tasks[i]
		}
	}
	return nil
}

func (k *KanbanPane) currentColumn() string {
	if k.selectedIdx >= 0 && k.selectedIdx < len(k.flat) {
		return k.flat[k.selectedIdx].column
	}
	if k.board != nil && len(k.board.Columns) > 0 {
		return k.board.Columns[0]
	}
	return "backlog"
}

// --- Navigation ---

func (k *KanbanPane) moveUp() {
	if k.selectedIdx > 0 {
		k.selectedIdx--
	}
	k.ensureVisible()
}

func (k *KanbanPane) moveDown() {
	if k.selectedIdx < len(k.flat)-1 {
		k.selectedIdx++
	}
	k.ensureVisible()
}

func (k *KanbanPane) jumpNextColumn() {
	for i := k.selectedIdx + 1; i < len(k.flat); i++ {
		if k.flat[i].isHeader {
			k.selectedIdx = i
			k.ensureVisible()
			return
		}
	}
}

func (k *KanbanPane) jumpPrevColumn() {
	for i := k.selectedIdx - 1; i >= 0; i-- {
		if k.flat[i].isHeader {
			k.selectedIdx = i
			k.ensureVisible()
			return
		}
	}
}

func (k *KanbanPane) selectLastTaskInColumn(col string) {
	for i := len(k.flat) - 1; i >= 0; i-- {
		if !k.flat[i].isHeader && k.flat[i].column == col {
			k.selectedIdx = i
			k.ensureVisible()
			return
		}
	}
}

func (k *KanbanPane) ensureVisible() {
	visible := k.height - 4
	if visible < 1 {
		visible = 1
	}
	if k.selectedIdx < k.scrollOff {
		k.scrollOff = k.selectedIdx
	}
	if k.selectedIdx >= k.scrollOff+visible {
		k.scrollOff = k.selectedIdx - visible + 1
	}
}

// --- Mutations ---

func (k *KanbanPane) deleteSelected() {
	t := k.getTaskAtFlat(k.selectedIdx)
	if t == nil {
		return
	}
	k.board.DeleteTask(t.ID)
	k.dirty = true
	k.rebuildFlat()
}

func (k *KanbanPane) startCarry() {
	t := k.getTaskAtFlat(k.selectedIdx)
	if t == nil {
		return
	}
	carried := *t
	k.carriedTask = &carried
	k.carriedFrom = k.flat[k.selectedIdx].column
	k.carrying = true
	k.board.DeleteTask(carried.ID)
	k.dirty = true
	k.rebuildFlat()
}

func (k *KanbanPane) dropCarry() {
	if k.carriedTask == nil {
		return
	}
	k.carriedTask.Status = k.currentColumn()
	k.carriedTask.UpdatedAt = time.Now()

	idx := k.findInsertIndex()
	if idx >= len(k.board.Tasks) {
		k.board.Tasks = append(k.board.Tasks, *k.carriedTask)
	} else {
		k.board.Tasks = append(k.board.Tasks[:idx+1], k.board.Tasks[idx:]...)
		k.board.Tasks[idx] = *k.carriedTask
	}

	k.carrying = false
	k.carriedTask = nil
	k.carriedFrom = ""
	k.dirty = true
	k.rebuildFlat()
}

func (k *KanbanPane) cancelCarry() {
	if k.carriedTask == nil {
		return
	}
	k.carriedTask.Status = k.carriedFrom
	k.board.Tasks = append(k.board.Tasks, *k.carriedTask)
	k.carrying = false
	k.carriedTask = nil
	k.carriedFrom = ""
	k.rebuildFlat()
}

func (k *KanbanPane) findInsertIndex() int {
	if k.selectedIdx < 0 || k.selectedIdx >= len(k.flat) {
		return len(k.board.Tasks)
	}
	fi := k.flat[k.selectedIdx]
	col := fi.column
	if fi.isHeader {
		for i, t := range k.board.Tasks {
			if t.Status == col {
				return i
			}
		}
		return len(k.board.Tasks)
	}
	tasks := k.board.GetTasksByStatus(col)
	if fi.taskIdx < len(tasks) {
		targetID := tasks[fi.taskIdx].ID
		for i, t := range k.board.Tasks {
			if t.ID == targetID {
				return i + 1
			}
		}
	}
	return len(k.board.Tasks)
}

// --- Rendering ---

var (
	kanbanHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	kanbanColumnStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#36CFC9"))
	kanbanSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	kanbanNormalStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	kanbanDimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#555555"))
	kanbanHintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	kanbanEditStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF79C6"))
)

func columnDisplayName(col string) string {
	switch col {
	case "backlog":
		return "Backlog"
	case "in_progress":
		return "In Progress"
	case "review":
		return "Review"
	case "done":
		return "Done"
	default:
		return col
	}
}

func (k *KanbanPane) String() string {
	var b strings.Builder
	b.WriteString(kanbanHeaderStyle.Render("Board"))
	b.WriteString("\n")

	if k.board == nil || len(k.flat) == 0 {
		b.WriteString("\n")
		b.WriteString(kanbanNormalStyle.Render("  No tasks yet. Press Enter to focus, then n to add."))
		b.WriteString("\n")
		k.writeHints(&b)
		return b.String()
	}

	contentWidth := k.width - 4
	if contentWidth < 20 {
		contentWidth = 40
	}
	counts := k.board.CountByStatus()

	for i, fi := range k.flat {
		if i < k.scrollOff {
			continue
		}
		if i > k.scrollOff+k.height-4 {
			break
		}

		isSelected := i == k.selectedIdx && k.hasFocus

		if fi.isHeader {
			label := fmt.Sprintf(" %s (%d) ", columnDisplayName(fi.column), counts[fi.column])
			lineLen := contentWidth - len(label) - 2
			if lineLen < 2 {
				lineLen = 2
			}
			line := strings.Repeat("─", 2) + label + strings.Repeat("─", lineLen)
			if isSelected {
				b.WriteString(kanbanSelectedStyle.Render(line))
			} else {
				b.WriteString(kanbanColumnStyle.Render(line))
			}
			b.WriteString("\n")
			continue
		}

		t := k.getTaskAtFlat(i)
		if t == nil {
			continue
		}

		if k.editing && isSelected {
			b.WriteString(kanbanEditStyle.Render(" > " + k.editBuffer + "_"))
		} else if isSelected && k.carrying {
			b.WriteString(kanbanSelectedStyle.Render(" > (drop here)"))
		} else if isSelected {
			b.WriteString(kanbanSelectedStyle.Render(" > " + t.Title))
		} else if fi.column == "done" {
			b.WriteString(kanbanDimStyle.Render("   " + t.Title))
		} else {
			b.WriteString(kanbanNormalStyle.Render("   " + t.Title))
		}
		b.WriteString("\n")
	}

	if k.adding {
		col := k.currentColumn()
		b.WriteString(kanbanEditStyle.Render(fmt.Sprintf(" > [%s] %s_", columnDisplayName(col), k.editBuffer)))
		b.WriteString("\n")
	}

	if k.carrying && k.carriedTask != nil {
		b.WriteString("\n")
		b.WriteString(kanbanEditStyle.Render(fmt.Sprintf(" ╭ carrying: %s ╮", k.carriedTask.Title)))
		b.WriteString("\n")
	}

	k.writeHints(&b)
	return b.String()
}

func (k *KanbanPane) writeHints(b *strings.Builder) {
	b.WriteString("\n")
	if !k.hasFocus {
		b.WriteString(kanbanHintStyle.Render("enter to focus and edit board"))
	} else if k.editing || k.adding {
		b.WriteString(kanbanHintStyle.Render("enter save | esc cancel"))
	} else if k.carrying {
		b.WriteString(kanbanHintStyle.Render("M drop here | j/k position | h/l column | esc cancel"))
	} else {
		b.WriteString(kanbanHintStyle.Render("j/k navigate | h/l jump section | n add | M grab/drop | D del"))
	}
}
