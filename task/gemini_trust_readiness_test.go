package task

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// geminiTrustCapture reads one of the real gemini-cli 0.51.0 captures that
// session/tmux's folder-trust tests use, so readiness and the dismissal are
// held against the same bytes (#7162).
func geminiTrustCapture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "session", "tmux", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Readiness and Gemini's folder-trust dialog (#7162).
//
// The dialog is a readiness STOP: isReadyContent names it, CheckAndHandleTrustPrompt
// answers it, and only then is a prompt sent. Its own box is NOT a ready signal:
// the "╰" glyph check used to read the dialog — and the dialog with Gemini's
// restart notice below it — as a ready pane, so the prompt went into the dialog
// or into the process that was exiting. What IS ready after the answer is the
// relaunched composer, painted below that notice: it has the folder's trust
// recorded and cannot open the dialog again.
//
// The composer's first paint stays NOT ready. Gemini opens the dialog in an
// effect after that paint (80–156 ms later, measured), so a composer on its own
// says nothing about whether the dialog is coming.
func TestIsReadyContent_GeminiFolderTrustIsAStopAndItsBoxIsNotReady(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		ready   bool
	}{
		{"the dialog, first paint", geminiTrustCapture(t, "gemini_0_51_0_folder_trust.ansi"), true},
		{"the dialog below the tips", geminiTrustCapture(t, "gemini_0_51_0_folder_trust_below_tips.ansi"), true},
		{"the dialog at 160 columns", geminiTrustCapture(t, "gemini_0_51_0_folder_trust_wide.ansi"), true},
		{"the dialog with its body cut to fit", geminiTrustCapture(t, "gemini_0_51_0_folder_trust_discovery.ansi"), true},
		{"the dialog with the cursor on Trust parent folder", geminiTrustCapture(t, "gemini_0_51_0_folder_trust_cursor_on_parent.ansi"), true},
		{"the dialog relaunching after the answer", geminiTrustCapture(t, "gemini_0_51_0_trust_restarting.ansi"), false},
		{"the relaunched composer below the answered dialog", geminiTrustCapture(t, "gemini_0_51_0_restarted_composer_below_dialog.ansi"), true},
		{"the relaunched composer, the dialog scrolled away", geminiTrustCapture(t, "gemini_0_51_0_restarted_composer.ansi"), true},
		{"the composer's first paint, before the dialog", geminiTrustCapture(t, "gemini_0_51_0_composer_first_paint.ansi"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReadyContent(tt.content, tmux.ProgramGemini); got != tt.ready {
				t.Fatalf("isReadyContent(gemini) = %t, want %t", got, tt.ready)
			}
		})
	}
}
