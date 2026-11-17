package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7452. Another dialog's footer above a live dialog's
// options explains nothing about the dialog below it. Claude Code erases an
// answered dialog before it draws its next screen, but a relaunch in the same
// pane (an account swap, a respawn) paints the new Claude under the old
// process's last frame. The re-wrap tail read such a footer as "no dialog", so
// the pane read clear, and af typed its prompt and pressed Enter into the live
// dialog: Enter confirms the preselected "Yes, I trust this folder". A footer
// above the options is now a row af cannot place, like any other, so the pane
// holds.

// The two frames #7452 filed: an answered dialog's footer, then a dialog in
// wording af does not know, plain at 40 columns, and with a blank row and that
// dialog's own 2.1.322 top rule.
func TestCheckAndHandleTrustPrompt_Issue7452FooterAboveALiveDialogHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"frame 1: an answered dialog's footer above a dialog in wording af does not know",
			"Enter to confirm · Esc to cancel\nBefore you start: do you want Claude to\nwork in this folder?\n\n❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel"},
		{"frame 2: the same with a blank row and the live dialog's own top rule",
			"Enter to confirm · Esc to cancel\n\n────────────────────────────────────────\nOpening a workspace:\n\n/home/u/proj\n\nBefore you start: do you want Claude to\nwork in this folder?\n\nSecurity guide\n\n❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel"},
	} {
		t.Run(tt.name, func(t *testing.T) { requireHeldWithoutAKey(t, tt.content, tt.name) })
	}
}

// newWordingQuestionDialog is a folder-trust dialog in wording af does not
// know, painted as only its question over its options.
var newWordingQuestionDialog = []string{
	"Before you start: do you want Claude to work in this folder?", "",
	"❯ 1. Yes, I trust this folder", "  2. No, exit", "",
	"Enter to confirm · Esc to cancel",
}

// staleDialogRows is what another dialog leaves on screen above a live one,
// wrapped at width: its footer alone, its last three rows, or all of it.
func staleDialogRows(dialog, keep string, width int) []string {
	var rows []string
	switch dialog {
	case "reworded":
		rows = indentedRows(width, rewordedDialog...)
	case "legacy":
		rows = legacyTrustDialogRows(width, true)
	case "mcp":
		rows = mcpServerDialogRows(width, 2)[1:]
	}
	switch keep {
	case "its footer":
		footer := rewordedTrustFooter
		if dialog == "legacy" {
			footer = legacyTrustFooter
		}
		return wrapAtSpaces(footer, width)
	case "its last three rows":
		return rows[len(rows)-3:]
	}
	return rows
}

// liveDialogRows is dialog wrapped at width in style: plain, under its own top
// rule, or boxed with its footer inside the box or under it.
func liveDialogRows(dialog []string, style string, width int) []string {
	switch style {
	case "under its own top rule":
		return ruleTopDialog(width, dialog)
	case "boxed":
		return styledBox("round", width, dialog...)
	case "boxed, its footer under the box":
		return append(styledBox("round", width, dialog[:len(dialog)-2]...), wrapAtSpaces(dialog[len(dialog)-1], width)...)
	}
	return indentedRows(width, dialog...)
}

// A dialog in wording af does not know, under another dialog's footer, holds at
// every width and in every style. Before #7452 it read clear at every width
// from 16 columns up, where the old footer's first row still reads as a
// footer. The footer is alone, among the old dialog's last three rows, or under
// the whole old dialog: today's reworded folder dialog, the legacy one, or the
// 2.1.322 MCP server dialog.
func TestCheckAndHandleTrustPrompt_DialogInWordingAfCannotReadUnderAnotherDialogsFooterHolds(t *testing.T) {
	lives := map[string][]string{"a dialog in wording af does not know": newWordingDialog, "its question alone": newWordingQuestionDialog}
	for liveName, live := range lives {
		for _, style := range []string{"plain", "under its own top rule", "boxed", "boxed, its footer under the box"} {
			for _, old := range []string{"reworded", "legacy", "mcp"} {
				for _, keep := range []string{"its footer", "its last three rows", "all of it"} {
					t.Run(fmt.Sprintf("%s, %s, under the %s dialog's %s", liveName, style, old, keep), func(t *testing.T) {
						for _, width := range []int{12, 16, 17, 20, 24, 30, 40, 60, 80, 120} {
							for _, gap := range []int{0, 1} {
								rows := append(staleDialogRows(old, keep, width), make([]string, gap)...)
								rows = append(rows, liveDialogRows(live, style, width)...)
								requireHeldWithoutAKey(t, strings.Join(rows, "\n"), fmt.Sprintf("at %d columns, %d blank rows between", width, gap))
							}
						}
					})
				}
			}
		}
	}
}

// quotedDialogText is dialog text Claude quotes in its transcript: a dialog's
// footer above that dialog's bare options, the whole of a dialog above them, or
// the footer in a sentence.
var quotedDialogText = map[string][]string{
	"a footer above bare options": {
		rewordedTrustFooter, "", "❯ 1. Yes, I trust this folder", "  2. No, exit", "", rewordedTrustFooter,
	},
	"a legacy footer above bare options": {
		legacyTrustFooter, "", "❯ 1. Yes, proceed", "  2. No, exit", "", legacyTrustFooter,
	},
	"a whole dialog above bare options": append(append([]string{}, rewordedDialog...),
		"", "❯ 1. Yes, I trust this folder", "  2. No, exit", "", rewordedTrustFooter),
	"the footer in a sentence above bare options": {
		rewordedTrustFooter + " is the footer it paints.", "", "❯ 1. Yes, I trust this folder", "  2. No, exit", "", rewordedTrustFooter,
	},
}

// quotedInTranscript wraps quote at width as Claude's transcript paints it:
// flush, two columns in, or behind its ⏺ bullet, which only the quote's first
// row carries. Both prefixes are two columns wide.
func quotedInTranscript(quote []string, how string, width int) []string {
	prefixes := map[string][2]string{"flush": {"", ""}, "indented": {"  ", "  "}, "behind its bullet": {"⏺ ", "  "}}[how]
	var out []string
	for _, row := range quote {
		if row == "" {
			out = append(out, "")
			continue
		}
		indent := row[:len(row)-len(strings.TrimLeft(row, " "))]
		for _, piece := range wrapAtSpaces(row, max(1, width-len([]rune(prefixes[1]))-len(indent))) {
			prefix := prefixes[1]
			if len(out) == 0 {
				prefix = prefixes[0]
			}
			out = append(out, prefix+indent+piece)
		}
	}
	return out
}

// A real 2.1.322 screen whose transcript quotes a dialog's footer above its
// bare options stays clear: the composer Claude paints under the quote is what
// says no dialog is live, and #7452 changes nothing below one. Cut at the
// quote's own footer, before Claude paints its composer, the same transcript
// holds, as it holds with no footer quoted above the options at all: the pane
// then ends with a dialog's options and its footer, and a footer above them
// says nothing about them.
func TestCheckAndHandleTrustPrompt_QuotedFooterAboveQuotedOptionsInARealTranscript(t *testing.T) {
	screen := strings.Split(strings.TrimRight(readTrustFixture(t, mcpServerComposerFixture), "\n"), "\n")
	status := -1
	for i, line := range screen {
		if strings.Contains(line, "/effort") {
			status = i
			break
		}
	}
	require.Positive(t, status, "the capture's status row, above its composer")
	for name, quote := range quotedDialogText {
		for _, how := range []string{"flush", "indented", "behind its bullet"} {
			t.Run(name+", "+how, func(t *testing.T) {
				for _, width := range []int{24, 40, 80} {
					quoted := append(append([]string{""}, quotedInTranscript(quote, how, width)...), "")
					rows := append(append(append([]string{}, screen[:status]...), quoted...), screen[status:]...)
					at := fmt.Sprintf("at %d columns", width)

					handled, cmds := pollStaticPane(t, strings.Join(rows, "\n"), 4)
					require.False(t, handled, "%s, under Claude's composer:\n%s", at, strings.Join(rows, "\n"))
					require.Empty(t, sentKeystrokes(cmds))

					painting := rows[:status+len(quoted)-1]
					requireHeldWithoutAKey(t, strings.Join(painting, "\n"), at+", cut at the quote's footer")
					unquoted := append([]string{}, painting...)
					for i := status; i < len(unquoted)-2; i++ {
						unquoted[i] = strings.Replace(unquoted[i], "Enter to confirm", "Notes to confirm", 1)
					}
					requireHeldWithoutAKey(t, strings.Join(unquoted, "\n"), at+", cut at the quote's footer, with no footer above the options")
				}
			})
		}
	}
}
