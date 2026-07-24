package overlay

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
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
	assert.Contains(t, overlayProse(def.Render()), "y/enter to confirm",
		"an ordinary dialog must advertise enter as a confirm alias")

	esc := NewConfirmationOverlay("[!] Kill session 'root'?")
	esc.SetConfirmKey("k")
	esc.SetWidth(50)
	esc.SetMaxSize(80, 24)
	rendered := overlayProse(esc.Render())
	assert.Contains(t, rendered, "k to confirm",
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
	return strings.Join(strings.Fields(frame.Replace(xansi.Strip(rendered))), " ")
}

// TestConfirmationOverlay_GuardedMessageIsNeverClipped: a guarded overlay (one
// with a detail set) must render its message in full. The message carries the
// consequences the user is consenting to; windowOverlayBody drops the TAIL, so
// without this guarantee the last consequence silently vanishes and the user
// confirms something the dialog never showed them (#1973).
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

// TestConfirmationOverlay_ClippedDetailIsAnnounced: when the elaboration does
// not fit, the overlay must SAY so. A bare "…" (or nothing at all) is
// indistinguishable from "there was nothing more to say".
func TestConfirmationOverlay_ClippedDetailIsAnnounced(t *testing.T) {
	c := NewConfirmationOverlay("[!] Delete project 'acme'?\n1 in-place session torn down — not restorable.")
	c.SetDetail("Line one of elaboration that will not fit. Line two of elaboration. Line three of elaboration. Line four of elaboration that keeps going for a while.")
	c.SetWidth(50)
	c.SetMaxSize(40, 10)

	rendered := c.Render()
	assert.Contains(t, rendered, "resize to read",
		"clipped detail must name itself; silence reads as completeness")
	assert.Regexp(t, `more line`, rendered, "the notice must say how much is hidden")
}

// TestConfirmationOverlay_TooSmallRefusesConfirm: a destructive confirm that
// cannot render its consequences has no business collecting a 'y'. The refusal
// must be real — the key handler rejects the confirm, not just the renderer
// showing a warning — otherwise a blind 'y' still fires the action (#1973).
// The trigger is realistic rather than contrived: at the declared 40x10 floor a
// long project name wraps the title onto a second line, which pushes the split
// past the four-line body budget. That is the backstop the guarantee needs —
// the copy is tuned to fit typical names, and refuses rather than clips when it
// cannot.
func TestConfirmationOverlay_TooSmallRefusesConfirm(t *testing.T) {
	c := NewConfirmationOverlay("[!] Delete project 'a-project-with-a-very-long-name-indeed'?\n3 in-place sessions torn down — not restorable.\n7 sessions archived — restorable.")
	c.SetDetail("Elaboration that does not matter here.")
	c.SetWidth(50)
	c.SetMaxSize(40, 10)

	confirmed := false
	c.OnConfirm = func() { confirmed = true }
	cancelled := false
	c.OnCancel = func() { cancelled = true }

	rendered := overlayProse(c.Render())
	assert.Contains(t, rendered, "Too small to confirm safely",
		"an overlay that cannot show the consequences must say why")
	assert.NotContains(t, rendered, "7 sessions archived",
		"a refused dialog must not show the reassuring half either — that is the trap")

	shouldClose := c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	assert.False(t, shouldClose, "the dialog must stay open so the user can resize and read it")
	assert.False(t, confirmed, "a 'y' typed blind against an unreadable dialog must NOT confirm")
	assert.False(t, c.Dismissed)

	// Esc must always work — the user is never trapped.
	shouldClose = c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	assert.True(t, shouldClose, "esc must still cancel a refused dialog")
	assert.True(t, cancelled)
}

// TestConfirmationOverlay_RefusalSurvivesDegenerateSizes: the refusal must say
// something true even when the window is far too small for its own explanation.
// Windowing it would degrade the refusal into a bare "… N more lines" notice —
// swallowing the reason at exactly the moment the reason is the whole point,
// which is the same defect one level up.
func TestConfirmationOverlay_RefusalSurvivesDegenerateSizes(t *testing.T) {
	for _, size := range [][2]int{{30, 6}, {40, 7}, {24, 5}} {
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

// TestConfirmationOverlay_UnguardedKeepsConfirming: overlays with no detail (the
// existing archive/kill confirms) keep their historical behavior — they clip
// rather than refuse. The guarantee is opt-in via SetDetail, so this fix does
// not silently make every confirm in the app refusable.
func TestConfirmationOverlay_UnguardedKeepsConfirming(t *testing.T) {
	c := NewConfirmationOverlay(strings.Repeat("a long confirmation message that will certainly not fit. ", 8))
	c.SetWidth(50)
	c.SetMaxSize(30, 6)

	confirmed := false
	c.OnConfirm = func() { confirmed = true }

	assert.NotContains(t, c.Render(), "Too small to confirm safely",
		"an unguarded overlay must not start refusing")
	assert.True(t, c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}))
	assert.True(t, confirmed, "unguarded confirms keep working exactly as before")
}
