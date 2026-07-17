package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/keys"
)

// openConfigEditorForTest puts the model in stateConfigEditor with the manifest
// loaded and the cursor on a settable key, ready to edit.
func openConfigEditorForTest(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	h.configPane.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	h.configPane.SetFocus(true)
	h.state = stateConfigEditor
	return h
}

// TestConfigEditorQuitKeyIsTypeableIntoAFocusedValueField is the APP-LAYER guard
// for the #1961 bug class, and it exists because the pane-layer test is not
// enough.
//
// ui.TestConfigPaneQuitKeyIsTypeableWhileEditing proves the PANE consumes "q".
// That test passes even if this layer never gives the pane the key — and this
// layer is exactly where #1961 lives: handleStateHooks and handleStateTasks
// root-route the configured quit key before their pane sees it, so "q" cannot be
// typed into those forms at all. A pane-layer test would not have caught that,
// which is precisely why it must be asserted here, through the real handler.
//
// Config values contain "q" constantly — a sqlite path, any URL with a query
// string, a binary under /home/quentin. The letter must reach the field.
func TestConfigEditorQuitKeyIsTypeableIntoAFocusedValueField(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	h := openConfigEditorForTest(t)

	// Open the value field, clear it, then type a value containing the quit key.
	_, _ = h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, h.configPane.IsEditing(), "precondition: enter opens the value field")
	h.configPane.SetEditValueForTest("")

	for _, r := range "sqlite" {
		_, cmd := h.handleStateConfigEditor(runeKey(r))
		assert.False(t, reachesQuit(cmd), "typing %q into a config value must not quit af", string(r))
	}

	assert.True(t, h.configPane.IsEditing(), "typing must not leave the value field")
	assert.Equal(t, "sqlite", h.configPane.EditValueForTest(),
		"every character typed into a config value must reach the field")
}

// TestConfigEditorReboundQuitKeyIsAlsoTypeable pins that the gate follows the
// CONFIGURED key, not the literal "q": a user who rebinds quit to "Q" must still
// be able to type "Q" into a value (a capitalized path, an env var name).
func TestConfigEditorReboundQuitKeyIsAlsoTypeable(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{"quit": {"Q"}}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	h := openConfigEditorForTest(t)
	_, _ = h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
	h.configPane.SetEditValueForTest("")

	_, cmd := h.handleStateConfigEditor(runeKey('Q'))

	assert.False(t, reachesQuit(cmd), "a rebound quit key must be typeable into a value field too")
	assert.Equal(t, "Q", h.configPane.EditValueForTest())
}

// TestConfigEditorQuitKeyStillQuitsFromNormalMode is the other half of the
// contract: the gate is on the FIELD, not on the overlay. With no value field
// open, the config editor is an ordinary list and the quit key quits, exactly as
// it does everywhere else.
func TestConfigEditorQuitKeyStillQuitsFromNormalMode(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	h := openConfigEditorForTest(t)
	require.False(t, h.configPane.IsEditing(), "precondition: no value field open")

	_, cmd := h.handleStateConfigEditor(runeKey('q'))

	assert.True(t, reachesQuit(cmd), "with no field open, the quit key must quit")
}

// TestConfigEditorCtrlCQuitsEvenFromAFocusedValueField is #1727's actual point,
// held for this overlay: however a text field is being used, it must never
// swallow the hard exit. This is what makes gating the configured quit key safe
// — a user who cannot type their way out still has ctrl+c (and esc).
func TestConfigEditorCtrlCQuitsEvenFromAFocusedValueField(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	h := openConfigEditorForTest(t)
	_, _ = h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, h.configPane.IsEditing(), "precondition: the value field is open")

	_, cmd := h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyCtrlC})

	assert.True(t, reachesQuit(cmd), "ctrl+c must quit even from a focused value field (#1727)")
}
