package overlay

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmationOverlay_HandleKeyPress_CtrlC(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	ctrlCMsg := tea.KeyMsg{Type: tea.KeyCtrlC}
	shouldClose := overlay.HandleKeyPress(ctrlCMsg)

	assert.True(t, shouldClose, "ctrl+c should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, cancelCalled, "OnCancel should be called")
	assert.False(t, confirmCalled, "OnConfirm should not be called")
}

func TestConfirmationOverlay_HandleKeyPress_Esc(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	escMsg := tea.KeyMsg{Type: tea.KeyEsc}
	shouldClose := overlay.HandleKeyPress(escMsg)

	assert.True(t, shouldClose, "esc should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, cancelCalled, "OnCancel should be called")
}

func TestConfirmationOverlay_HandleKeyPress_ConfirmKey(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	yMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}
	shouldClose := overlay.HandleKeyPress(yMsg)

	assert.True(t, shouldClose, "confirm key should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, confirmCalled, "OnConfirm should be called")
}

func TestConfirmationOverlay_HandleKeyPress_CancelKey(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	nMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}
	shouldClose := overlay.HandleKeyPress(nMsg)

	assert.True(t, shouldClose, "cancel key should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, cancelCalled, "OnCancel should be called")
	assert.False(t, confirmCalled, "OnConfirm should not be called")
}

// TestConfirmationOverlay_HandleKeyPress_EscBeatsConfirmKey verifies the
// invariant from #468: when ConfirmKey is set to "esc", pressing ESC must
// still cancel rather than silently confirming a destructive action.
func TestConfirmationOverlay_HandleKeyPress_EscBeatsConfirmKey(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")
	overlay.SetConfirmKey("esc")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	escMsg := tea.KeyMsg{Type: tea.KeyEsc}
	shouldClose := overlay.HandleKeyPress(escMsg)

	assert.True(t, shouldClose, "esc should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, cancelCalled, "OnCancel should be called even when ConfirmKey is esc")
	assert.False(t, confirmCalled, "OnConfirm must not be called for esc")
}

// TestConfirmationOverlay_HandleKeyPress_CtrlCBeatsConfirmKey verifies the
// invariant from #468 for Ctrl+C: it must always cancel, even if ConfirmKey
// is misconfigured to "ctrl+c".
func TestConfirmationOverlay_HandleKeyPress_CtrlCBeatsConfirmKey(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")
	overlay.SetConfirmKey("ctrl+c")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	ctrlCMsg := tea.KeyMsg{Type: tea.KeyCtrlC}
	shouldClose := overlay.HandleKeyPress(ctrlCMsg)

	assert.True(t, shouldClose, "ctrl+c should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, cancelCalled, "OnCancel should be called even when ConfirmKey is ctrl+c")
	assert.False(t, confirmCalled, "OnConfirm must not be called for ctrl+c")
}

func TestConfirmationOverlay_HandleKeyPress_OtherKey(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	otherMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}
	shouldClose := overlay.HandleKeyPress(otherMsg)

	assert.False(t, shouldClose, "other keys should not close the overlay")
	assert.False(t, overlay.Dismissed, "overlay should not be dismissed")
	assert.False(t, cancelCalled, "OnCancel should not be called")
	assert.False(t, confirmCalled, "OnConfirm should not be called")
}

// TestConfirmationOverlay_HandleKeyPress_EnterConfirmsDefault pins #2405: on an
// ordinary (un-escalated) confirmation, enter is an affirmative alias for the
// confirm key. Before the fix enter fell through to the ignore branch, so a
// user's reflexive Enter left the dialog sitting open with no visible effect.
func TestConfirmationOverlay_HandleKeyPress_EnterConfirmsDefault(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	confirmCalled := false
	overlay.OnConfirm = func() { confirmCalled = true }
	cancelCalled := false
	overlay.OnCancel = func() { cancelCalled = true }

	shouldClose := overlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, shouldClose, "enter should confirm an ordinary dialog")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, confirmCalled, "OnConfirm should be called for enter")
	assert.False(t, cancelCalled, "OnCancel should not be called for enter")
}

// TestConfirmationOverlay_HandleKeyPress_EnterIgnoredWhenEscalated pins the
// safety half of #2405: a dialog that escalated to a distinct confirm key (root
// #1238, unmerged #2022) must NOT accept enter, or the exact D+enter reflex the
// escalation defends against would dispatch the irreversible action. The named
// key must still confirm.
func TestConfirmationOverlay_HandleKeyPress_EnterIgnoredWhenEscalated(t *testing.T) {
	overlay := NewConfirmationOverlay("[!] Kill session 'root'?")
	overlay.SetConfirmKey("k")

	confirmCalled := false
	overlay.OnConfirm = func() { confirmCalled = true }
	cancelCalled := false
	overlay.OnCancel = func() { cancelCalled = true }

	shouldClose := overlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, shouldClose, "enter must be ignored on an escalated dialog")
	assert.False(t, overlay.Dismissed, "overlay must stay open")
	assert.False(t, confirmCalled, "OnConfirm must not be called by enter")
	assert.False(t, cancelCalled, "OnCancel must not be called by enter")

	shouldClose = overlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	assert.True(t, shouldClose, "the escalated key must still confirm")
	assert.True(t, confirmCalled, "OnConfirm should be called for the named key")
}

// TestConfirmationOverlay_Instruction_AdvertisesEnterOnlyWhenAccepted pins the
// prompt copy: an ordinary dialog advertises enter as a confirm alias, while an
// escalated dialog names only its distinct key so the copy never promises an
// enter it will refuse.
func TestConfirmationOverlay_Instruction_AdvertisesEnterOnlyWhenAccepted(t *testing.T) {
	def := NewConfirmationOverlay("[!] Kill session 'alpha'?")
	def.SetWidth(50)
	def.SetMaxSize(80, 24)
	assert.Contains(t, overlayProse(def.Render()), "y/enter confirm",
		"an ordinary dialog must advertise enter as a confirm alias")

	esc := NewConfirmationOverlay("[!] Kill session 'root'?")
	esc.SetConfirmKey("k")
	esc.SetWidth(50)
	esc.SetMaxSize(80, 24)
	rendered := overlayProse(esc.Render())
	assert.Contains(t, rendered, "k confirm",
		"an escalated dialog still names its distinct key")
	assert.NotContains(t, rendered, "enter",
		"an escalated dialog must not offer enter")
}

// overlayProse reduces a rendered overlay to the prose inside it, so a multi-word
// assertion matches content rather than failing on the wrap.
//
// It strips ANSI first, then the frame. Both are required: whether the border
// arrives as "│" glyphs or as colour-escaped spaces depends on lipgloss's colour
// profile, which is process-global — so a sibling test that enables colour
// changes what this renders. Stripping only the glyphs passes alone and fails in
// the full package run.
func overlayProse(rendered string) string {
	frame := strings.NewReplacer(
		"│", " ", "─", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ",
	)
	return strings.Join(strings.Fields(frame.Replace(renderedText(rendered))), " ")
}

// TestConfirmationOverlay_GuardedMessageIsNeverClipped: a guarded overlay (one
// with a detail set) must render its message in full at the top of the body
// window. The message carries the consequences the user is consenting to — it
// leads the scrollable body, so it is what the reader sees first (#1973, and
// reachable in full since #5171).
func TestConfirmationOverlay_GuardedMessageIsNeverClipped(t *testing.T) {
	c := NewConfirmationOverlay("[!] Delete project 'acme'?\n1 in-place session torn down — not restorable.\n2 sessions archived — restorable.")
	c.SetDetail("Its worktree is yours — the branch and uncommitted changes stay exactly where they are, but the session and its agent are gone. Restore an archived session to bring the project back.")
	c.SetWidth(50)
	c.SetMaxSize(40, 10)

	rendered := overlayProse(c.Render())
	assert.Contains(t, rendered, "1 in-place session torn down — not restorable.",
		"the destructive consequence must survive at the declared 40x10 floor")
	assert.Contains(t, rendered, "2 sessions archived — restorable.",
		"and so must the other half of the split")
	assert.Contains(t, rendered, "confirm",
		"the confirm prompt must render alongside it")
}

// TestConfirmationOverlay_OverflowIsAnnouncedWithScrollKeys: when the body does
// not fit, the overlay must SAY so — and say how to read the rest. A bare "…"
// is indistinguishable from "there was nothing more to say", and "resize to
// read" made the user do the work the dialog should do (#5171).
func TestConfirmationOverlay_OverflowIsAnnouncedWithScrollKeys(t *testing.T) {
	c := NewConfirmationOverlay("[!] Delete project 'acme'?\n1 in-place session torn down — not restorable.")
	c.SetDetail("Line one of elaboration that will not fit. Line two of elaboration. Line three of elaboration. Line four of elaboration that keeps going for a while.")
	c.SetWidth(50)
	c.SetMaxSize(40, 10)

	rendered := renderedText(c.Render())
	assert.NotContains(t, rendered, "resize to read",
		"the terminal is not the scroll affordance — the dialog pages itself")
	assert.Regexp(t, `↓ \d+ more line`, rendered, "the notice must count what the window hides")
	assert.Contains(t, rendered, "↑/↓ or j/k", "the notice must name the keys")
}

// TestConfirmationOverlay_CompactSizeScrollsNotRefuses: a destructive confirm
// whose body is taller than the window does not hide text and does not refuse
// — it pages. The refusal is reserved for windows that cannot show even one
// body row (#1973's trigger, kept; its remedy is now scroll, #5171). The
// trigger is realistic rather than contrived: at the declared 40x10 floor a
// long project name wraps the title onto a second line, which pushes the body
// past the four-line window.
func TestConfirmationOverlay_CompactSizeScrollsNotRefuses(t *testing.T) {
	c := NewConfirmationOverlay("[!] Delete project 'a-project-with-a-very-long-name-indeed'?\n3 in-place sessions torn down — not restorable.\n7 sessions archived — restorable.")
	c.SetDetail("Elaboration that does not matter here.")
	c.SetWidth(50)
	c.SetMaxSize(40, 10)

	confirmed := false
	c.OnConfirm = func() { confirmed = true }
	cancelled := false
	c.OnCancel = func() { cancelled = true }

	require.True(t, c.Scrollable(), "the oversized body must page rather than refuse or clip")
	rendered := overlayProse(c.Render())
	assert.NotContains(t, rendered, "Too small to confirm safely",
		"one readable row is enough — the rest is reachable")
	for i := 0; i < 50; i++ {
		c.ScrollDown()
	}
	assert.Contains(t, overlayProse(c.Render()), "Elaboration",
		"the hidden tail must be reachable by scrolling")

	shouldClose := c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	assert.True(t, shouldClose, "a dialog the user could read in full must confirm")
	assert.True(t, confirmed)
	assert.False(t, cancelled)
}

// TestConfirmationOverlay_RefusalSurvivesDegenerateSizes: the refusal must say
// something true even when the window is far too small for its own explanation.
// Windowing it would degrade the refusal into a bare "… N more lines" notice —
// swallowing the reason at exactly the moment the reason is the whole point,
// which is the same defect one level up. It fires only where no body row fits
// beside the prompt at all.
func TestConfirmationOverlay_RefusalSurvivesDegenerateSizes(t *testing.T) {
	for _, size := range [][2]int{{40, 5}, {30, 5}, {24, 5}, {24, 6}} {
		c := NewConfirmationOverlay("[!] Delete project 'acme'?\n2 in-place sessions torn down — not restorable.\n5 sessions archived — restorable.")
		c.SetDetail("Elaboration.")
		c.SetWidth(50)
		c.SetMaxSize(size[0], size[1])

		confirmed := false
		c.OnConfirm = func() { confirmed = true }

		rendered := overlayProse(c.Render())
		assert.Contains(t, rendered, "Too small", "at %dx%d the refusal must still name itself, got: %q", size[0], size[1], rendered)
		assert.NotRegexp(t, `^\s*…`, rendered, "the refusal must never degrade into a bare ellipsis at %dx%d", size[0], size[1])

		c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
		assert.False(t, confirmed, "a refused dialog must not confirm at %dx%d", size[0], size[1])
	}
}

// TestConfirmationOverlay_UnguardedKeepsConfirming: overlays with no detail
// (the existing archive/kill confirms) scroll like everything else but never
// refuse. The refusal is opt-in via SetDetail, so this fix does not silently
// make every confirm in the app refusable.
func TestConfirmationOverlay_UnguardedKeepsConfirming(t *testing.T) {
	c := NewConfirmationOverlay(strings.Repeat("a long confirmation message that will certainly not fit. ", 8))
	c.SetWidth(50)
	c.SetMaxSize(30, 6)

	confirmed := false
	c.OnConfirm = func() { confirmed = true }

	assert.NotContains(t, renderedText(c.Render()), "Too small to confirm safely",
		"an unguarded overlay must not start refusing")
	assert.True(t, c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}))
	assert.True(t, confirmed, "unguarded confirms keep working exactly as before")
}

// TestConfirmationOverlay_PendingWithholdsConfirm: a dialog opened before its
// copy is complete (#4848) must refuse every confirm gesture — the named key and
// the enter alias — while cancel still works, and must confirm normally once the
// pending note is cleared.
func TestConfirmationOverlay_PendingWithholdsConfirm(t *testing.T) {
	c := NewConfirmationOverlay("Delete session 'alpha'?")
	c.SetWidth(60)
	confirmed := 0
	c.OnConfirm = func() { confirmed++ }
	c.SetPending("Checking for unsaved work…")

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("y")},
		{Type: tea.KeyEnter},
	} {
		assert.False(t, c.HandleKeyPress(key), "%q must not close a pending dialog", key.String())
	}
	assert.Zero(t, confirmed, "a pending dialog must not confirm")
	assert.False(t, c.Dismissed)

	rendered := c.Render()
	assert.Contains(t, rendered, "Checking for unsaved work…", "the pending note is the visible affordance")
	assert.NotContains(t, rendered, "y/enter confirm", "a refused confirm key must not be advertised")
	assert.Contains(t, rendered, "n/esc cancel", "cancel stays available and advertised")

	c.SetPending("")
	assert.True(t, c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}))
	assert.Equal(t, 1, confirmed, "clearing the note restores the confirm")
}

// TestConfirmationOverlay_PendingStillCancels: esc and the cancel key are never
// withheld, or a slow result would trap the user in the dialog.
func TestConfirmationOverlay_PendingStillCancels(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("n")},
	} {
		c := NewConfirmationOverlay("Delete session 'alpha'?")
		c.SetPending("Checking for unsaved work…")
		cancelled := false
		c.OnCancel = func() { cancelled = true }
		assert.True(t, c.HandleKeyPress(key), "%q must close a pending dialog", key.String())
		assert.True(t, cancelled)
	}
}

// overflowingConfirm builds the #5171 shape — a destructive headline followed
// by more warning lines than the window can hold — at the given terminal size.
// Each warning leads with a unique short token ("Warning NN") so an assertion
// can prove it reached the screen without depending on the wrap.
func overflowingConfirm(warnings int, termW, termH int) *ConfirmationOverlay {
	lines := make([]string, 0, warnings+1)
	lines = append(lines, "[!] Delete session 'risky'?")
	for i := 1; i <= warnings; i++ {
		lines = append(lines, fmt.Sprintf("Warning %02d — consequences that must not be hidden.", i))
	}
	c := NewConfirmationOverlay(strings.Join(lines, "\n"))
	c.SetDetail("Final elaboration line every reader must reach.")
	c.SetWidth(50)
	c.SetMaxSize(termW, termH)
	return c
}

// TestConfirmationOverlay_EveryBodyLineReachable is the #5171 property: at the
// ordinary sizes the bug was reported against, a destructive confirmation must
// be able to bring EVERY line of its body on screen — the risk text, the
// elaboration, all of it — while the confirm prompt stays put.
func TestConfirmationOverlay_EveryBodyLineReachable(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 20}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			c := overflowingConfirm(12, size[0], size[1])
			require.True(t, c.Scrollable(), "the body must overflow at %dx%d", size[0], size[1])

			var prose []string
			for i := 0; i < 100; i++ {
				frame := overlayProse(c.Render())
				prose = append(prose, frame)
				assert.Contains(t, frame, "confirm", "the prompt must stay pinned at scroll %d", c.scroll)
				assert.Contains(t, frame, "cancel", "the prompt must stay pinned at scroll %d", c.scroll)
				c.ScrollDown()
			}
			joined := strings.Join(prose, " ")
			for i := 1; i <= 12; i++ {
				assert.Contains(t, joined, fmt.Sprintf("Warning %02d", i),
					"warning %d must be reachable by scrolling at %dx%d", i, size[0], size[1])
			}
			assert.Contains(t, joined, "Final elaboration",
				"the detail tail must be reachable by scrolling")
		})
	}
}

// TestConfirmationOverlay_ScrollNoticeTracksPosition: the footer must tell the
// truth about which directions hide content — "↓ N more" while anything sits
// below the window, "↑ N more" once scrolling has passed lines, and neither
// direction it does not apply to.
func TestConfirmationOverlay_ScrollNoticeTracksPosition(t *testing.T) {
	c := overflowingConfirm(12, 80, 24)

	top := renderedText(c.Render())
	assert.Regexp(t, `↓ \d+ more lines`, top, "a fresh overflow advertises what is hidden below")
	assert.Contains(t, top, "↑/↓ or j/k", "the notice must name the keys")
	assert.NotRegexp(t, `↑ \d+ more lines`, top, "nothing is hidden above at scroll 0")

	for i := 0; i < 6; i++ {
		c.ScrollDown()
	}
	mid := renderedText(c.Render())
	assert.Regexp(t, `↑ \d+ more lines`, mid, "scrolled-down content must be announced")
	assert.Regexp(t, `↓ \d+ more lines`, mid, "content still below must still be announced")

	for i := 0; i < 100; i++ {
		c.ScrollDown()
	}
	bottom := renderedText(c.Render())
	assert.Regexp(t, `↑ \d+ more lines`, bottom, "the passed content stays announced at the bottom")
	assert.NotRegexp(t, `↓ \d+ more lines`, bottom, "the bottom must stop advertising more content below")
}

// TestConfirmationOverlay_ScrollKeysPageTheBody: every scrolling input the
// task names must move the body window — and none of them may close the
// dialog. The keys are checked through HandleKeyPress because that is the
// path app/handle_overlay.go forwards.
func TestConfirmationOverlay_ScrollKeysPageTheBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		msg   tea.KeyMsg
		delta func(budget int) int
	}{
		{"up", tea.KeyMsg{Type: tea.KeyUp}, func(int) int { return -1 }},
		{"k", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}, func(int) int { return -1 }},
		{"down", tea.KeyMsg{Type: tea.KeyDown}, func(int) int { return 1 }},
		{"j", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}, func(int) int { return 1 }},
		{"pgup", tea.KeyMsg{Type: tea.KeyPgUp}, func(b int) int { return -(b - 1) }},
		{"pgdown", tea.KeyMsg{Type: tea.KeyPgDown}, func(b int) int { return b - 1 }},
		{"ctrl+u", tea.KeyMsg{Type: tea.KeyCtrlU}, func(b int) int { return -(b / 2) }},
		{"ctrl+d", tea.KeyMsg{Type: tea.KeyCtrlD}, func(b int) int { return b / 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := overflowingConfirm(12, 80, 24)
			_, budget := c.scrollContent()
			for i := 0; i < 5; i++ {
				c.ScrollDown()
			}
			before := c.scroll

			closed := c.HandleKeyPress(tc.msg)
			assert.False(t, closed, "%s must not close the dialog", tc.name)
			assert.False(t, c.Dismissed, "%s must not dismiss the dialog", tc.name)

			want := before + tc.delta(budget)
			if max := len(c.bodyLines(c.textRect().W)) - budget; want > max {
				want = max
			}
			if want < 0 {
				want = 0
			}
			assert.Equal(t, want, c.scroll, "%s must move the body window", tc.name)
		})
	}
}

// TestConfirmationOverlay_ScrollKeysClamp: the window cannot scroll past
// either end — pasting the same key forever parks at the boundary rather than
// drifting or wrapping.
func TestConfirmationOverlay_ScrollKeysClamp(t *testing.T) {
	c := overflowingConfirm(12, 80, 24)
	_, budget := c.scrollContent()
	max := len(c.bodyLines(c.textRect().W)) - budget
	require.Positive(t, max, "the fixture must overflow")

	for i := 0; i < 100; i++ {
		c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	assert.Equal(t, max, c.scroll, "the window must stop at the last line")
	for i := 0; i < 100; i++ {
		c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyUp})
	}
	assert.Zero(t, c.scroll, "the window must stop at the first line")
}

// TestConfirmationOverlay_EscalatedKeyWinsOverScroll: on a dialog that
// escalated to 'k' (root #1238, unmerged #2022) 'k' must keep confirming —
// paging is what ↑ and j are for — and the scroll notice must not advertise a
// 'k' that would dispatch the action instead.
func TestConfirmationOverlay_EscalatedKeyWinsOverScroll(t *testing.T) {
	c := overflowingConfirm(12, 80, 24)
	c.SetConfirmKey("k")
	require.True(t, c.Scrollable())

	rendered := renderedText(c.Render())
	assert.Contains(t, rendered, "↑/↓ or j",
		"the notice must name only the keys that actually scroll")
	assert.NotContains(t, rendered, "or k",
		"the notice must never advertise 'k' as a scroll key on a 'k' confirm")

	closed := c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	assert.False(t, closed, "the unclaimed letter still scrolls")
	assert.Equal(t, 1, c.scroll)

	confirmed := false
	c.OnConfirm = func() { confirmed = true }
	closed = c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	assert.True(t, closed, "the escalated key must still confirm")
	assert.True(t, confirmed, "the claimed key confirms rather than scrolling")
}

// TestConfirmationOverlay_FittingBodyHasNoNotice: a body that fits renders
// exactly as it always has — every line, a blank gap, the prompt — with no
// scroll affordance and no offset for keys to move.
func TestConfirmationOverlay_FittingBodyHasNoNotice(t *testing.T) {
	c := NewConfirmationOverlay("Delete session 'alpha'?")
	c.SetWidth(50)
	c.SetMaxSize(80, 24)

	assert.False(t, c.Scrollable())
	rendered := renderedText(c.Render())
	assert.NotContains(t, rendered, "more line", "a fitting body must not advertise a scroll")
	assert.NotContains(t, rendered, "scroll")

	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyDown},
		{Type: tea.KeyRunes, Runes: []rune("j")},
		{Type: tea.KeyPgDown},
	} {
		assert.False(t, c.HandleKeyPress(msg))
	}
	assert.Zero(t, c.scroll, "scroll keys are inert when nothing overflows")
}

// TestConfirmationOverlay_GrowResizeCollapsesScroll: a dialog that outlives
// the resize must not reopen mid-scroll — when the window grows to fit the
// body the offset resets so the whole thing is on screen at once.
func TestConfirmationOverlay_GrowResizeCollapsesScroll(t *testing.T) {
	c := overflowingConfirm(12, 40, 10)
	for i := 0; i < 20; i++ {
		c.ScrollDown()
	}
	require.Positive(t, c.scroll, "the fixture must be scrolled")

	c.SetMaxSize(200, 60)
	rendered := renderedText(c.Render())
	assert.Zero(t, c.scroll, "a window that now fits must not render mid-scroll")
	assert.Contains(t, rendered, "[!] Delete session 'risky'?")
	assert.Contains(t, rendered, "Warning 12")
	assert.NotContains(t, rendered, "more lines", "a fitting body must not advertise a scroll")
}

// TestConfirmationOverlay_ScrollKeepsFrameHeight: while the body pages, the
// dialog's outer height must not breathe — the pinned prompt would otherwise
// walk the confirm buttons up and down the screen.
func TestConfirmationOverlay_ScrollKeepsFrameHeight(t *testing.T) {
	c := overflowingConfirm(12, 80, 24)
	want := renderedLineCount(c.Render())
	for i := 0; i < 30; i++ {
		c.ScrollDown()
		assert.Equal(t, want, renderedLineCount(c.Render()),
			"the frame must not change height at scroll %d", c.scroll)
	}
}
