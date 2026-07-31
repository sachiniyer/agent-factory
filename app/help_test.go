package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestHelpReflectsKeymapRebinds is the regression guard for the #1141
// play-test blocker 2: the help overlay rendered hardcoded key literals, so a
// [keys] rebind showed everywhere EXCEPT help. It must now build from the same
// generated binding table as dispatch and the bottom menu.
func TestHelpReflectsKeymapRebinds(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"quit": {"Q"},
		"new":  {"g"},
		"up":   {"u", "ctrl+g"},
	}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	content := helpTypeGeneral{}.toContent()

	// Rebound keys must appear...
	for _, want := range []string{"Q", "g", "u/ctrl+g"} {
		if !strings.Contains(content, want) {
			t.Errorf("help must show rebound key %q; got:\n%s", want, content)
		}
	}
	// ...and the replaced defaults must be gone from their action lines.
	if strings.Contains(content, "q         - Quit") || strings.Contains(content, "↑/k, ↓/j") {
		t.Errorf("help still shows default keys after a rebind; got:\n%s", content)
	}
}

func TestGeneralHelpReboundDismissKeyWinsOverPaging(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"help": {"space"},
	}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	h := newTestHome(t)
	resizeHome(h, 80, 24)
	_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeySpace})

	require.Equal(t, stateDefault, h.state,
		"a configured help key must dismiss help even when it is also a paging key")
}

func TestGeneralHelpReboundLineKeyWinsOverPagingAlias(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"up": {"pgdown"},
	}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	actual := newTestHome(t)
	resizeHome(actual, 80, 24)
	_, _ = actual.showHelpScreen(helpTypeGeneral{}, nil)
	_, _ = actual.handleHelpState(tea.KeyMsg{Type: tea.KeyCtrlD})

	want := newTestHome(t)
	resizeHome(want, 80, 24)
	_, _ = want.showHelpScreen(helpTypeGeneral{}, nil)
	_, _ = want.handleHelpState(tea.KeyMsg{Type: tea.KeyCtrlD})
	want.textOverlay.ScrollUp()

	_, _ = actual.handleHelpState(tea.KeyMsg{Type: tea.KeyPgDown})

	require.Equal(t, xansi.Strip(want.View()), xansi.Strip(actual.View()),
		"a configured line key must win over a hardcoded paging alias")
}

func TestGeneralHelpHidesPagingAliasShadowedByRebind(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"up": {"pgdown"},
	}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	var pageLine string
	for _, line := range strings.Split(xansi.Strip(helpTypeGeneral{}.toContent()), "\n") {
		if strings.HasPrefix(line, "Page:") {
			pageLine = line
			break
		}
	}

	require.NotEmpty(t, pageLine)
	require.NotContains(t, pageLine, "pgdn",
		"the page controls must not advertise an alias shadowed by a rebind")
	require.Contains(t, xansi.Strip(helpTypeGeneral{}.toContent()), "pgdown, ↓/j",
		"the effective line binding remains advertised on its actual action")
}

func TestGeneralHelpHidesJumpAliasShadowedByRebind(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"help": {"home"},
	}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	var lineControls string
	content := xansi.Strip(helpTypeGeneral{}.toContent())
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "Line:") {
			lineControls = line
			break
		}
	}

	require.NotEmpty(t, lineControls)
	require.NotContains(t, lineControls, "home",
		"the line controls must not advertise a jump alias shadowed by a rebind")
	require.Contains(t, content, "Close: esc · home toggles help",
		"the effective help binding remains advertised on its actual action")
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

// TestInstanceStartHelpRemoteOmitsUnsupportedTabKeys removed — remote (hook)
// backends now have full local parity including TabManagement, so the
// instance-start help advertises the same t/w/1-9 tab keys for remote as for
// local. The #988 remote tab-key restriction no longer exists. // #1592 Phase 4 PR7

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

func TestFirstRunHelpTitlesUseSentenceCase(t *testing.T) {
	local := newStartedInstance(t, "local")
	tests := []struct {
		name    string
		content string
		want    string
		old     string
	}{
		{"instance created", helpStart(local).toContent(), "Session created", "Session Created"},
		{"attach", helpTypeInstanceAttach{}.toContent(), "Attaching to session", "Attaching to Session"},
		{"interactive pane", helpTypeInteractive{}.toContent(), "Interactive pane", "Interactive Pane"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, tc.content, tc.want)
			require.NotContains(t, tc.content, tc.old)
		})
	}
}

func TestInstanceAttachHelpShowsProceedCancelAndDetach(t *testing.T) {
	content := helpTypeInstanceAttach{}.toContent()

	for _, want := range []string{
		"The attached program owns input and scrolling",
		"preview scrolling works only in navigation mode",
		"enter attach full-screen · esc cancel",
		"Detach later with",
		"ctrl-w",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("attach help missing %q; got:\n%s", want, content)
		}
	}
}

// TestAttachedScrollHelpIsAgentSpecific pins #2195's central honesty rule:
// Claude and Codex do not share attached scroll controls, and an unknown child
// gets no guessed hint. Codex PageUp works only after Ctrl-T opens transcript;
// its wheel did not scroll in the real 0.144.6 TUI verification.
func TestAttachedScrollHelpIsAgentSpecific(t *testing.T) {
	tests := []struct {
		name   string
		agent  string
		want   string
		reject []string
	}{
		{
			name:   "Claude",
			agent:  tmux.ProgramClaude,
			want:   "Claude owns attached scrolling: " + claudeAttachedScrollControls,
			reject: []string{"ctrl+t opens transcript"},
		},
		{
			name:   "Codex",
			agent:  tmux.ProgramCodex,
			want:   "Codex owns attached scrolling: " + codexAttachedScrollControls,
			reject: []string{"mouse wheel", "ctrl+home/end"},
		},
		{
			name:   "unknown child",
			agent:  "aider",
			reject: []string{"owns attached scrolling", "mouse wheel", "opens transcript"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := helpTypeInstanceAttach{agent: tc.agent}.toContent()
			if tc.want != "" {
				require.Contains(t, content, tc.want)
			}
			for _, reject := range tc.reject {
				require.NotContains(t, content, reject)
			}
		})
	}
}

type remoteHelpBackend struct{ *session.FakeBackend }

func (b *remoteHelpBackend) Capabilities() session.Capabilities {
	capabilities := b.FakeBackend.Capabilities()
	capabilities.Workspace = session.WorkspaceRemote
	return capabilities
}

func (b *remoteHelpBackend) Type() string { return "remote" }

func TestHelpAttachOnlyNamesControlsForAgentTab(t *testing.T) {
	instance := newStartedInstance(t, "claude-session")
	instance.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedName("af_help_claude", "claude"))

	agentTab, ok := helpAttach(instance, 0).(helpTypeInstanceAttach)
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agentTab.agent)

	shellTab, ok := helpAttach(instance, 1).(helpTypeInstanceAttach)
	require.True(t, ok)
	require.Empty(t, shellTab.agent, "an arbitrary shell/process child must not get an agent-specific scroll guess")

	instance.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedName("af_help_override", "bash"))
	overriddenAgentTab, ok := helpAttach(instance, 0).(helpTypeInstanceAttach)
	require.True(t, ok)
	require.Empty(t, overriddenAgentTab.agent,
		"a configured Claude slot overridden to bash must follow the resolved child, not guess from Instance.Program")

	remote := newStartedInstance(t, "remote-claude")
	remote.SetBackend(&remoteHelpBackend{FakeBackend: session.NewFakeBackend()})
	remoteAgentTab, ok := helpAttach(remote, 0).(helpTypeInstanceAttach)
	require.True(t, ok)
	require.Empty(t, remoteAgentTab.agent,
		"a remote tab with no local resolved command must not guess controls from Instance.Program")
}

func TestGeneralHelpSeparatesPreviewAndAttachedScrolling(t *testing.T) {
	content := helpTypeGeneral{}.toContent()

	require.Contains(t, content, "Scroll the current tab preview (navigation mode only)")
	require.Contains(t, content, "Claude")
	require.Contains(t, content, claudeAttachedScrollControls)
	require.Contains(t, content, "Codex")
	require.Contains(t, content, codexAttachedScrollControls)
	require.NotContains(t, content, "Scroll in the current tab")
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

// TestGeneralHelpStaysCenteredWhenScrolledAt80x24 is the #1998 regression: at
// 80x24 the general help wraps to lines one cell past the box's text width, so
// the overlay box grew a row per such visible line, overflowed the terminal,
// and PlaceOverlay fell back to dumping the raw frame — a ~50-column fragment at
// column 0 with its top border clipped and the surrounding TUI blank. Drive the
// real Ctrl-D scroll path deep into (and past) the content through home.View()
// and assert every composited frame stays the full 80x24 window (which the
// overflow dump violates: it is only as wide as the box and taller than the
// terminal). The overlay's own centering geometry is locked, over a blank
// background, by TestTextOverlayStaysFramedWhenLinesSoftWrapPastWidth.
func TestGeneralHelpStaysCenteredWhenScrolledAt80x24(t *testing.T) {
	const termWidth, termHeight = 80, 24

	h := newTestHome(t)
	resizeHome(h, termWidth, termHeight)
	_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)

	for step := 0; step < 30; step++ {
		out := h.View()
		// The whole-window contract: a taller-than-terminal box makes PlaceOverlay
		// return the raw foreground, which is only box-wide and overflows the
		// height — failing both the per-line width and the line-count checks here.
		requireViewSized(t, out, termWidth, termHeight)
		// A scroll marker proves the framed, scrollable overlay is actually
		// composited on top — not that PlaceOverlay silently dropped it.
		require.Containsf(t, out, "more", "step %d: the scrollable help overlay must stay on screen", step)

		_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyCtrlD})
		require.Equalf(t, stateHelp, h.state, "step %d: Ctrl-D must scroll, not dismiss the help overlay", step)
	}
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

func TestGeneralHelpOverlayPagesAndJumpsAt80x24(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)

	initial := xansi.Strip(h.View())
	require.Contains(t, initial, "pgup/pgdn",
		"the initial viewport must advertise its page-sized controls")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyPgDown})
	paged := xansi.Strip(h.View())
	require.NotEqual(t, initial, paged, "PageDown must move the help viewport")
	require.NotContains(t, paged, "Agent Factory v",
		"one page step must move by a viewport, not one row")
	require.Contains(t, paged, "↑ more")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEnd})
	require.Contains(t, xansi.Strip(h.View()), "Other:",
		"End must jump to the bottom of help")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyHome})
	require.Contains(t, xansi.Strip(h.View()), "Agent Factory v",
		"Home must jump back to the top of help")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyCtrlD})
	require.NotContains(t, xansi.Strip(h.View()), "Agent Factory v",
		"the advertised Ctrl-D binding must page rather than move one row")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyPgUp})
	require.Contains(t, xansi.Strip(h.View()), "Agent Factory v",
		"PageUp must move a viewport back toward the top")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	require.Contains(t, xansi.Strip(h.View()), "↑ more",
		"the advertised down binding must provide line-sized navigation")
	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	require.Contains(t, xansi.Strip(h.View()), "Agent Factory v",
		"the advertised up binding must return one line toward the top")
}

func TestGeneralHelpWrappedDescriptionsStayOutOfKeyColumnAt80x24(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)
	h.textOverlay.SetHeight(100)

	lines := strings.Split(xansi.Strip(h.textOverlay.Render()), "\n")
	var retryLine, continuationLine string
	for _, line := range lines {
		if strings.Contains(line, "Retry") {
			retryLine = line
		}
		if strings.Contains(line, "usage limit") {
			continuationLine = line
		}
	}
	require.NotEmpty(t, retryLine, "the narrow help viewport must contain the retry binding")
	require.NotEmpty(t, continuationLine, "the retry description must wrap at 80 columns")

	retryContent := strings.TrimSuffix(strings.TrimPrefix(retryLine, "│"), "│")
	continuationContent := strings.TrimSuffix(strings.TrimPrefix(continuationLine, "│"), "│")
	descriptionColumn := strings.Index(retryContent, "Retry")
	require.GreaterOrEqual(t, descriptionColumn, 0)
	continuationColumn := len(continuationContent) - len(strings.TrimLeft(continuationContent, " "))
	require.Equal(t, descriptionColumn, continuationColumn,
		"wrapped descriptions must use a hanging indent at the description column")
}
