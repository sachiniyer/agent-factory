package app

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// submitJumpTabMiss opens the jump-to-tab prompt, types query, and submits
// Enter — the gesture that produces a miss notice. Returns the model/cmd pair
// from the submit so a test can assert on the scheduled expiry.
func submitJumpTabMiss(t *testing.T, h *home, query string) (tea.Model, tea.Cmd) {
	t.Helper()
	_, _ = h.showJumpTabPrompt()
	require.NotNil(t, h.promptOverlay, "the jump prompt must open before a miss can be produced")
	_, _ = h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(query)})
	return h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyEnter})
}

// TestJumpTabMissNoticeLifecycle pins the notice-lifecycle invariants the
// jump-to-tab miss path must satisfy, mirroring TestRenameTabNoticeExpires for
// the rename analog. The miss notice must route through handleNotice (not
// errBox.SetNotice): it advances the notice generation and schedules the usual
// 3-second expiry, so the miss neither lingers forever nor gets erased early by
// an older notice's pending timer.
func TestJumpTabMissNoticeLifecycle(t *testing.T) {
	t.Run("missSchedulesItsOwnExpiry", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-miss-expiry")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		selectInstance(h, inst)

		_, cmd := submitJumpTabMiss(t, h, "missing")
		require.NotNil(t, cmd, "the miss must schedule its notice's expiry")
	})

	t.Run("missTakesNewGeneration", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-miss-gen")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		selectInstance(h, inst)

		_ = h.handleNotice(fmt.Errorf("an older notice"))
		older := h.transientNoticeID

		_, _ = submitJumpTabMiss(t, h, "missing")

		require.Greater(t, h.transientNoticeID, older,
			"the miss must take a new generation")
	})

	t.Run("olderTimerMustNotEraseMissFromBar", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-miss-erase")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		selectInstance(h, inst)

		_ = h.handleNotice(fmt.Errorf("an older notice"))
		older := h.transientNoticeID

		_, _ = submitJumpTabMiss(t, h, "missing")

		h.errBox.SetSize(200, 1)
		require.Contains(t, h.errBox.FullError(), "no tab matches")

		_, _ = h.Update(hideErrMsg{noticeID: older})
		require.Contains(t, h.errBox.FullError(), "no tab matches",
			"an older notice's timer must not erase the miss notice from the bar")

		retained, _ := h.errBox.RetainedNotice()
		require.Contains(t, retained, "no tab matches",
			"the retained notice survives Expire, so E details still works")
	})

	t.Run("missOwnExpiryClearsBarButKeepsRetained", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-miss-own-expiry")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		selectInstance(h, inst)

		_, _ = submitJumpTabMiss(t, h, "missing")

		_, _ = h.Update(hideErrMsg{noticeID: h.transientNoticeID})
		require.Empty(t, h.errBox.FullError(),
			"the miss must expire from the bar on its own schedule")

		retained, isFailure := h.errBox.RetainedNotice()
		require.Contains(t, retained, "no tab matches",
			"the miss stays reachable via E details after expiry")
		require.False(t, isFailure,
			"a jump miss is a deliberate decline, so E details titles it 'Last notice', not 'Last error'")
	})
}

// TestJumpTabPromptReportsLiveMatch is #5587: the prompt used to hold its
// answer until Enter, so a miss cost the whole jump — type, submit, get told
// no, retype blind. Each keystroke must update the status line with where the
// current query would land, through the same resolver Enter uses.
func TestJumpTabPromptReportsLiveMatch(t *testing.T) {
	typeKeys := func(h *home, s string) {
		t.Helper()
		for _, r := range s {
			_, _ = h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	statusOf := func(h *home) string {
		t.Helper()
		return xansi.Strip(h.promptOverlay.Render())
	}

	t.Run("shows the target as it becomes unique", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-live-target")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		inst.AddTabForTest("vagrant", session.TabKindShell)
		selectInstance(h, inst)

		_, _ = h.showJumpTabPrompt()
		require.NotNil(t, h.promptOverlay)
		typeKeys(h, "v")
		require.Contains(t, statusOf(h), "more than one match",
			"a shared prefix must say so, not silently point at one tab")
		typeKeys(h, "s")
		require.Contains(t, statusOf(h), "→ VS Code (vscode)",
			"the status must name the tab Enter would land on")
	})

	t.Run("says no match before Enter is pressed", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-live-miss")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		selectInstance(h, inst)

		_, _ = h.showJumpTabPrompt()
		typeKeys(h, "zz")
		require.Contains(t, statusOf(h), "no match")
	})

	t.Run("status tracks deletions too", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-live-back")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		selectInstance(h, inst)

		_, _ = h.showJumpTabPrompt()
		typeKeys(h, "zz")
		require.Contains(t, statusOf(h), "no match")
		_, _ = h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyBackspace})
		_, _ = h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyBackspace})
		require.NotContains(t, statusOf(h), "no match",
			"an empty query must clear the status, not keep a stale miss")
	})

	t.Run("an ordinal names its tab", func(t *testing.T) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "jump-live-num")
		inst.AddTabForTest("vscode", session.TabKindVSCode)
		selectInstance(h, inst)

		_, _ = h.showJumpTabPrompt()
		typeKeys(h, "2")
		require.Contains(t, statusOf(h), "→ VS Code (vscode)")
	})
}

// TestJumpTabPromptSizesToContent is the geometry half of #5587: one line of
// input must not render inside a screen of blank rows. The dialog stays a
// handful of rows tall even on a large terminal.
func TestJumpTabPromptSizesToContent(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "jump-size")
	inst.AddTabForTest("vscode", session.TabKindVSCode)
	selectInstance(h, inst)
	resizeHome(h, 120, 40)

	_, _ = h.showJumpTabPrompt()
	require.NotNil(t, h.promptOverlay)
	_, _ = h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})

	rows := strings.Count(strings.TrimRight(h.promptOverlay.Render(), "\n"), "\n") + 1
	require.Less(t, rows, 15,
		"a one-line jump prompt must hug its content, not fill a %d-row terminal", 40)
}
