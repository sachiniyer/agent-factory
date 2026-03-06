package overlay

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HooksOverlay is an interactive list overlay for managing post-worktree hook commands.
type HooksOverlay struct {
	commands    []string
	selectedIdx int
	editing     bool
	adding      bool
	editBuffer  string
	width       int
	closed      bool
	dirty       bool
}

// NewHooksOverlay creates a new hooks overlay with the given commands.
func NewHooksOverlay(commands []string) *HooksOverlay {
	cmds := make([]string, len(commands))
	copy(cmds, commands)
	return &HooksOverlay{
		commands: cmds,
		width:    60,
	}
}

func (h *HooksOverlay) SetWidth(width int) { h.width = width }
func (h *HooksOverlay) IsClosed() bool      { return h.closed }
func (h *HooksOverlay) IsDirty() bool       { return h.dirty }
func (h *HooksOverlay) GetCommands() []string { return h.commands }

// HandleKeyPress processes a key press. Returns true if the overlay should close.
func (h *HooksOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	if h.editing || h.adding {
		return h.handleEditMode(msg)
	}
	return h.handleNormalMode(msg)
}

func (h *HooksOverlay) handleNormalMode(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "ctrl+c":
		h.closed = true
		return true
	case "up", "k":
		if h.selectedIdx > 0 {
			h.selectedIdx--
		}
	case "down", "j":
		if h.selectedIdx < len(h.commands)-1 {
			h.selectedIdx++
		}
	case "n":
		h.adding = true
		h.editBuffer = ""
	case "enter":
		if len(h.commands) > 0 {
			h.editing = true
			h.editBuffer = h.commands[h.selectedIdx]
		}
	case "D":
		if len(h.commands) > 0 {
			h.commands = append(h.commands[:h.selectedIdx], h.commands[h.selectedIdx+1:]...)
			h.dirty = true
			if h.selectedIdx >= len(h.commands) && h.selectedIdx > 0 {
				h.selectedIdx--
			}
		}
	}
	return false
}

func (h *HooksOverlay) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEnter:
		if h.editBuffer != "" {
			if h.adding {
				h.commands = append(h.commands, h.editBuffer)
				h.selectedIdx = len(h.commands) - 1
			} else {
				h.commands[h.selectedIdx] = h.editBuffer
			}
			h.dirty = true
		}
		h.adding = false
		h.editing = false
		h.editBuffer = ""
	case tea.KeyEsc:
		h.adding = false
		h.editing = false
		h.editBuffer = ""
	case tea.KeyBackspace:
		if len(h.editBuffer) > 0 {
			runes := []rune(h.editBuffer)
			h.editBuffer = string(runes[:len(runes)-1])
		}
	case tea.KeySpace:
		h.editBuffer += " "
	case tea.KeyRunes:
		h.editBuffer += string(msg.Runes)
	}
	return false
}

// Render renders the hooks overlay.
func (h *HooksOverlay) Render(opts ...WhitespaceOption) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	editStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF79C6"))
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A")).Italic(true)

	content := titleStyle.Render("Post-Worktree Hooks") + "\n"
	content += descStyle.Render("Commands run async in new worktrees") + "\n\n"

	if len(h.commands) == 0 && !h.adding {
		content += normalStyle.Render("  No hooks configured. Press n to add one.") + "\n"
	}

	for i, cmd := range h.commands {
		isSelected := i == h.selectedIdx
		if h.editing && isSelected {
			content += editStyle.Render("▸ "+h.editBuffer+"_") + "\n"
		} else if isSelected {
			content += selectedStyle.Render("▸ "+cmd) + "\n"
		} else {
			content += normalStyle.Render("  "+cmd) + "\n"
		}
	}

	if h.adding {
		content += editStyle.Render("▸ "+h.editBuffer+"_") + "\n"
	}

	content += "\n"
	if h.editing || h.adding {
		content += hintStyle.Render("enter save • esc cancel")
	} else {
		content += hintStyle.Render("n add • enter edit • D delete • esc close")
	}

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(1, 2).
		Width(h.width)

	return style.Render(content)
}
