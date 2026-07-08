package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
)

func TestTinyClipConfirmationOverlaysFit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{name: "kill", message: "[!] Kill session 'beta'?"},
		{
			name: "archive",
			message: "[!] Archive session 'beta'?\n\n" +
				"Its tmux is torn down and its worktree is moved out to the archive directory " +
				"(branch + uncommitted changes preserved). Restore later with a.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			resizeHome(h, 40, 10)

			_ = h.confirmAction(tc.message, nil)

			view := h.View()
			requireViewSized(t, view, 40, 10)
			plain := xansi.Strip(view)
			assert.Contains(t, plain, "confirm")
			assert.Contains(t, plain, "cancel")
		})
	}
}

func TestTinyClipSearchOverlayFitsAtSmallSizes(t *testing.T) {
	for _, size := range [][2]int{{40, 10}, {60, 15}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			h := newTestHome(t)
			h.store.AddInstance(newSnapshotTestInstance(t, "alpha"))
			h.store.AddInstance(newSnapshotTestInstance(t, "beta"))
			h.store.AddInstance(newSnapshotTestInstance(t, "gamma"))
			resizeHome(h, size[0], size[1])

			_, _ = h.showSearchOverlay()
			for _, r := range "gamma" {
				_ = h.searchOverlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			}

			view := h.View()
			requireViewSized(t, view, size[0], size[1])
			plain := xansi.Strip(view)
			assert.Contains(t, plain, "Search Sessions")
			assert.Contains(t, plain, "gamma")
			assert.Contains(t, plain, "esc close")
		})
	}
}

func TestTinyClipHooksOverlayFits(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 40, 10)

	_, _ = h.showHooksOverlay()
	_, _ = h.handleStateHooks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	for _, r := range "echo hook && false" {
		_, _ = h.handleStateHooks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	view := h.View()
	requireViewSized(t, view, 40, 10)
	plain := xansi.Strip(view)
	assert.Contains(t, plain, "Post-Worktree Hooks")
	assert.Contains(t, plain, "enter save")
	assert.Contains(t, plain, "esc cancel")
}

func TestTinyClipTasksOverlayFits(t *testing.T) {
	h := newTestHome(t)
	h.store.SetTasks([]task.Task{})
	resizeHome(h, 40, 10)

	_, _ = h.showTasksOverlay()
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	for _, r := range "tiny-task" {
		_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	view := h.View()
	requireViewSized(t, view, 40, 10)
	plain := xansi.Strip(view)
	assert.Contains(t, plain, "tiny-task")
	assert.Contains(t, plain, "esc cancel")
	assert.Contains(t, plain, "↑ more")
}
