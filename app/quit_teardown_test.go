package app

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func commandEmitsQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); ok {
		return true
	}
	seq := reflect.ValueOf(msg)
	if seq.Kind() != reflect.Slice {
		return false
	}
	for i := 0; i < seq.Len(); i++ {
		next, ok := seq.Index(i).Interface().(tea.Cmd)
		if ok && commandEmitsQuit(next) {
			return true
		}
	}
	return false
}

func requireTeaSequence(t *testing.T, cmd tea.Cmd) []tea.Cmd {
	t.Helper()
	require.NotNil(t, cmd)
	msg := cmd()
	seq := reflect.ValueOf(msg)
	require.Equal(t, reflect.Slice, seq.Kind(), "expected tea.Sequence, got %T", msg)
	cmds := make([]tea.Cmd, 0, seq.Len())
	for i := 0; i < seq.Len(); i++ {
		next, ok := seq.Index(i).Interface().(tea.Cmd)
		require.True(t, ok, "sequence entry %d must be tea.Cmd", i)
		cmds = append(cmds, next)
	}
	return cmds
}

func TestHandleQuitCleansTerminalBeforeQuit(t *testing.T) {
	h := newTestHome(t)

	_, cmd := h.handleQuit()

	require.True(t, h.quitting, "quit must suppress Bubble Tea's final render")
	assert.Empty(t, h.View(), "final graceful render must not repaint stale TUI chrome")

	cmds := requireTeaSequence(t, cmd)
	// Clear runs only while still on the alt screen: a second clear after
	// ExitAltScreen would erase the host screen — inside tmux, the shell pane
	// (#4785).
	require.Len(t, cmds, 3, "quit must clear alt, leave alt-screen, then quit")
	assert.Equal(t, tea.ClearScreen(), cmds[0]())
	assert.Equal(t, tea.ExitAltScreen(), cmds[1]())
	_, isQuit := cmds[2]().(tea.QuitMsg)
	assert.True(t, isQuit, "teardown sequence must still finish with tea.Quit")
}
