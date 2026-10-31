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
	// committed is the list as last loaded or saved, so Discard can put back what
	// is actually stored instead of leaving the pane dirty forever.
	committed []string
	// readOnly, when non-empty, is why this pane cannot be edited (W9.7: a remote
	// target's hooks live in the checkout on the daemon host). The edit keys do
	// nothing, so an unsaveable edit cannot be made in the first place.
	readOnly string
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
	h.committed = append([]string(nil), commands...)
	h.dirty = false
	if len(h.commands) == 0 {
		h.selectedIdx = 0
	} else if h.selectedIdx >= len(h.commands) {
		h.selectedIdx = len(h.commands) - 1
	}
}

// Discard drops every unsaved edit and restores the last loaded or saved list.
func (h *HooksPane) Discard() {
	h.commands = append([]string(nil), h.committed...)
	h.dirty = false
	h.editing = false
	h.adding = false
	h.editBuffer = ""
	if h.selectedIdx >= len(h.commands) {
		h.selectedIdx = max(0, len(h.commands)-1)
	}
}

// SetReadOnly makes the pane refuse edits and explain why; "" makes it editable.
func (h *HooksPane) SetReadOnly(reason string) {
	h.readOnly = reason
	if reason != "" {
		h.Discard()
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
	case "n", "enter", "D":
		if h.readOnly != "" {
			return true // consumed, and nothing to edit: the pane says why
		}
	}
	switch msg.String() {
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
	default:
		h.editBuffer, _ = EditLine(h.editBuffer, msg)
	}
	return true
}

func (h *HooksPane) String() string {
	t := CurrentTheme()
	tStyle := DialogTitleStyle()
	selectedStyle := lipgloss.NewStyle().Bold(true).Background(t.SurfaceRaised).Foreground(t.Ink)
	normalStyle := lipgloss.NewStyle().Foreground(t.Ink)
	hintStyle := DialogHintStyle()
	editStyle := selectedStyle
	descStyle := lipgloss.NewStyle().Foreground(t.Ink)

	var b strings.Builder
	b.WriteString(tStyle.Render("Post-worktree hooks"))
	b.WriteString("\n")
	b.WriteString(descStyle.Render("Commands run async in new worktrees"))
	b.WriteString("\n\n")

	if h.readOnly != "" {
		b.WriteString(hintStyle.Render("  " + h.readOnly))
		b.WriteString("\n\n")
	}
	if len(h.commands) == 0 && !h.adding && h.readOnly == "" {
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
			b.WriteString(SelectionMarker("▸ ") + editStyle.Render(h.editBuffer) + InputCaret())
		} else if isSelected && h.hasFocus && !h.adding {
			b.WriteString(SelectionMarker("▸ ") + selectedStyle.Render(cmd))
		} else {
			b.WriteString(normalStyle.Render("  " + cmd))
		}
		b.WriteString("\n")
	}

	if h.adding {
		b.WriteString(SelectionMarker("▸ ") + editStyle.Render(h.editBuffer) + InputCaret())
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if h.hasFocus {
		if h.editing || h.adding {
			b.WriteString(hintStyle.Render("enter save · esc cancel"))
		} else if h.readOnly != "" {
			b.WriteString(hintStyle.Render("esc back"))
		} else {
			hint := "n add · enter edit · D delete · esc back"
			if h.width > 0 && lipgloss.Width(hint) > h.width {
				hint = "n add · enter · esc back"
			}
			b.WriteString(ActionHint(hint))
		}
	} else {
		b.WriteString(hintStyle.Render("enter edit hooks"))
	}

	return fitBlockToSize(b.String(), h.width, h.height, 1)
}
