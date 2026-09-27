package app

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #4940: a clipped notice advertises the key that opens it in full, and that
// hint must appear exactly where pressing the key opens the details. These
// tests pair each state's hint with what the key actually does there.

// clippedNoticeTail is the part of the notice the bar clips; only the details
// overlay shows it.
const clippedNoticeTail = "https://example.invalid/session/4940"

var detailsKeyE = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("E")}

func raiseClippedNotice(t *testing.T, h *home) {
	t.Helper()
	msg := "no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS)" +
		strings.Repeat(" · retry later", 12) + "; Session URL: " + clippedNoticeTail
	require.NotNil(t, h.handleError(errors.New(msg)))
}

// renderedBar renders the whole frame, as the terminal would, and returns the
// notice line behind whatever is on top.
func renderedBar(h *home) string {
	_ = h.View()
	return h.errBox.String()
}

func detailsOpen(h *home) bool {
	return h.state == stateHelp && h.textOverlay != nil &&
		strings.Contains(h.textOverlay.Render(), clippedNoticeTail)
}

// TestNoticeDetailsHintHiddenBehindHelpOverlay is the issue's repro: `?` help
// opened over a clipped notice. The help overlay's handler owns the keyboard,
// so E never reaches the details and the bar behind it must not say it does.
func TestNoticeDetailsHintHiddenBehindHelpOverlay(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	raiseClippedNotice(t, h)
	require.Contains(t, renderedBar(h), "E details", "precondition: the clipped notice advertises E")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	require.Equal(t, stateHelp, h.state, "precondition: help is open")
	assert.NotContains(t, renderedBar(h), "details", "no key opens the details behind help")

	_, _ = h.handleKeyPress(detailsKeyE)
	assert.False(t, detailsOpen(h), "E behind help must not open the details, and the bar never said it would")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state)
	assert.Contains(t, renderedBar(h), "E details", "with help closed, E opens the details again and says so")
	_, _ = h.handleKeyPress(detailsKeyE)
	assert.True(t, detailsOpen(h), "control: the advertised key works")
}

// TestNoticeDetailsHintHiddenBehindTheDetailsThemselves: once E has opened the
// details, the notice is still on the bar behind them, and the details overlay
// dismisses on its own keys, so E is not advertised there either.
func TestNoticeDetailsHintHiddenBehindTheDetailsThemselves(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	raiseClippedNotice(t, h)

	_, _ = h.handleKeyPress(detailsKeyE)
	require.True(t, detailsOpen(h), "precondition: the details are open")
	assert.NotContains(t, renderedBar(h), "details")
}

// TestNoticeDetailsHintHiddenBehindConfirmation: a confirmation dialog answers
// y/n/esc and nothing else.
func TestNoticeDetailsHintHiddenBehindConfirmation(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	raiseClippedNotice(t, h)
	_ = h.confirmAction("Delete session?", nil)
	require.Equal(t, stateConfirm, h.state, "precondition: the dialog is open")

	assert.NotContains(t, renderedBar(h), "details")
	_, _ = h.handleKeyPress(detailsKeyE)
	assert.False(t, detailsOpen(h))
}

// TestNoticeDetailsHintHiddenWhileProjectsFocused: no overlay at all, but the
// focused Projects section is captive (#1620) and swallows E as a no-op.
func TestNoticeDetailsHintHiddenWhileProjectsFocused(t *testing.T) {
	h := paneTestHome(t)
	raiseClippedNotice(t, h)
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active(), "precondition: Projects has focus")

	assert.NotContains(t, renderedBar(h), "details", "Projects swallows E, so the bar must not offer it")
	_, _ = h.Update(detailsKeyE)
	assert.False(t, detailsOpen(h))

	h.focusRegion(layout.RegionTree)
	assert.Contains(t, renderedBar(h), "E details", "control: from the tree, E works and is advertised")
	_, _ = h.Update(detailsKeyE)
	assert.True(t, detailsOpen(h))
}

// TestNoticeDetailsHintHiddenWhileInteractive: in interactive mode every key
// goes to the agent, E included, so the bar must not claim E opens anything.
func TestNoticeDetailsHintHiddenWhileInteractive(t *testing.T) {
	h, _ := liveTestHome(t)
	stubLiveTermFactory(t)
	h.syncLiveTermPane()
	fake := focusedFake(h)
	require.NotNil(t, fake, "precondition: the focused pane has a live attachment")
	h.setInteractive(true)
	raiseClippedNotice(t, h)

	assert.NotContains(t, renderedBar(h), "details", "E is typed into the agent, not dispatched")
	_, _ = h.handleKeyPress(detailsKeyE)
	assert.False(t, detailsOpen(h))
	assert.Equal(t, []string{"E"}, fake.keys, "E went to the agent")
}

// TestNoticeDetailsHintOnlyWhereDispatchOpensTheDetails sweeps every state but
// the two that open the details (stateDefault, and stateNew through ctrl+e),
// with and without a naming form behind it: none may advertise a details key.
func TestNoticeDetailsHintOnlyWhereDispatchOpensTheDetails(t *testing.T) {
	for _, naming := range []bool{false, true} {
		h := newTestHome(t)
		resizeHome(h, 80, 24)
		if naming {
			inst, err := session.NewInstance(session.InstanceOptions{Title: "form", Path: t.TempDir(), Program: "claude"})
			require.NoError(t, err)
			h.namingInstance = inst
		}
		for s := stateHelp; s <= stateRenameTab; s++ {
			h.state = s
			_, hide := h.noticeDetailsHint()
			assert.True(t, hide, "state %d (naming form open: %v) must advertise no details key", s, naming)
		}
	}
}

// TestNoticeDetailsHintNeverAdvertisesTheHardExit: ctrl+c quits before any
// binding is consulted, so an error_details rebind that includes it must
// advertise only the keys that open the details, and one made of nothing else
// advertises none (Codex on #4941).
func TestNoticeDetailsHintNeverAdvertisesTheHardExit(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	require.NoError(t, keys.ApplyOverrides(map[string][]string{"error_details": {"ctrl+c", "X"}}))
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	raiseClippedNotice(t, h)
	bar := renderedBar(h)
	assert.Contains(t, bar, "X details", "the key that opens the details is advertised")
	assert.NotContains(t, bar, "ctrl+c", "ctrl+c quits, so the bar must not offer it")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	assert.True(t, detailsOpen(h), "the advertised key opens the details")

	require.NoError(t, keys.ApplyOverrides(map[string][]string{"error_details": {"ctrl+c"}}))
	h = newTestHome(t)
	resizeHome(h, 80, 24)
	raiseClippedNotice(t, h)
	assert.NotContains(t, renderedBar(h), "details", "no key but the hard exit, so no hint")
}
