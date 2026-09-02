package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmdpkg "github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestClaudeTrustCursorSurvivesRealCaptureUnderNonUTF8Locale is #3605.
//
// The concern was that a non-UTF-8 daemon locale (LANG=C / LC_ALL=C) could make
// tmux lose or substitute the ❯ cursor cell, so parseClaudeFolderTrustDialog
// would find no cursor and af would refuse a dialog it could have answered.
//
// Measured rather than assumed, which is what the issue asked for, and the
// premise did not hold: the glyph survives byte-identically (E2 9D AF) with the
// server under C, the client under C, either one mixed with UTF-8, and with
// `tmux -u`. tmux 3.4 has no `utf8` option at all — it was removed upstream, and
// `-u` is documented as affecting output written TO A TERMINAL, which a
// capture-pane pipe is not. `#{cursor_x}` is 1 after printing the glyph under
// LANG=C, so tmux stores it as one cell rather than three byte cells. Claude
// Code 2.1.258 itself was checked too — a real folder-trust dialog rendered
// under LANG=C draws the same ❯, so the agent does not fall back to ASCII
// either.
//
// So no second cursor signal is warranted; adding an SGR fallback would be
// unfounded complexity on the one path that decides whether af presses a key on
// a trust dialog. What WAS missing is any coverage at all of the glyph through a
// real capture: every other trust test parses a Go string literal, so nothing
// would notice if af's own capture path started mangling that cell. This is that
// test, and it runs under both locales so the non-UTF-8 case is pinned rather
// than argued.
//
// The fixture is the shape measured on 2.1.258 — "No, exit" carrying the cursor
// ABOVE the affirmative row, options then a blank then the footer, nothing after
// it — so a pass also proves af still steers Down toward the right option.
func TestClaudeTrustCursorSurvivesRealCaptureUnderNonUTF8Locale(t *testing.T) {
	for _, locale := range []string{"C.UTF-8", "C"} {
		t.Run(locale, func(t *testing.T) {
			// Before IsolateTmux's first tmux command, so the private server and
			// every client it serves inherit the locale under test.
			t.Setenv("LANG", locale)
			t.Setenv("LC_ALL", locale)
			testguard.IsolateTmux(t)

			name := toTmuxName(fmt.Sprintf("trust-locale-%d", len(locale)), "")
			script := fmt.Sprintf(
				"printf '%%s\\n' '' '%s No, exit' '  %s' '' '%s · Esc to exit'; sleep 300",
				claudeTrustSelectionGlyph, claudeTrustAffirmativeLabel, claudeTrustAffordancePrefix)
			out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "100", "-y", "30",
				"sh", "-c", script).CombinedOutput()
			require.NoError(t, err, "tmux new-session: %s", out)
			testguard.KeepTmuxServerOnEmpty(t)
			t.Cleanup(func() {
				_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
			})

			session := NewTmuxSessionFromSanitizedNameWithDeps(name, "claude", MakePtyFactory(), cmdpkg.MakeExecutor())
			var content string
			require.Eventually(t, func() bool {
				captured, capErr := session.CapturePaneContentContext(context.Background())
				if capErr != nil || !strings.Contains(captured, claudeTrustAffordancePrefix) {
					return false
				}
				content = captured
				return true
			}, 30*time.Second, 25*time.Millisecond, "the dialog fixture never rendered in the pane")

			// The #3605 assertion, stated on the bytes rather than on the parse:
			// af's own capture path returned the cursor cell intact.
			require.Contains(t, content, claudeTrustSelectionGlyph,
				"the %q selection cursor did not survive `capture-pane -p -e -J` under LANG=%s:\n%s",
				claudeTrustSelectionGlyph, locale, content)

			dialog, err := parseClaudeFolderTrustDialog(content)
			require.NoError(t, err,
				"af could not locate the cursor in a REAL capture under LANG=%s:\n%s", locale, content)
			require.Equal(t, "No, exit", dialog.selectedLabel(),
				"the cursor row was misread under LANG=%s:\n%s", locale, content)
			require.False(t, dialog.onAffirmative(),
				"the fixture preselects the exit row, which is what #3579 was about")
			require.Equal(t, "Down", dialog.keyTowardAffirmative(),
				"the affirmative row is below the cursor, so af must steer Down")
		})
	}
}
