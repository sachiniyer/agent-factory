package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestHelpReflectsKeymapRebinds is the regression guard for the #1141
// play-test blocker 2: the help overlay rendered hardcoded key literals, so a
// [keys] rebind showed everywhere EXCEPT help. It must now build from the same
// generated binding table as dispatch and the bottom menu.
func TestHelpReflectsKeymapRebinds(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"quit": {"Q"},
		"new":  {"g"},
		"up":   {"u", "ctrl+p"},
	}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	content := helpTypeGeneral{}.toContent()

	// Rebound keys must appear...
	for _, want := range []string{"Q", "g", "u/ctrl+p"} {
		if !strings.Contains(content, want) {
			t.Errorf("help must show rebound key %q; got:\n%s", want, content)
		}
	}
	// ...and the replaced defaults must be gone from their action lines.
	if strings.Contains(content, "q         - Quit") || strings.Contains(content, "↑/k, ↓/j") {
		t.Errorf("help still shows default keys after a rebind; got:\n%s", content)
	}
}

// TestGeneralHelpNavigationMatchesBindings guards against regressing #764, where
// the help screen documented "↑/j, ↓/k" while the canonical bindings in
// keys/keys.go map k=up and j=down (standard vim convention).
func TestGeneralHelpNavigationMatchesBindings(t *testing.T) {
	content := helpTypeGeneral{}.toContent()

	if !strings.Contains(content, "↑/k, ↓/j") {
		t.Errorf("help content missing canonical navigation label \"↑/k, ↓/j\"; got:\n%s", content)
	}
	if strings.Contains(content, "↑/j, ↓/k") {
		t.Errorf("help content contains reversed navigation label \"↑/j, ↓/k\" (see #764); got:\n%s", content)
	}
}

// TestInstanceStartHelpRemoteOmitsUnsupportedTabKeys guards against regressing
// #988: remote instances block `t` (new tab) and `w` (close tab) — those
// handlers reject IsRemote() with an error — so the instance-start help must
// only advertise the tab keys that actually work (cycle / 1-9 jump). Local
// instances keep the full hint.
func TestInstanceStartHelpRemoteOmitsUnsupportedTabKeys(t *testing.T) {
	remote := newStartedInstance(t, "remote")
	remote.SetBackend(&session.HookBackend{})
	require.True(t, remote.IsRemote(), "sanity: instance should report as remote")

	remoteContent := helpStart(remote).toContent()
	if strings.Contains(remoteContent, "t new tab") || strings.Contains(remoteContent, "w close") {
		t.Errorf("remote instance-start help must not advertise unsupported t/w tab keys; got:\n%s", remoteContent)
	}
	if !strings.Contains(remoteContent, "1-9 jump") {
		t.Errorf("remote instance-start help should still advertise the supported 1-9 jump; got:\n%s", remoteContent)
	}

	local := newStartedInstance(t, "local")
	require.False(t, local.IsRemote(), "sanity: instance should report as local")

	localContent := helpStart(local).toContent()
	if !strings.Contains(localContent, "t new tab") || !strings.Contains(localContent, "w close") {
		t.Errorf("local instance-start help should advertise the full t/w/1-9 tab hint; got:\n%s", localContent)
	}
}

func TestInstanceStartHelpMentionsFullScreenDetach(t *testing.T) {
	local := newStartedInstance(t, "local")
	content := helpStart(local).toContent()

	if !strings.Contains(content, "ctrl-w") || !strings.Contains(content, "Detach from a full-screen session") {
		t.Errorf("instance-start help must name the full-screen detach key; got:\n%s", content)
	}
}

func TestInstanceStartHelpShowsFirstRunActionsAndGenericAgentCopy(t *testing.T) {
	local := newStartedInstance(t, "local")
	content := helpStart(local).toContent()

	for _, want := range []string{
		"Agent process running in background tmux session",
		"enter continue",
		"esc close",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("instance-start help missing %q; got:\n%s", want, content)
		}
	}
	if strings.Contains(content, "claude running in background tmux session") {
		t.Errorf("instance-start help must not hard-code the selected program as the running process; got:\n%s", content)
	}
}

func TestInstanceAttachHelpShowsProceedCancelAndDetach(t *testing.T) {
	content := helpTypeInstanceAttach{}.toContent()

	for _, want := range []string{
		"enter attach full-screen",
		"esc cancel",
		"Detach later with",
		"ctrl-w",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("attach help missing %q; got:\n%s", want, content)
		}
	}
}

func TestGeneralHelpOverlayFitsAndMarksScrollAt80x24(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)

	_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)
	fg := h.textOverlay.Render()
	require.LessOrEqual(t, strings.Count(fg, "\n")+1, 24, "help overlay foreground must fit inside the terminal")
	out := h.View()
	require.Contains(t, out, "Agent Factory v", "initial viewport must include the title")
	require.Contains(t, out, "↓ more", "initial viewport must show overflow below")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyCtrlD})
	require.Equal(t, stateHelp, h.state, "configured scroll key must keep the help overlay open")
	fg = h.textOverlay.Render()
	require.LessOrEqual(t, strings.Count(fg, "\n")+1, 24, "scrolled help overlay foreground must fit inside the terminal")
	out = h.View()
	require.Contains(t, out, "↑ more", "scrolled viewport must show overflow above")
}

func TestGeneralHelpOverlayShiftArrowsScrollAt80x24(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)

	_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyShiftDown})
	require.Equal(t, stateHelp, h.state, "Shift+Down must scroll, not dismiss the help overlay")
	require.False(t, h.textOverlay.Dismissed, "scrolling must not mark the overlay dismissed")
	require.Contains(t, h.View(), "↑ more", "Shift+Down should move the viewport down")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyShiftUp})
	require.Equal(t, stateHelp, h.state, "Shift+Up must scroll, not dismiss the help overlay")
	require.False(t, h.textOverlay.Dismissed, "scrolling up must not mark the overlay dismissed")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.Equal(t, stateHelp, h.state, "non-dismiss keys must not close the scrollable general help overlay")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "Esc remains the explicit help dismiss key")
}
