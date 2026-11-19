package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The reworded folder-trust dialog as each shape of Claude Code paints it at
// 80x24 (#7618). Every capture was taken with af's capture flags (-p -e -J)
// from a throwaway HOME and CLAUDE_CONFIG_DIR, an untrusted scratch folder and
// a private tmux server killed by its socket afterwards. No key was sent, so
// no folder was trusted. The path is scrubbed to /home/u/proj, padded to keep
// every row's width.
//
// From 2.1.20 the dialog is the reworded one, and these builds paint it as
// they ship:
//   - 2.1.20 writes its footer as one string;
//   - 2.1.113 builds it from key hints, as every build from 2.1.98 does;
//   - 2.1.200 is the next code shape, through 2.1.241;
//   - 2.1.323, 2.1.327 and 2.1.328 put "No, exit" first, as every build from
//     2.1.242 does.
//
// 2.0.25 through 2.1.19 paint it only in one arm of the trust_folder_dialog_copy
// experiment, which the server hands out. The "reworded_arm" captures are
// those builds' own code with only the experiment's default arm switched from
// "control" to "variant_normalize_action", in a scratch copy. 2.0.25 draws the
// dialog in a round box with its footer below it, and 2.0.25 and 2.0.50 paint
// "Esc to exit".
var rewordedDialogFixtures = []struct {
	name, footer string
	keys         []string
}{
	{"claude_2_0_25_reworded_arm_folder_trust_dialog.ansi", legacyTrustFooter, []string{"Enter"}},
	{"claude_2_0_50_reworded_arm_folder_trust_dialog.ansi", legacyTrustFooter, []string{"Enter"}},
	{"claude_2_1_19_reworded_arm_folder_trust_dialog.ansi", rewordedTrustFooter, []string{"Enter"}},
	{"claude_2_1_20_folder_trust_dialog.ansi", rewordedTrustFooter, []string{"Enter"}},
	{"claude_2_1_113_folder_trust_dialog.ansi", rewordedTrustFooter, []string{"Enter"}},
	{"claude_2_1_200_folder_trust_dialog.ansi", rewordedTrustFooter, []string{"Enter"}},
	{rewordedDialogNoLinkFixture, rewordedTrustFooter, []string{"Down"}},
	{rewordedDialogFixture, rewordedTrustFooter, []string{"Down"}},
	{rewordedDialogOnYesFixture, rewordedTrustFooter, []string{"Enter"}},
	{"claude_2_1_328_folder_trust_dialog.ansi", rewordedTrustFooter, []string{"Down"}},
}

// rewordedFooterSteps is a dialog painted from the frame its option block is
// whole until its footer is: every row after the last option row, then the
// footer row at every character, from its first one until its text is whole.
// The whole footer is not a step.
func rewordedFooterSteps(rows []string, footer string) [][]string {
	at := -1
	for i, row := range rows {
		if strings.Contains(row, footer) {
			at = i
		}
	}
	if at < 0 {
		return nil
	}
	option := -1
	for i, row := range rows[:at] {
		if strings.Contains(row, claudeTrustYesLabel) || strings.Contains(row, claudeTrustNoLabel) {
			option = i
		}
	}
	if option < 0 {
		return nil
	}
	var steps [][]string
	for end := option; end < at; end++ {
		steps = append(steps, rows[:end+1])
	}
	row := []rune(rows[at])
	start := len([]rune(rows[at][:strings.Index(rows[at], footer)]))
	for k := start + 1; k < start+len([]rune(footer)); k++ {
		steps = append(steps, append(append([]string{}, rows[:at]...), string(row[:k])))
	}
	return steps
}

// TestIssue7618RealRewordedDialogsEndWithTheirFooterAndAreAnswered pins what
// #7618 rests on: every shape of the reworded dialog a supported build paints
// ends with its whole footer, with nothing below it, and the gate answers it,
// moving the cursor onto "Yes, I trust this folder" or confirming it there.
func TestIssue7618RealRewordedDialogsEndWithTheirFooterAndAreAnswered(t *testing.T) {
	for _, fixture := range rewordedDialogFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			content := readTrustFixture(t, fixture.name)
			screen := claudeScreenOf(content)
			lines := screen.lines()
			last := len(lines) - 1
			for last >= 0 && strings.TrimSpace(lines[last]) == "" {
				last--
			}
			require.GreaterOrEqual(t, last, 0, "the capture is empty")
			require.Equal(t, fixture.footer, strings.TrimSpace(lines[last]),
				"the dialog's footer must be its last row, with nothing painted below it")
			require.False(t, claudeComposerRuleUnderDialog(screen),
				"no rule may sit under the real dialog, or #7464's hold would stall the create")
			require.Equal(t, claudeFolderTrustComplete, claudeFolderTrustDialogOf(screen),
				"the real dialog must read whole")

			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled, "the real dialog is in the way until it is answered")
			require.Equal(t, fixture.keys, injectedKeyNames(sentKeystrokes(cmds)),
				"af must move onto %q, or confirm it once the cursor is there", claudeTrustYesLabel)
		})
	}
}

// TestIssue7618RewordedDialogHoldsUntilItsFooterIsWhole pins #7618's property:
// af answers the reworded dialog only once its footer row is whole. Before it,
// a single footer row counted as whole whatever it held, so a frame whose
// footer was painted only as far as "Enter to confirm", or "Enter to confirm ·
// Esc to", got af's key before the dialog finished painting. Each frame from
// the whole option block until the footer is whole holds without a key: every
// real capture on a fresh pane, and the boxed dialog with its footer inside or
// below the box, under nothing, an earlier ruled composer, and that composer
// with its status row.
func TestIssue7618RewordedDialogHoldsUntilItsFooterIsWhole(t *testing.T) {
	for _, fixture := range rewordedDialogFixtures {
		rows := visibleTextOf(readTrustFixture(t, fixture.name)).lines()
		steps := rewordedFooterSteps(rows, fixture.footer)
		require.NotEmpty(t, steps, "%s: no footer steps", fixture.name)
		for _, step := range steps {
			last := strings.TrimSpace(step[len(step)-1])
			requireHeldWithoutAKey(t, strings.Join(step, "\n"),
				fmt.Sprintf("the captured %s, painted as far as %q, must hold until its footer is whole", fixture.name, last))
		}
	}
	for _, width := range []int{40, 80} {
		for _, variant := range []string{"inside", "outside"} {
			steps := rewordedFooterSteps(framedRewordedDialogRows(width, variant), rewordedTrustFooter)
			require.NotEmpty(t, steps, "the box, footer %s, at %d columns: no footer steps", variant, width)
			for setting, above := range legacyPaintSettings(width) {
				for _, step := range steps {
					last := strings.TrimSpace(step[len(step)-1])
					requireHeldWithoutAKey(t, above+strings.Join(step, "\n"),
						fmt.Sprintf("the boxed dialog, footer %s, at %d columns under %s, painted as far as %q, must hold until its footer is whole",
							variant, width, setting, last))
				}
			}
		}
	}
}
