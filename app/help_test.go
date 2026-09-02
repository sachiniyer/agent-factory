package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

// TestHelpKeyColumnCapStopsOneWideKeyStarvingEveryRow is #3629. The key column
// was sized to the widest key across ALL sections with no cap, and the help has
// exactly one wide key — "tab / shift+tab / ctrl+r", 24 cells — against ~30 rows
// of 1-6. At 80x24 (overlay 48 wide, contentWidth 44) it took 26 of the 44
// content columns and left 16 for every description.
func TestHelpKeyColumnCapStopsOneWideKeyStarvingEveryRow(t *testing.T) {
	// contentWidth at 80x24: layoutTextOverlay sets the overlay to 80*0.6 = 48,
	// textWidth() subtracts 2*textOverlayHorizontalPadding.
	narrow := 44
	// The real distribution: the 24-cell outlier, then the widest ordinary key
	// ("ctrl+u/ctrl+d", 13), then the rest.
	widths := []int{24, 13, 9, 8, 6, 6, 5, 1, 1, 1}

	got := helpKeyColumnWidth(widths, narrow)
	require.LessOrEqual(t, got, int(float64(narrow)*helpKeyColumnFraction),
		"the key column must never exceed its share of the content width")
	require.Equal(t, 15, got,
		"it snaps to the widest key that FITS under the 17-cell cap (13+2), not to the cap")

	desc := narrow - got - 2
	require.Greater(t, desc, got,
		"the descriptions must get more columns than the keys — 16 of 44 was the bug")
	require.Equal(t, 27, desc)

	// Wide terminals are untouched: at 200x50 contentWidth is 116, the cap is 46,
	// and the natural 26 is far below it.
	wide := 116
	require.Equal(t, 26, helpKeyColumnWidth(widths, wide),
		"the cap must be inert where the layout already reads correctly")
}

// The cap must claim exactly one victim — the outlier that caused the problem.
// Snapping to the widest key that FITS under the cap, rather than to the cap
// itself, is what keeps an ordinary key from wrapping as collateral.
func TestHelpKeyColumnWrapsOnlyTheOutlier(t *testing.T) {
	narrow := 44
	sections := []helpSection{{title: "Managing:", rows: []helpRow{
		{"tab / shift+tab / ctrl+r", "While naming a new session: pick its agent / initial prompt / backend"},
		{"ctrl+u/ctrl+d", "Scroll the current tab preview (navigation mode only)"},
		{"n", "Create a new session"},
	}}}

	lines := strings.Split(xansi.Strip(renderHelpSections("header", sections, narrow)), "\n")
	var wideRow, ordinaryRow string
	for _, line := range lines {
		if strings.Contains(line, "ctrl+u/ctrl+d") {
			ordinaryRow = line
		}
		if strings.Contains(line, "shift+tab") {
			wideRow = line
		}
	}
	require.NotEmpty(t, ordinaryRow)
	require.NotEmpty(t, wideRow)

	require.Contains(t, ordinaryRow, "Scroll the current tab",
		"the widest ORDINARY key must still fit its column on one line, beside its description")
	require.NotContains(t, wideRow, "ctrl+r",
		"the over-cap key must wrap inside its own column rather than widen everyone's")
	// The 2 cells past the widest key are a GUTTER: a wrapped key must not spend
	// them, or the row reads "tab / shift+tab- While naming…" — the key colliding
	// with its own separator.
	for _, line := range lines {
		if i := strings.Index(line, "- "); i > 0 {
			require.Equalf(t, " ", line[i-1:i],
				"the key column must keep a blank cell before its separator: %q", line)
		}
	}
	for _, line := range lines {
		require.LessOrEqualf(t, lipgloss.Width(line), narrow,
			"no rendered row may exceed the content width: %q", line)
	}
}

// TestGeneralHelpReadsInFarFewerPagesAt80x24 is the user-visible metric from
// #3629: the help was 8 page-downs at 80x24 versus 1 at 200x50, because every
// description had 16 columns and wrapped into 3-5-line ribbons.
//
// 3 is the structural floor at this size — ~36 rows plus the header and section
// chrome is more than twice an 18-row viewport even if every row were a single
// line — so this asserts the count is near it, not at 1.
func TestGeneralHelpReadsInFarFewerPagesAt80x24(t *testing.T) {
	pagesToBottom := func(w, hgt int) int {
		h := newTestHome(t)
		resizeHome(h, w, hgt)
		_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)
		pages := 0
		for strings.Contains(xansi.Strip(h.View()), "↓ more") {
			_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyPgDown})
			pages++
			require.Less(t, pages, 30, "paging must reach the bottom of the help")
		}
		return pages
	}

	narrow := pagesToBottom(80, 24)
	require.LessOrEqualf(t, narrow, 5,
		"the help must read in at most 5 page-downs at 80x24 (it was 8); got %d", narrow)

	wide := pagesToBottom(200, 50)
	require.LessOrEqualf(t, wide, 1,
		"200x50 already read in one page and must not regress; got %d", wide)
}

// TestInstanceStartHelpScrollsRatherThanDismissingAt80x24 is the #3628
// regression. The first-run "Session created" screen overflows 80x24, so
// TextOverlay paints "↓ more" — and the overlay closed on ANY key, including
// the ↓ it advertised. Being a once-per-home screen, that took the whole tail
// with it permanently: the tab line, the kill key, and the "Actions:" section
// whose "esc close" is the only instruction for getting out.
func TestInstanceStartHelpScrollsRatherThanDismissingAt80x24(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	local := newStartedInstance(t, "todo-notes")

	_, _ = h.showHelpScreen(helpStart(local), nil)
	require.Equal(t, stateHelp, h.state)
	require.True(t, h.textOverlay.Scrollable(),
		"precondition: the session-created screen overflows 80x24")
	initial := xansi.Strip(h.View())
	require.Contains(t, initial, "Session created")
	require.Contains(t, initial, "↓ more", "precondition: the overlay advertises more content")
	require.NotContains(t, initial, "esc close",
		"precondition: the line naming the dismiss key starts below the fold")

	// The advertised key must scroll, not close the screen out from under it.
	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyDown},
		{Type: tea.KeyRunes, Runes: []rune("j")},
		{Type: tea.KeyPgDown},
		{Type: tea.KeySpace},
	} {
		_, _ = h.handleHelpState(msg)
		require.Equalf(t, stateHelp, h.state, "%v must scroll the session-created overlay, not dismiss it", msg)
		require.Falsef(t, h.textOverlay.Dismissed, "%v must not mark the overlay dismissed", msg)
	}
	require.Contains(t, xansi.Strip(h.View()), "↑ more", "the viewport must have moved down")

	// The tail is reachable, so the screen can actually be finished.
	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEnd})
	bottom := xansi.Strip(h.View())
	require.Contains(t, bottom, "Actions:", "End must reach the section that was unreachable")
	require.Contains(t, bottom, "esc close", "the dismiss instruction must be readable")

	// …and scrolling back up still works.
	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyHome})
	require.Contains(t, xansi.Strip(h.View()), "Session created", "Home must return to the top")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "esc — the key the screen names — closes it")
}

// The other half of #3628: the seen bit is a record of the user DISMISSING the
// screen, not of af painting it. Burning it on display is what made the lost
// tail permanent — the screen never came back to be finished.
func TestFirstRunHelpMarkedSeenOnDismissNotDisplay(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	local := newStartedInstance(t, "todo-notes")
	mask := helpTypeInstanceStart{}.mask()

	_, _ = h.showHelpScreen(helpStart(local), nil)
	require.Equal(t, stateHelp, h.state)
	require.Zero(t, h.appState.GetHelpScreensSeen()&mask,
		"displaying the screen must not mark it seen")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyDown})
	require.Equal(t, stateHelp, h.state)
	require.Zero(t, h.appState.GetHelpScreensSeen()&mask,
		"scrolling is reading, not dismissing — the screen is still open")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state)
	require.NotZero(t, h.appState.GetHelpScreensSeen()&mask,
		"dismissing the screen is what records it as seen")

	// And it stays a one-shot: a second creation does not re-open it.
	_, _ = h.showHelpScreen(helpStart(local), nil)
	require.Equal(t, stateDefault, h.state, "a dismissed one-shot screen must not come back")
}

// A screen the user never dismissed has not been seen: quitting with the
// overlay open must leave it to be shown again (#3628).
func TestFirstRunHelpAbandonedUndismissedIsNotSeen(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	local := newStartedInstance(t, "todo-notes")
	mask := helpTypeInstanceStart{}.mask()

	_, _ = h.showHelpScreen(helpStart(local), nil)
	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyPgDown})
	require.Equal(t, stateHelp, h.state, "precondition: the user is still reading")

	// The process ends here — nothing dismissed the overlay. The gate the next
	// run evaluates is GetHelpScreensSeen()&mask, and it is still open.
	require.Zero(t, h.appState.GetHelpScreensSeen()&mask)
	h.state, h.textOverlay = stateDefault, nil
	_, _ = h.showHelpScreen(helpStart(local), nil)
	require.Equal(t, stateHelp, h.state,
		"a screen abandoned mid-read must be offered again")
}

// The attach overlay keeps its enter/esc policy, and gains scrolling for free
// when its content overflows — the class property #3628 asks for, not a
// per-caller patch.
func TestAttachHelpScrollsWhenItOverflowsAndKeepsItsPolicy(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)

	_, _ = h.showHelpScreen(helpTypeInstanceAttach{agent: tmux.ProgramClaude}, nil)
	require.Equal(t, stateHelp, h.state)
	// Squeeze the viewport so the screen overflows regardless of copy length.
	h.textOverlay.SetHeight(8)
	require.True(t, h.textOverlay.Scrollable(), "precondition: the attach screen overflows")

	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyDown})
	require.Equal(t, stateHelp, h.state, "↓ must scroll the attach overlay")
	require.Contains(t, xansi.Strip(h.textOverlay.Render()), "↑ more")

	// Esc still cancels, exactly as attachHelpDismissPolicy says.
	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "esc must still cancel the attach overlay")
}

// The general `?` help is unchanged except that enter and q now close it too:
// it is the screen the dismiss-key set was designed around (#1290/#1399/#1447).
func TestGeneralHelpClosesOnEnterAndQuitKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}},
		{"quit key", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			resizeHome(h, 80, 24)
			_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)
			require.Equal(t, stateHelp, h.state)

			_, _ = h.handleHelpState(tc.msg)
			require.Equal(t, stateDefault, h.state)
		})
	}
}

// TestGeneralHelpHidesAliasShadowedByAQuitRebind answers the Codex review on
// #3634. Adding q to the dismiss set means a user who configures
// `[keys] quit = "pgdown"` has made pgdn a dismiss key — so the help must stop
// advertising it as a paging control, exactly as it already does for a rebound
// help key (TestGeneralHelpHidesPagingAliasShadowedByRebind). Dismissal keeps
// precedence over paging; the copy is what has to follow.
func TestGeneralHelpHidesAliasShadowedByAQuitRebind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		prefix   string
		hidden   string
		msg      tea.KeyMsg
	}{
		{"paging alias", "pgdown", "Page:", "pgdn", tea.KeyMsg{Type: tea.KeyPgDown}},
		{"jump alias", "home", "Line:", "home", tea.KeyMsg{Type: tea.KeyHome}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, keys.ApplyOverrides(map[string][]string{"quit": {tc.override}}))
			t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

			var line string
			for _, l := range strings.Split(xansi.Strip(helpTypeGeneral{}.toContent()), "\n") {
				if strings.HasPrefix(l, tc.prefix) {
					line = l
					break
				}
			}
			require.NotEmpty(t, line, "the help must still render its %s controls", tc.prefix)
			require.NotContainsf(t, line, tc.hidden,
				"a quit rebind made %q a dismiss key; the help must not advertise it as navigation", tc.hidden)

			// …and the key really does dismiss, so the copy is telling the truth.
			h := newTestHome(t)
			resizeHome(h, 80, 24)
			_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)
			require.Equal(t, stateHelp, h.state)
			_, _ = h.handleHelpState(tc.msg)
			require.Equal(t, stateDefault, h.state,
				"the configured quit key must close the help, matching what the copy no longer promises")
		})
	}
}
