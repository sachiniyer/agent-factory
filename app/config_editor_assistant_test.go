package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/configagent"
)

// TestConfigEditorAssistantButtonSpawnsTheAgent is the #2453 wiring, end to end
// through the real overlay handler: pressing C in the config editor closes the
// overlay AND opens the config assistant. It drives handleStateConfigEditor (not
// the pane directly), because the app layer is where the pane's assistant request
// is turned into a spawn — a pane-layer test proves the request is recorded but
// not that the host acts on it.
//
// The spawn is swapped for a seam that records it, so the test asserts the button
// reaches the real spawn path without starting a daemon or a tmux session.
func TestConfigEditorAssistantButtonSpawnsTheAgent(t *testing.T) {
	h := openConfigEditorForTest(t)

	spawned := 0
	t.Cleanup(SetConfigAgentSpawnerForTest(func(mode configagent.Mode, _ string) (string, string, error) {
		spawned++
		assert.Equal(t, configagent.ModeChange, mode,
			"the config-pane button opens the CHANGE flow, same as the C hotkey")
		return "af_af-config-1", "", nil
	}))

	model, cmd := h.handleStateConfigEditor(runeKey('C'))
	hm := model.(*home)

	// The overlay must have closed — the assistant takes over the full screen, so
	// the editor cannot still own it.
	assert.Equal(t, stateDefault, hm.state, "pressing C must close the config editor overlay")
	assert.False(t, hm.configPane.HasFocus(), "the config pane must drop focus when the assistant opens")

	// A command must have been returned, and running it must reach the spawn seam.
	require.NotNil(t, cmd, "pressing C must return a command that spawns the assistant")
	msg := cmd()
	_, isSpawn := msg.(configAgentSpawnedMsg)
	require.True(t, isSpawn, "the command must drive the config-agent spawn, got %T", msg)
	assert.Equal(t, 1, spawned, "the assistant button must spawn exactly one config agent")

	// The request is one-shot: the pane must not still be holding it, or a later
	// close would spawn a second agent.
	assert.False(t, hm.configPane.TakeAssistantRequest(),
		"the assistant request must be consumed by the spawn, not left pending")
}

// TestConfigEditorCTypedIntoAValueFieldDoesNotSpawn is the collision guard at the
// app layer: while a value is being edited, C is ordinary text (a path under
// /home/carol, a branch prefix) and must reach the field, never the assistant.
func TestConfigEditorCTypedIntoAValueFieldDoesNotSpawn(t *testing.T) {
	h := openConfigEditorForTest(t)

	spawned := 0
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, string, error) {
		spawned++
		return "", "", nil
	}))

	// Open a value field, then type C.
	_, _ = h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, h.configPane.IsEditing(), "precondition: a value field is open")
	_, cmd := h.handleStateConfigEditor(runeKey('C'))

	assert.Equal(t, stateConfigEditor, h.state, "typing C into a value must not close the editor")
	assert.Nil(t, cmd, "typing C into a value must not spawn the assistant")
	assert.Equal(t, 0, spawned, "no config agent may be spawned by C typed as text")
}
