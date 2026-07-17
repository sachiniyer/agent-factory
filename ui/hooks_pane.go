package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HooksPane renders an interactive list of post-worktree hook commands
// inline in the right pane.
type HooksPane struct {
	commands    []string
	selectedIdx int
	editing     bool
	editBuffer  string
	adding      bool
	width       int
	height      int
	dirty       bool
	hasFocus    bool
}

func NewHooksPane() *HooksPane {
	return &HooksPane{}
}

func (h *HooksPane) SetSize(width, height int) {
	h.width = width
	h.height = height
}

func (h *HooksPane) SetCommands(commands []string) {
	h.commands = commands
	h.dirty = false
	if len(h.commands) == 0 {
		h.selectedIdx = 0
	} else if h.selectedIdx >= len(h.commands) {
		h.selectedIdx = len(h.commands) - 1
	}
}

func (h *HooksPane) GetCommands() []string {
	return h.commands
}

func (h *HooksPane) IsDirty() bool {
	return h.dirty
}

// IsEditing reports whether a text field is focused and taking runes (the add
// form or an inline edit).
//
// The app asks before root-routing the configured quit key (#1961): while a hook
// command is being typed, "q" is a character, not an exit. ctrl+c still quits
// unconditionally, which is what #1727 is actually about.
func (h *HooksPane) IsEditing() bool { return h.editing || h.adding }

func (h *HooksPane) HasFocus() bool {
	return h.hasFocus
}

func (h *HooksPane) SetFocus(focus bool) {
	h.hasFocus = focus
	if !focus {
		h.editing = false
		h.adding = false
		h.editBuffer = ""
	}
}

// HandleKeyPress processes a key press. Returns true if the key was consumed.
func (h *HooksPane) HandleKeyPress(msg tea.KeyMsg) bool {
	if !h.hasFocus {
		return false
	}
	if h.editing || h.adding {
		return h.handleEditMode(msg)
	}
	return h.handleNormalMode(msg)
}

func (h *HooksPane) handleNormalMode(msg tea.KeyMsg) bool {
	if msg.String() == "ctrl+c" || configuredQuitKey(msg) {
		return false
	}
	switch msg.String() {
	case "esc":
		h.hasFocus = false
		return true
	case "up", "k":
		if h.selectedIdx > 0 {
			h.selectedIdx--
		}
		return true
	case "down", "j":
		if h.selectedIdx < len(h.commands)-1 {
			h.selectedIdx++
		}
		return true
	case "n":
		h.adding = true
		h.editBuffer = ""
		return true
	case "enter":
		if len(h.commands) > 0 {
			h.editing = true
			h.editBuffer = h.commands[h.selectedIdx]
		}
		return true
	case "D":
		if len(h.commands) > 0 {
			h.commands = append(h.commands[:h.selectedIdx], h.commands[h.selectedIdx+1:]...)
			h.dirty = true
			if h.selectedIdx >= len(h.commands) && h.selectedIdx > 0 {
				h.selectedIdx--
			}
		}
		return true
	}
	return true // consume all keys when focused
}

func (h *HooksPane) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEnter:
		// Reject blank/whitespace-only commands so they neither persist to the
		// on-disk config nor overwrite a non-empty command on edit (#870),
		// matching how watch/cron/remote-hook inputs are validated.
		if strings.TrimSpace(h.editBuffer) != "" {
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
	case tea.KeyEsc, tea.KeyCtrlC:
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
	return true
}

func (h *HooksPane) String() string {
	t := CurrentTheme()
	tStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Warning)
	normalStyle := lipgloss.NewStyle().Foreground(t.ForegroundMuted)
	hintStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	editStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Purple)
	descStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim).Italic(true)

	var b strings.Builder
	b.WriteString(tStyle.Render("Post-Worktree Hooks"))
	b.WriteString("\n")
	b.WriteString(descStyle.Render("Commands run async in new worktrees"))
	b.WriteString("\n\n")

	if len(h.commands) == 0 && !h.adding {
		msg := "  No hooks configured. Press Enter to focus, then n to add."
		if h.width > 0 && lipgloss.Width(msg) > h.width {
			msg = "  No hooks configured. Press n to add."
		}
		b.WriteString(normalStyle.Render(msg))
		b.WriteString("\n")
	}

	for i, cmd := range h.commands {
		isSelected := i == h.selectedIdx
		if h.editing && isSelected {
			b.WriteString(editStyle.Render("▸ "+h.editBuffer) + InputCaret())
		} else if isSelected && h.hasFocus {
			b.WriteString(selectedStyle.Render("▸ " + cmd))
		} else {
			b.WriteString(normalStyle.Render("  " + cmd))
		}
		b.WriteString("\n")
	}

	if h.adding {
		b.WriteString(editStyle.Render("▸ "+h.editBuffer) + InputCaret())
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if h.hasFocus {
		if h.editing || h.adding {
			b.WriteString(hintStyle.Render("enter save • esc cancel"))
		} else {
			hint := "n add • enter edit • D delete • esc back"
			if h.width > 0 && lipgloss.Width(hint) > h.width {
				hint = "n add • enter • esc back"
			}
			b.WriteString(hintStyle.Render(hint))
		}
	} else {
		b.WriteString(hintStyle.Render("enter to focus and edit hooks"))
	}

	return fitBlockToSize(b.String(), h.width, h.height, 1)
}
