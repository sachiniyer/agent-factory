package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namingFormWithConflict opens the naming form holding a title an existing
// session already has, and submits it: #4123's repro. The title is long enough
// that the conflict notice clips at 80 columns.
func namingFormWithConflict(t *testing.T) (*home, *session.Instance, string) {
	t.Helper()
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	const title = "todo-core-with-a-longer-title"

	existing, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	h.store.AddInstance(existing)
	naming, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	h.state = stateNew
	h.pendingProgram = "claude"
	h.namingInstance = naming

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state, "precondition: the form refuses the title and stays open")
	want := fmt.Sprintf("a session titled %q conflicts with existing session %q", title, title)
	require.Contains(t, h.errBox.FullError(), want, "precondition: the conflict notice is up")
	return h, naming, want
}

// TestNamingFormNoticeStaysWhileFormOpen: the reason the form refused its
// input stays on the bar for as long as the form holds that input, and expires
// normally once the form closes (#4123).
func TestNamingFormNoticeStaysWhileFormOpen(t *testing.T) {
	h, _, want := namingFormWithConflict(t)

	_, cmd := h.Update(hideErrMsg{noticeID: h.transientNoticeID})
	assert.Contains(t, h.errBox.FullError(), want, "the 3s timer must not expire a notice the open form raised")
	assert.NotNil(t, cmd, "the timer re-arms, so the notice expires once the form closes")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	require.Nil(t, h.namingInstance, "precondition: esc closed the form")
	_, _ = h.Update(hideErrMsg{noticeID: h.transientNoticeID})
	assert.Empty(t, h.errBox.FullError(), "with the form gone the notice expires as usual")
}

// TestNamingFormCtrlEOpensNoticeDetailsAndReturns: inside the form E is a
// title character, so ctrl+e opens the full notice, the clipped bar says so,
// and closing the details goes back to the form with its input intact (#4123).
func TestNamingFormCtrlEOpensNoticeDetailsAndReturns(t *testing.T) {
	h, naming, want := namingFormWithConflict(t)
	_ = h.View()
	assert.Contains(t, h.errBox.String(), "ctrl+e details",
		"the clipped notice advertises the key that works inside the form")
	assert.NotContains(t, h.errBox.String(), "E details")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlE})
	require.Equal(t, stateHelp, h.state)
	require.NotNil(t, h.textOverlay)
	assert.Contains(t, h.textOverlay.Render(), "conflicts with existing session")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, stateNew, h.state, "closing the details returns to the form")
	assert.Same(t, naming, h.namingInstance, "the form's input survives reading why it was refused")
	assert.Equal(t, "todo-core-with-a-longer-title", naming.Title)
	assert.Contains(t, h.errBox.FullError(), want)
}
