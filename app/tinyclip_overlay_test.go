package app

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
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
			assert.Contains(t, plain, "Search sessions")
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
	// The form still WINDOWS and still flags the fields it is hiding — the
	// #1431 property this test pins. The marker is `↓ more` rather than the
	// `↑ more` this asserted before #1821: at 40x10 the overlay is now
	// full-screen, so the form has the terminal's whole height instead of 60%
	// of it, and the top of the form (New task / Essentials / the focused Name
	// field) now fits without scrolling off the top. Strictly more of the form
	// is visible; the hidden remainder moved below the window, so the marker
	// moved with it. `↑`/`↓` marker semantics are pinned directly, and
	// independently of the overlay's geometry, in ui/task_pane_test.go.
	assert.Contains(t, plain, "↓ more")
}

// railFragments are strings only the rail ever renders — its title chip, its
// section headers, and an instance row's title. None of them appear in the task
// overlay's own chrome, so finding one in a full-screen frame means the rail
// bled through.
var railFragments = []string{"Agent Factory", "Instances", "Automations", "Projects", "rail-inst"}

// TestTasksOverlayNarrowIsFullScreenWithoutRailBleed is the #1821 regression.
//
// The task editor's width floor (52) ignores the terminal, so on a narrow one
// the box came out wider than the workspace and the CENTERED modal landed on
// top of the rail — at 60x20, a 54-column box centered at x=3 over a 22-column
// rail painted over 19 of its 22 columns and left `▸`, `Au`, `Pr` stubs down
// the side, with the frame drawn straight through them.
//
// Narrow geometry now switches to the deliberate full-screen presentation
// (#1821's own suggestion): the modal takes the whole terminal, so the rail is
// covered cleanly and completely instead of being shredded into fragments.
//
// 60x20 is the reported size; 40x12 is the floor — minimal mode (< MinimalWidth
// 60) but still above the HardMin 40x10 fallback banner, so a real overlay is
// composed at both.
func TestTasksOverlayNarrowIsFullScreenWithoutRailBleed(t *testing.T) {
	for _, size := range [][2]int{{60, 20}, {40, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			w, hgt := size[0], size[1]
			h := newTestHome(t)
			h.store.SetTasks([]task.Task{})
			addTreeInstance(t, h, "rail-inst")
			resizeHome(h, w, hgt)

			_, _ = h.showTasksOverlay()

			view := h.View()
			// No clipping: the composed frame is still exactly the terminal.
			requireViewSized(t, view, w, hgt)
			plain := xansi.Strip(view)

			// No overlap: nothing of the rail survives to bleed into the editor.
			for _, frag := range railFragments {
				assert.NotContainsf(t, plain, frag,
					"rail fragment %q bled into the full-screen task overlay at %dx%d", frag, w, hgt)
			}

			// The overlay owns every row edge-to-edge — this is what rules out
			// the stub sliver, which requireViewSized alone cannot see.
			for i, line := range strings.Split(plain, "\n") {
				require.Truef(t, strings.HasPrefix(line, "╭") || strings.HasPrefix(line, "│") ||
					strings.HasPrefix(line, "╰"),
					"row %d must start at the overlay's own left border, got %q", i, line)
				require.Truef(t, strings.HasSuffix(line, "╮") || strings.HasSuffix(line, "│") ||
					strings.HasSuffix(line, "╯"),
					"row %d must end at the overlay's own right border, got %q", i, line)
			}

			// …and it is still legible, not merely bounded.
			assert.Contains(t, plain, "Tasks")
			assert.Contains(t, plain, "r run now")
		})
	}
}

// TestTasksOverlayWideKeepsRailAndCenteredModal pins the OTHER side of the
// #1821 switch: full-screen is for narrow terminals only. At a roomy size the
// modal must still be the centered box it has always been, with the rail beside
// it — so the fix cannot quietly turn every task overlay full-screen.
func TestTasksOverlayWideKeepsRailAndCenteredModal(t *testing.T) {
	h := newTestHome(t)
	h.store.SetTasks([]task.Task{})
	addTreeInstance(t, h, "rail-inst")
	resizeHome(h, 100, 30)

	_, _ = h.showTasksOverlay()

	assert.False(t, h.modalGoesFullScreen(hooksOverlayStyle, h.preferredOverlayWidth(52)),
		"a 100x30 terminal has room for the modal beside the rail; it must not go full-screen")

	view := h.View()
	requireViewSized(t, view, 100, 30)
	plain := xansi.Strip(view)
	assert.Contains(t, plain, "Agent Factory", "the rail must still render beside a centered modal")
	assert.Contains(t, plain, "rail-inst", "the rail's instance row must still render")
	assert.Contains(t, plain, "Tasks")
}
