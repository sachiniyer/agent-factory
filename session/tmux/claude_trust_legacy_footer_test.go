package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The legacy folder-trust dialog as Claude Code paints it at 80x24, one capture
// for each shape of it a supported build paints (#7584): 2.0.25 draws it in a
// round box with its footer below the box; 2.0.50 under a top rule, every row
// indented one column, its footer "Esc to exit"; 2.1.19 the same with "Esc to
// cancel". Each was taken with af's capture flags (-p -e -J) from a throwaway
// HOME and CLAUDE_CONFIG_DIR, an untrusted scratch folder and a private tmux
// server, with non-essential traffic off so the build showed its default
// "control" copy. No key was sent, so the folder was never trusted. The path is
// scrubbed to /home/u/proj, padded to keep every row's width.
var legacyDialogFixtures = []struct {
	name, footer string
}{
	{"claude_2_0_25_legacy_folder_trust_dialog.ansi", legacyTrustFooter},
	{"claude_2_0_50_legacy_folder_trust_dialog.ansi", legacyTrustFooter},
	{"claude_2_1_19_legacy_folder_trust_dialog.ansi", rewordedTrustFooter},
}

// legacyFooterSteps is a legacy dialog painted from the frame its option block
// is whole until its footer is: every row after the last option row, then the
// footer at every character after its indent. The whole footer is not a step.
func legacyFooterSteps(rows []string) [][]string {
	last := len(rows) - 1
	for last >= 0 && strings.TrimSpace(rows[last]) == "" {
		last--
	}
	no := -1
	for i, row := range rows[:max(0, last)] {
		if strings.Contains(row, "2. No") {
			no = i
		}
	}
	if no < 0 {
		return nil
	}
	var steps [][]string
	for end := no; end < last; end++ {
		steps = append(steps, rows[:end+1])
	}
	painted := strings.TrimRight(rows[last], " ")
	footer := []rune(painted)
	indent := len(footer) - len([]rune(strings.TrimLeft(painted, " ")))
	for k := indent + 1; k < len(footer); k++ {
		steps = append(steps, append(append([]string{}, rows[:last]...), string(footer[:k])))
	}
	return steps
}

// TestIssue7584RealLegacyDialogsEndWithTheirFooterAndAreAnswered pins what
// #7584's second part rests on: every build that paints the legacy dialog
// paints its footer as its last row, with nothing below it, and the gate
// answers the whole dialog with one Enter on Claude Code's preselected Yes.
func TestIssue7584RealLegacyDialogsEndWithTheirFooterAndAreAnswered(t *testing.T) {
	for _, fixture := range legacyDialogFixtures {
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
			require.Equal(t, claudeLegacyTrustAnswerable, claudeLegacyTrustDialogOf(screen),
				"the real dialog must read whole, its cursor on Yes")

			handled, cmds := runTrustPromptCheck(t, ProgramClaude, content)
			require.True(t, handled, "the real dialog is in the way until it is answered")
			require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
				"the whole legacy dialog keeps its Enter on Claude Code's preselected %q", claudeLegacyTrustProceedLabel)
		})
	}
}

// TestIssue7584LegacyDialogHoldsUntilItsFooterIsWhole pins #7584's second
// property: af answers the legacy dialog only once it is whole, footer
// included. Before it, the grammar took the footer for optional, so a frame
// with the whole option block and the footer not yet painted, or only begun,
// got its Enter before the dialog finished painting. Each frame from the whole
// option block until the footer is whole holds without a key: the real
// captures on a fresh pane, and the synthetic shapes under nothing, an earlier
// ruled composer, and that composer with its status row.
func TestIssue7584LegacyDialogHoldsUntilItsFooterIsWhole(t *testing.T) {
	for _, fixture := range legacyDialogFixtures {
		rows := visibleTextOf(readTrustFixture(t, fixture.name)).lines()
		steps := legacyFooterSteps(rows)
		require.NotEmpty(t, steps, "%s: no footer steps", fixture.name)
		for _, step := range steps {
			last := strings.TrimSpace(step[len(step)-1])
			requireHeldWithoutAKey(t, strings.Join(step, "\n"),
				fmt.Sprintf("the real %s, painted as far as %q, must hold until its footer is whole", fixture.name, last))
		}
	}
	for _, width := range []int{40, 80} {
		for shape, rows := range legacyPaintedDialogs(width) {
			if width < 80 && strings.HasPrefix(shape, "in a box") {
				continue // the box wraps the question under 80 columns; the rewrap reader holds it
			}
			if shape == "short labels, no body" {
				// No supported build paints "1. Yes" / "2. No" under this
				// question: 2.0.25 through 2.1.19 paint "Yes, proceed" and
				// "No, exit". The rewrap reader holds a footer's first
				// characters, before "Enter to confirm" is whole, under
				// those labels only, so under the short ones they read clear,
				// on master as here.
				continue
			}
			steps := legacyFooterSteps(rows)
			require.NotEmpty(t, steps, "%s at %d columns: no footer steps", shape, width)
			for setting, above := range legacyPaintSettings(width) {
				for _, step := range steps {
					last := strings.TrimSpace(step[len(step)-1])
					requireHeldWithoutAKey(t, above+strings.Join(step, "\n"),
						fmt.Sprintf("%s at %d columns under %s, painted as far as %q, must hold until its footer is whole",
							shape, width, setting, last))
				}
			}
		}
	}
}
