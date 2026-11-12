package task

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Readiness for Gemini CLI 0.51.0's composer (#7168).
//
// Its composer is a half-block frame with no "╰", so af's #714 glyph check never
// saw it: in a folder Gemini already trusts, every start waited out the 60 s
// readiness budget and failed. A composer alone is not proof either — in an
// untrusted folder Gemini replaces its first-painted composer with the
// folder-trust dialog 80–156 ms later — so the property is: a Gemini pane is
// ready only when the frame proves Gemini is past its first render, which means
// the dialog decision is already on screen.
//
// The captures are real gemini-cli 0.51.0 frames (see
// session/tmux/gemini_trust_test.go for how they were taken): a pre-trusted
// folder with the defaults, with ui.hideBanner, with ui.hideFooter, and with
// both, each at its first composer paint and once settled.
func TestIsReadyContent_GeminiComposerNeedsALaterRenderAnchor(t *testing.T) {
	footerHidden := geminiTrustCapture(t, "gemini_0_51_0_composer_footer_hidden.ansi")
	bareFirstPaint := geminiTrustCapture(t, "gemini_0_51_0_composer_banner_and_footer_hidden_first_paint.ansi")
	for _, tt := range []struct {
		name    string
		content string
		ready   bool
	}{
		{"trusted folder: banner and branch column", geminiTrustCapture(t, "gemini_0_51_0_trusted_folder_composer.ansi"), true},
		{"banner hidden: the branch column alone anchors", geminiTrustCapture(t, "gemini_0_51_0_composer_banner_hidden.ansi"), true},
		{"footer hidden: the banner alone anchors", footerHidden, true},
		{"the relaunched composer after the trust answer (#7162)", geminiTrustCapture(t, "gemini_0_51_0_restarted_composer.ansi"), true},
		{"trusted folder, first paint: no anchor yet", geminiTrustCapture(t, "gemini_0_51_0_trusted_folder_first_paint.ansi"), false},
		{"untrusted folder, first paint: the dialog is about to replace it", geminiTrustCapture(t, "gemini_0_51_0_composer_first_paint.ansi"), false},
		{"banner and footer hidden: one frame cannot tell", geminiTrustCapture(t, "gemini_0_51_0_composer_banner_and_footer_hidden.ansi"), false},
		{"a banner above an older composer anchors nothing", strings.TrimRight(footerHidden, "\n") + "\n" + bareFirstPaint, false},
		{"the folder-trust dialog has replaced the composer", geminiTrustCapture(t, "gemini_0_51_0_folder_trust_below_tips.ansi"), true},
		// #7169 review, P2-2: on a short pane the relaunch after the trust
		// answer scrolls the restart notice — and the answered box above it —
		// away (50x24, measured). The relaunched composer is then proven by its
		// own later-render anchors, without the notice.
		{"the relaunched composer once the notice has scrolled away", geminiTrustCapture(t, "gemini_0_51_0_restarted_composer_50_columns_notice_gone.ansi"), true},
		// #7169 review, P3-2: an unrelated rounded box on screen (an update
		// notice, say) must not make a first paint ready through the "╰" check.
		{"a first paint with an unrelated rounded box above it", withRoundedBoxAbove(geminiTrustCapture(t, "gemini_0_51_0_composer_first_paint.ansi")), false},
		{"a trusted first paint with an unrelated rounded box above it", withRoundedBoxAbove(geminiTrustCapture(t, "gemini_0_51_0_trusted_folder_first_paint.ansi")), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReadyContent(tt.content, tmux.ProgramGemini); got != tt.ready {
				t.Fatalf("isReadyContent(gemini) = %t, want %t", got, tt.ready)
			}
		})
	}
}

// withRoundedBoxAbove paints an unrelated rounded box — the shape of an
// update-available notice — at the top of a capture.
func withRoundedBoxAbove(content string) string {
	box := " ╭──────────────────────────────────────╮\n" +
		" │ Gemini CLI update available!         │\n" +
		" ╰──────────────────────────────────────╯\n"
	return box + content
}
