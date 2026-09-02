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
	// "C" is POSIX-mandated and present everywhere; it is the arm #3605 is
	// actually about. The UTF-8 arm is the control, and its NAME is not
	// portable — glibc spells it "C.utf8", macOS ships "en_US.UTF-8" and has no
	// C.UTF-8 at all — so it is discovered rather than hardcoded.
	//
	// Discovering it is not pedantry. tmux does not reject an unknown locale
	// name (measured: `new-session` exits 0 under LC_ALL=xx_YY.UTF-8), so a
	// hardcoded C.UTF-8 would not fail on macOS — setlocale would quietly fall
	// back to C and this arm would run a SECOND non-UTF-8 pass while claiming to
	// be the UTF-8 control. A test that silently checks something weaker than it
	// says is worse than one that skips.
	locales := []string{"C"}
	if utf8Locale := availableUTF8Locale(); utf8Locale != "" {
		locales = append(locales, utf8Locale)
	} else {
		t.Log("no UTF-8 locale installed; running the C arm only, which is the one #3605 is about")
	}

	for _, locale := range locales {
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

// availableUTF8Locale returns a UTF-8 locale name this machine actually has,
// taken verbatim from `locale -a` so the spelling matches the platform's own
// (glibc "C.utf8" vs macOS "en_US.UTF-8"), or "" when there is none.
func availableUTF8Locale() string {
	out, err := exec.Command("locale", "-a").Output()
	if err != nil {
		return ""
	}
	var fallback string
	for _, name := range strings.Split(string(out), "\n") {
		name = strings.TrimSpace(name)
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".utf-8") && !strings.HasSuffix(lower, ".utf8") {
			continue
		}
		// Prefer a C-based UTF-8 locale: it differs from the "C" arm in exactly
		// the character encoding, which is the variable under test.
		if strings.HasPrefix(lower, "c.") {
			return name
		}
		if fallback == "" {
			fallback = name
		}
	}
	return fallback
}
