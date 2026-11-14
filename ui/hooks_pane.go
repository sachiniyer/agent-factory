package ui

import (
	"slices"
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
	hasFocus    bool
	// committed is the list as last loaded or saved. IsDirty diffs commands
	// against it, Discard puts it back — so the pane is dirty exactly when it
	// holds something a save would persist (#6790).
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
	if len(h.commands) == 0 {
		h.selectedIdx = 0
	} else if h.selectedIdx >= len(h.commands) {
		h.selectedIdx = len(h.commands) - 1
	}
}

// Discard drops every unsaved edit and restores the last loaded or saved list.
func (h *HooksPane) Discard() {
	h.commands = append([]string(nil), h.committed...)
	h.editing = false
	h.adding = false
	h.editBuffer = ""
	if h.selectedIdx >= len(h.commands) {
		h.selectedIdx = max(0, len(h.commands)-1)
	}
}

// MarkSaved advances the committed baseline to the list a save just wrote, so
// a successful save leaves the pane clean and a later close writes nothing
// (#6790). Without it the pane stayed dirty for the session: every Esc or
// quit re-saved the in-memory list over hooks added on disk in the meantime,
// and the re-read on open was skipped as a dirty-pane guard.
func (h *HooksPane) MarkSaved() {
	h.committed = append([]string(nil), h.commands...)
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

// IsDirty reports whether the in-memory list differs from the committed
// baseline — the copy last loaded (SetCommands) or saved (MarkSaved). A no-op
// confirm, an edit restored to the stored value, and a successful save all
// leave the pane clean, so a clean Esc or quit writes nothing (#6790).
func (h *HooksPane) IsDirty() bool {
	return !slices.Equal(h.commands, h.committed)
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
	out, _ := h.render()
	return out
}

// render draws the pane and reports, for each line it draws, the index of the
// command shown there or -1 — one pass, so a click zone and the row it names
// cannot disagree (#7175).
func (h *HooksPane) render() (string, []int) {
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

	// commandStartLine is the count of header chrome (title, description and
	// any readOnly message) above the command rows — the pinned header height
	// for the window below. Capturing it here, before the rows are written, lets
	// the window name absolute source line indices the same way tagLine does.
	commandStartLine := strings.Count(b.String(), "\n")
	var sourceRows []int
	for i, cmd := range h.commands {
		sourceRows = tagLine(sourceRows, strings.Count(b.String(), "\n"), i)
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

	// Window the list around whatever the user is acting on, the way
	// fitTaskListPlan does for the task list, instead of the selection-blind
	// top-anchored clip that used to drop the selected row — and the add/edit
	// input row drawn after the command rows — below the fold. The anchor is
	// the selected command's line, or the add field's line when adding (the
	// edit field reuses the selected row's line); keeping it visible keeps both
	// the cursor and any in-progress text on screen. The footer is the blank
	// line plus the hint, pinned so the keys never scroll out of view.
	lines := strings.Split(b.String(), "\n")
	const footer = 2
	header := commandStartLine
	anchorStart := commandStartLine
	if h.adding {
		anchorStart += len(h.commands)
	} else if len(h.commands) > 0 {
		anchorStart += h.selectedIdx
	}
	plan := fitWindowedPlan(len(lines), h.height, header, footer, anchorStart, anchorStart+1)
	return renderFitPlan(lines, plan, h.width), planRows(plan, sourceRows)
}
