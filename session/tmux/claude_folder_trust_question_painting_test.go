package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sentencePaintSteps paints rows top-down from the reworded dialog's sentence
// until its question is whole: every character of each row the sentence
// spans, after the row's leading chrome. The rows above the sentence are
// painted whole. The step that makes the question whole is left out; from
// there claudeFolderTrustDialogOf holds the dialog.
func sentencePaintSteps(rows []string) []string {
	question := claudeSquash(claudeFolderTrustQuestion)
	first := -1
	for i, row := range rows {
		if strings.Contains(row, "Quick safety check:") {
			first = i
			break
		}
	}
	var frames []string
	for k := first; k >= 0 && k < len(rows); k++ {
		row := []rune(strings.TrimRight(rows[k], " "))
		lead := len(row) - len([]rune(strings.TrimLeft(string(row), " │")))
		for c := lead + 1; c <= len(row); c++ {
			painted := append(append([]string(nil), rows[:k]...), string(row[:c]))
			if strings.Contains(claudeSquash(strings.Join(painted[first:], "")), question) {
				return frames
			}
			frames = append(frames, strings.Join(painted, "\n"))
		}
	}
	return frames
}

// lastRows is the pane a dialog painted into a pane h rows tall shows: its
// last h rows, the rows above scrolled off.
func lastRows(frame string, h int) string {
	rows := strings.Split(frame, "\n")
	return strings.Join(rows[max(0, len(rows)-h):], "\n")
}

// TestIssue7585QuestionPaintedWithNoTitleHolds pins #7585's property: the
// reworded dialog painted from the top down holds from its first row of text,
// with or without its title. Every build captured paints the title (testdata,
// 2.1.318 to 2.1.327), and while it is on screen claudeFolderTrustPaintingOf
// holds the paint. A pane too short for the dialog scrolls the title off
// before the question is whole. Then, before this, the pane read clear from the
// sentence's first letter until the question was whole, and the create typed
// its prompt and pressed Enter into the dialog.
func TestIssue7585QuestionPaintedWithNoTitleHolds(t *testing.T) {
	captured := visibleTextOf(readTrustFixture(t, rewordedDialogFixture)).lines()
	for _, h := range []int{3, 4} {
		for i, frame := range sentencePaintSteps(captured) {
			pane := lastRows(frame, h)
			require.NotContains(t, pane, claudeFolderTrustTitle, "the title must be scrolled off")
			requireHeldWithoutAKey(t, pane, fmt.Sprintf(
				"the real 2.1.327 dialog in a pane %d rows tall, sentence step %d, must hold without a key", h, i))
		}
	}

	for _, width := range []int{20, 40, 80} {
		rule := strings.Repeat("─", width)
		for setting, above := range map[string]string{
			"nothing above":                      "",
			"an earlier ruled composer":          rule + "\n❯ \n" + rule + "\n",
			"an earlier composer and its status": rule + "\n❯ \n" + rule + "\n  ? for shortcuts\n",
		} {
			for _, variant := range []string{"outside", "inside"} {
				for i, frame := range sentencePaintSteps(framedRewordedDialogRows(width, variant)) {
					requireHeldWithoutAKey(t, above+frame, fmt.Sprintf(
						"the boxed dialog with no title (footer %s) at %d columns under %s, sentence step %d, must hold without a key",
						variant, width, setting, i))
				}
			}
		}
	}
}

// TestIssue7585SettledScreensAreLeftAlone: the reader holds only a sentence
// begun with nothing below it, so the screens a create meets once a dialog is
// gone, or that a working agent shows, read as before.
//   - The real reworded dialog (testdata, #7575) is still answered.
//   - The idle composer Claude Code shows once its dialog is answered reads
//     clear.
//   - Each step of the sentence's paint, quoted above a composer drawn last,
//     reads clear and gets no key: a working agent frames its composer with
//     rules, so the reader declines a sentence with one below it.
func TestIssue7585SettledScreensAreLeftAlone(t *testing.T) {
	for _, name := range []string{rewordedDialogFixture, rewordedDialogNoLinkFixture} {
		handled, cmds := pollStaticPane(t, readTrustFixture(t, name), 4)
		require.True(t, handled, "the real reworded dialog is in the way: %s", name)
		require.Equal(t, []string{"Down"}, injectedKeyNames(sentKeystrokes(cmds)),
			"the real reworded dialog is still answered: %s", name)
	}

	for _, name := range []string{mcpServerComposerFixture, mcpServersRejectedFixture} {
		requireClearWithoutAKey(t, readTrustFixture(t, name), "the idle composer after a dialog: "+name)
	}

	captured := visibleTextOf(readTrustFixture(t, rewordedDialogFixture)).lines()
	for _, width := range []int{20, 40, 80} {
		rule := strings.Repeat("─", width)
		composers := map[string]string{
			"a ruled composer and its status": rule + "\n❯ \n" + rule + "\n  ? for shortcuts",
			"a ruled composer with a draft":   rule + "\n❯ explain the dialog above\n" + rule,
			"a boxed composer": "╭" + strings.Repeat("─", width-2) + "╮\n│ > " +
				strings.Repeat(" ", max(0, width-6)) + " │\n╰" + strings.Repeat("─", width-2) + "╯",
		}
		frames := sentencePaintSteps(framedRewordedDialogRows(width, "outside"))
		if width == 80 {
			frames = append(frames, sentencePaintSteps(captured)...)
		}
		for name, composer := range composers {
			for i, frame := range frames {
				requireClearWithoutAKey(t, frame+"\n\n"+composer, fmt.Sprintf(
					"the reworded dialog's sentence at %d columns, step %d, quoted above %s", width, i, name))
			}
		}
	}
}
