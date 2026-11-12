package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// #7175: the task manager, hooks editor and config editor overlays answered the
// keyboard and swallowed every click — they registered no zones, and
// handleModalClick had no case for their states. Each now registers one
// overlay:select zone per list row, and a press selects the row; a press on the
// row already selected is Enter on it. Click coordinates come from the registry
// the last View() built (clickZone), so a layout change moves these tests with
// it.

// assertNoOverlayRows renders a frame and asserts no overlay row zone exists —
// the state while a form or text field owns the overlay.
func assertNoOverlayRows(t *testing.T, h *home, why string) {
	t.Helper()
	_ = h.View()
	for i := 0; i < 8; i++ {
		_, ok := h.zones.Find(zones.OverlaySelectRow(i))
		assert.False(t, ok, "%s: row %d must register no click zone", why, i)
	}
}

// TestTasksOverlayRowClickSelectsThenEdits: a click on a task row in the open
// task manager selects it; a click on the selected task opens its edit form,
// as Enter does. The form itself registers no row zones.
func TestTasksOverlayRowClickSelectsThenEdits(t *testing.T) {
	h, _, _ := mouseTestHome(t)
	newFakeClock(h)
	sp := h.automations.TaskPane()

	// The manager opens straight into the selected task's form (#1249); Esc
	// returns to the list, whose rows are what a click selects.
	h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	require.True(t, sp.IsEditing(), "precondition: the manager opens on the edit form")
	assertNoOverlayRows(t, h, "the task edit form")
	h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateTasks, h.state, "Esc in the form returns to the list, not out of the manager")
	require.False(t, sp.IsEditing())
	require.Equal(t, 0, sp.SelectedRow(), "precondition: the first task is selected")

	clickZone(t, h, zones.OverlaySelectRow(1))
	assert.Equal(t, 1, sp.SelectedRow(), "a click on a task row selects it")
	assert.False(t, sp.IsEditing(), "a click on an unselected row only selects it")
	assert.Equal(t, stateTasks, h.state)

	clickZone(t, h, zones.OverlaySelectRow(1))
	assert.True(t, sp.IsEditing(), "a click on the selected task opens its edit form, like Enter")
	assert.Equal(t, stateTasks, h.state)
}

// TestHooksOverlayRowClickSelectsThenEdits: the same gesture in the hooks
// editor — select, then edit the selected hook in place.
func TestHooksOverlayRowClickSelectsThenEdits(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 140, 40)
	newFakeClock(h)
	h.hooksPane.SetReadOnly("")
	h.hooksPane.SetCommands([]string{"make deps", "npm ci"})
	h.hooksPane.SetFocus(true)
	h.layoutPaneOverlays()
	h.state = stateHooks
	require.Equal(t, 0, h.hooksPane.SelectedRow())

	clickZone(t, h, zones.OverlaySelectRow(1))
	assert.Equal(t, 1, h.hooksPane.SelectedRow(), "a click on a hook row selects it")
	assert.False(t, h.hooksPane.IsEditing(), "a click on an unselected row only selects it")

	clickZone(t, h, zones.OverlaySelectRow(1))
	assert.True(t, h.hooksPane.IsEditing(), "a click on the selected hook opens its edit field, like Enter")
	assert.Equal(t, stateHooks, h.state)
	assertNoOverlayRows(t, h, "an open hook edit field")
}

// TestConfigOverlayRowClickSelectsThenEdits: the same gesture in the config
// editor — select a key, then open its value field.
func TestConfigOverlayRowClickSelectsThenEdits(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 140, 40)
	newFakeClock(h)
	h.configPane.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml", nil)
	h.configPane.SetFocus(true)
	h.layoutPaneOverlays()
	h.state = stateConfigEditor

	_ = h.View()
	selected := h.configPane.SelectedRow()
	require.GreaterOrEqual(t, selected, 0, "precondition: the cursor rests on a row")
	_, ok := h.zones.Find(zones.OverlaySelectRow(selected))
	require.True(t, ok, "the selected key's row must register a click zone; frame has %v", h.zones.IDs())
	// The next key down the list — a manifest entry, whose Enter is its value field.
	target := -1
	for i := selected + 1; i <= selected+4; i++ {
		if _, ok := h.zones.Find(zones.OverlaySelectRow(i)); ok {
			target = i
			break
		}
	}
	require.GreaterOrEqual(t, target, 0, "precondition: a second key row is on screen; frame has %v", h.zones.IDs())

	clickZone(t, h, zones.OverlaySelectRow(target))
	assert.Equal(t, target, h.configPane.SelectedRow(), "a click on a key row selects it")
	assert.False(t, h.configPane.IsEditing(), "a click on an unselected row only selects it")

	clickZone(t, h, zones.OverlaySelectRow(target))
	assert.True(t, h.configPane.IsEditing(), "a click on the selected key opens its value field, like Enter")
	assert.Equal(t, stateConfigEditor, h.state)
	assertNoOverlayRows(t, h, "an open value field")
}
