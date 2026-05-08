package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/stretchr/testify/assert"
)

func TestHandleStateSelectProgramSubstringCollision(t *testing.T) {
	h := newTestHome(t)
	h.program = "/tmp/claude-wrapper --flag"
	h.selectionOverlay = overlay.NewSelectionOverlay("Select Program", tmux.SupportedPrograms)
	h.selectionOverlay.SetSelectedIndex(0) // claude
	h.state = stateSelectProgram

	_, _ = h.handleStateSelectProgram(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, "claude", h.pendingProgram)
}

func TestHandleStateSelectProgramPreservesExactProgramFlags(t *testing.T) {
	h := newTestHome(t)
	h.program = "/usr/local/bin/claude --dangerously-skip-permissions"
	h.selectionOverlay = overlay.NewSelectionOverlay("Select Program", tmux.SupportedPrograms)
	h.selectionOverlay.SetSelectedIndex(0) // claude
	h.state = stateSelectProgram

	_, _ = h.handleStateSelectProgram(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, h.program, h.pendingProgram)
}
