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
//   - 2.1.20 writes its footer as one string, as every build through 2.1.97
//     does;
//   - 2.1.113 builds it from key hints, as every build from 2.1.98 does;
//   - 2.1.200 writes those hints as JSX, as 2.1.186 through 2.1.241 do;
//   - 2.1.323, 2.1.327 and 2.1.328 put "No, exit" first, with the cursor on
//     it.
//
// 2.0.25 through 2.1.19 paint it only in one arm of the trust_folder_dialog_copy
// experiment, which the server hands out. The "reworded_arm" captures are
// those builds' own code with only the experiment's default arm switched from
// "control" to "variant_normalize_action", in a scratch copy. 2.0.25 draws the
// dialog in a round box with its footer below it, and 2.0.25 and 2.0.50 paint
// "Esc to exit".
//
// key is the key af answers each one with: Enter when Claude Code painted the
// cursor on "Yes, I trust this folder", and Down when it painted it on "No,
// exit".
var rewordedDialogFixtures = []struct {
	name, footer, key string
}{
	{"claude_2_0_25_reworded_arm_folder_trust_dialog.ansi", legacyTrustFooter, "Enter"},
	{"claude_2_0_50_reworded_arm_folder_trust_dialog.ansi", legacyTrustFooter, "Enter"},
	{"claude_2_1_19_reworded_arm_folder_trust_dialog.ansi", rewordedTrustFooter, "Enter"},
	{"claude_2_1_20_folder_trust_dialog.ansi", rewordedTrustFooter, "Enter"},
	{"claude_2_1_113_folder_trust_dialog.ansi", rewordedTrustFooter, "Enter"},
	{"claude_2_1_200_folder_trust_dialog.ansi", rewordedTrustFooter, "Enter"},
	{rewordedDialogNoLinkFixture, rewordedTrustFooter, "Down"},
	{rewordedDialogFixture, rewordedTrustFooter, "Down"},
	{rewordedDialogOnYesFixture, rewordedTrustFooter, "Enter"},
	{"claude_2_1_328_folder_trust_dialog.ansi", rewordedTrustFooter, "Down"},
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

			// The pane never changes, so a confirmed dialog never closes and
			// af confirms it again on each later poll. Every key it sends is
			// the one key, and it moves the cursor only once: it confirms
			// nothing it has not seen land.
			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled, "the real dialog is in the way until it is answered")
			keys := injectedKeyNames(sentKeystrokes(cmds))
			require.NotEmpty(t, keys, "the real dialog must be answered")
			for _, key := range keys {
				require.Equal(t, fixture.key, key,
					"af must move onto %q, or confirm it once the cursor is there; sent %v", claudeTrustYesLabel, keys)
			}
			if fixture.key == "Down" {
				require.Len(t, keys, 1, "af moves the cursor once and waits for the repaint; sent %v", keys)
			}
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

// TestIssue7619UnfinishedRewordedDialogIsHeldWhicheverClauseAlsoReadsIt pins
// the #7619 review's R1. A pane whose reworded dialog reads whole but for its
// footer row, begun, is held, even when another answering clause reads the
// same pane as its own dialog. The review's frame is the reworded question and
// body over the older MCP prompt's question, then "❯ No, exit", "Yes, I trust
// this folder", "Yes" and "No", and a footer begun. The older MCP clause takes
// any last row that starts "Enter to confirm" for its footer. So once the
// reworded dialog read partial, the gate fell through to that clause's unaimed
// Enter, which lands on the preselected "No, exit" and quits Claude Code.
// Master moved the cursor onto Yes there, or held.
//
// The hold is only for that partial, the one #7618 makes. A live legacy dialog
// below a composer whose draft quotes the reworded question also reads as a
// partial reworded region, and master answers it, rightly: that stays so, or
// the create would stall.
func TestIssue7619UnfinishedRewordedDialogIsHeldWhicheverClauseAlsoReadsIt(t *testing.T) {
	const (
		reworded = "Quick safety check: Is this a project you created or one you trust? (Like your own code, …).\n\n" +
			"Claude Code'll be able to read, edit, and execute files here.\n\n"
		mcp     = "New MCP server found in .mcp.json: probe\nDo you trust this new MCP server?\n"
		options = "❯ No, exit\n  Yes, I trust this folder\n  Yes\n  No\n"
	)
	for _, gap := range []string{"", "\n"} {
		for _, footer := range []string{"Enter to confirm", "Enter to confirm ·", "Enter to confirm · Esc", "Enter to confirm · Esc to"} {
			content := reworded + mcp + options + gap + footer
			screen := claudeScreenOf(content)
			require.True(t, claudeMCPTrustFooterIsLast(screen),
				"the older MCP clause must read the frame as its own, or this test proves nothing:\n%s", content)
			_, footerUnfinished := claudeFolderTrustRead(screen)
			require.True(t, footerUnfinished, "the reworded dialog must read whole but for its footer:\n%s", content)
			require.False(t, claudeTrustPromptPresent(screen),
				"no clause may answer a reworded dialog whose footer is not yet whole:\n%s", content)
			requireHeldWithoutAKey(t, content, fmt.Sprintf("the review's frame, footer painted as far as %q", footer))
		}
	}

	box := "╭────────────────────────────────────╮\n" +
		"│ ! ls                               │\n" +
		"│ Quick safety check: Is this a project you created or one you trust? │\n" +
		"╰────────────────────────────────────╯\n"
	legacy := "Do you trust the files in this folder?\n\n/home/u/proj\n\n" +
		"Claude Code may read files in this\nfolder. Reading untrusted files may\nlead Claude Code to behave in\nunexpected ways.\n\n" +
		"❯ 1. Yes, proceed\n  2. No, exit\n\n" + legacyTrustFooter
	content := box + legacy
	verdict, footerUnfinished := claudeFolderTrustRead(claudeScreenOf(content))
	require.Equal(t, claudeFolderTrustPartial, verdict, "the quote opens a reworded region the legacy rows break")
	require.False(t, footerUnfinished)
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, content)
	require.True(t, handled)
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
		"the live legacy dialog under the quoting composer keeps its Enter")
}
