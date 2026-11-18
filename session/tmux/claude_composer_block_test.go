package tmux

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7464. A composer drawn last explained the transcript
// above it only in some of its modes, and only to some clauses. Under a "!"
// (bash) or "#" (memory) prompt, quoted dialog text held where the ❯ twin
// cleared. And a draft that held a whole dialog was answered: af sent the
// dialog's keys into the composer, and its Enter submitted the draft (#7538's
// review, R2). Claude Code paints its launch dialogs in place of the
// composer, never above a live one, so a whole composer in any mode, with any
// draft, explains every row of its own draft to every clause.

// composerModePrompts are the composer's prompt rows in each of its modes, ❯
// (prompt), "!" (bash), "#" (memory) and ">", with U+00A0 after the glyph, as
// 2.1.322 paints it, and with U+0020.
var composerModePrompts = []string{
	claudeTrustSelectionGlyph + "\u00a0", claudeTrustSelectionGlyph + " ",
	"!\u00a0", "! ", "#\u00a0", "# ", ">\u00a0", "> ",
}

// drawnComposer is a whole composer width columns wide in the mode prompt
// leads with, holding draft: ruled, as 2.1.322 draws it, or boxed, as older
// builds do. The draft's first line is on the prompt row, and the rest are
// indented under it.
func drawnComposer(width int, prompt string, draft []string, boxed bool) []string {
	lines := []string{prompt}
	if len(draft) > 0 {
		lines[0] += draft[0]
		for _, row := range draft[1:] {
			if row == "" {
				lines = append(lines, "")
				continue
			}
			lines = append(lines, "  "+row)
		}
	}
	if !boxed {
		rule := strings.Repeat("─", width)
		return append(append([]string{rule}, lines...), rule)
	}
	out := []string{"╭" + strings.Repeat("─", width-2) + "╮"}
	for _, line := range lines {
		out = append(out, "│ "+line+strings.Repeat(" ", max(0, width-4-utf8.RuneCountInString(line)))+" │")
	}
	return append(out, "╰"+strings.Repeat("─", width-2)+"╯")
}

// composerDrafts are what a composer can hold: nothing, a command, a two-line
// draft, and a draft that quotes a dialog's options.
var composerDrafts = map[string][]string{
	"empty":                   nil,
	"a command":               {"ls"},
	"a two-line draft":        {"fix it", "please"},
	"a draft quoting options": {"why does it say:", "1. Yes", "2. No"},
}

// liveDialogs are the dialogs the gate reads, as plain rows 40 columns wide:
// the reworded folder-trust dialog as 2.1.322 paints it, under its own top
// rule, and boxed, as older builds do; the legacy one; the old MCP modal; and
// the 2.1.322 MCP-server dialog and multi-server picker, as captured.
func liveDialogs(t *testing.T) map[string][]string {
	t.Helper()
	capture := func(name string) []string {
		rows := strings.Split(strings.TrimRight(string(visibleTextOf(readTrustFixture(t, name))), "\n "), "\n")
		for len(rows) > 0 && strings.TrimSpace(rows[0]) == "" {
			rows = rows[1:]
		}
		return rows
	}
	return map[string][]string{
		"the reworded folder-trust dialog": ruleTopDialog(40, rewordedDialog),
		"the reworded dialog, boxed":       framedRewordedDialogRows(40, "outside"),
		"the legacy folder-trust dialog":   legacyTrustDialogRows(40, true),
		"the old MCP modal":                oldMCPModalRows(40, true),
		"the 2.1.322 MCP-server dialog":    capture(mcpServerDialogFixture),
		"the 2.1.322 multi-server picker":  capture(mcpServersThreeFixture),
	}
}

// requireClearWithoutAKey asserts the pane reads clear and af sends it nothing.
func requireClearWithoutAKey(t *testing.T, content, why string) {
	t.Helper()
	handled, cmds := pollStaticPane(t, content, 4)
	require.False(t, handled, "%s:\n%s", why, content)
	require.Empty(t, sentKeystrokes(cmds), "%s:\n%s", why, content)
}

// The issue's frame, the old MCP modal quoted over a "!" composer drawn last,
// in every mode. Then each dialog the gate reads, quoted whole into the
// transcript, over a composer in each mode drawn last, ruled or boxed, empty
// or holding a draft, with and without its status row below: every one reads
// clear, as its ❯ twin does.
func TestCheckAndHandleTrustPrompt_Issue7464ComposerInAnyModeDrawnLastExplainsTheQuoteAboveIt(t *testing.T) {
	rule := strings.Repeat("─", 40)
	issue := "New MCP server found.\nDo you trust this new MCP server?\n\n❯ 1. Yes\n  2. No\n\nEnter to confirm\n\n"
	for _, prompt := range composerModePrompts {
		requireClearWithoutAKey(t, issue+rule+"\n"+prompt+"ls\n"+rule+"\n  ? for shortcuts", fmt.Sprintf("the issue's frame under %q (#7464)", prompt))
	}
	for name, dialog := range liveDialogs(t) {
		quote := append([]string{"⏺ The dialog reads:", ""}, dialog...)
		for _, prompt := range composerModePrompts {
			for draftName, draft := range composerDrafts {
				for _, boxed := range []bool{false, true} {
					for _, status := range [][]string{nil, {"  ? for shortcuts"}} {
						rows := append(append(append(append([]string{}, quote...), ""), drawnComposer(44, prompt, draft, boxed)...), status...)
						requireClearWithoutAKey(t, strings.Join(rows, "\n"),
							fmt.Sprintf("%s quoted over a composer in %q holding %s (boxed %v, status %v)", name, prompt, draftName, boxed, status != nil))
					}
				}
			}
		}
	}
}

// A composer drawn last explains every row of its own draft, for every clause,
// answering clauses included: a dialog held in a draft, whole or cut at any of
// its rows, is neither held nor answered. On master the whole dialog was
// answered, its keys typed into the draft.
func TestCheckAndHandleTrustPrompt_Issue7464DialogInADraftIsNeitherHeldNorAnswered(t *testing.T) {
	for name, dialog := range liveDialogs(t) {
		for _, prompt := range composerModePrompts {
			for _, lead := range [][]string{{"why does it say this:"}, nil} {
				for _, boxed := range []bool{false, true} {
					for _, status := range [][]string{nil, {"  ? for shortcuts"}} {
						cuts := []int{len(dialog)}
						if prompt == "!\u00a0" && lead != nil {
							// Every row of it, under one mode, since a cut changes
							// which clause reads the rest.
							cuts = cuts[:0]
							for k := 1; k <= len(dialog); k++ {
								cuts = append(cuts, k)
							}
						}
						for _, k := range cuts {
							draft := append(append([]string{}, lead...), dialog[:k]...)
							rows := append(append([]string{"⏺ Done.", ""}, drawnComposer(48, prompt, draft, boxed)...), status...)
							requireClearWithoutAKey(t, strings.Join(rows, "\n"),
								fmt.Sprintf("%s, %d of its rows, in a draft under %q (lead %v, boxed %v, status %v)", name, k, prompt, lead != nil, boxed, status != nil))
						}
					}
				}
			}
		}
	}
}

// A ruled composer's draft on a pane too short to show its prompt row: the
// dialog it holds, over the composer's closing rule. No dialog the gate reads
// paints a plain rule under its footer, so af holds it and sends nothing; on
// master the dialog was answered. A boxed composer's bottom edge is a boxed
// dialog's too, so a boxed draft cut the same way is not read here.
func TestCheckAndHandleTrustPrompt_Issue7464DialogInARuledDraftCutAboveItsPromptIsNotAnswered(t *testing.T) {
	for name, dialog := range liveDialogs(t) {
		for _, prompt := range composerModePrompts {
			rows := drawnComposer(48, prompt, append([]string{"why does it say this:"}, dialog...), false)
			for top := 1; top < len(rows)-1; top++ {
				content := strings.Join(rows[top:], "\n")
				_, cmds := pollStaticPane(t, content, 4)
				require.Empty(t, sentKeystrokes(cmds), "%s in a draft under %q, cut above row %d: an Enter would submit the draft:\n%s", name, prompt, top, content)
			}
		}
	}
}

// What a composer explains is its own draft, and nothing else. A live dialog
// painted part way, from its head down, under a composer in any mode, ruled or
// boxed, empty or holding a draft quoting a dialog's options, is read as it is
// with nothing above it: wherever the dialog alone is in the way, the pane is
// not clear, and the composer never adds a key. And a row under a composer in
// any mode that can begin a dialog holds, with no key.
func TestCheckAndHandleTrustPrompt_Issue7464ComposerInAnyModeAboveALiveDialogExplainsNoneOfIt(t *testing.T) {
	drafts := map[string][]string{"empty": nil, "a draft quoting options": composerDrafts["a draft quoting options"]}
	for name, dialog := range liveDialogs(t) {
		head := 0
		for head < len(dialog) && (strings.TrimSpace(dialog[head]) == "" || claudeTrustFrameRule(dialog[head])) {
			head++
		}
		for k := head + 1; k <= len(dialog); k++ {
			aloneHandled, aloneCmds := pollStaticPane(t, strings.Join(dialog[:k], "\n"), 4)
			if !aloneHandled {
				continue
			}
			for _, prompt := range composerModePrompts {
				for draftName, draft := range drafts {
					for _, boxed := range []bool{false, true} {
						rows := append(drawnComposer(44, prompt, draft, boxed), dialog[:k]...)
						content := strings.Join(rows, "\n")
						handled, cmds := pollStaticPane(t, content, 4)
						why := fmt.Sprintf("%s painted to row %d under a composer in %q holding %s (boxed %v)", name, k, prompt, draftName, boxed)
						require.True(t, handled, "%s: a live dialog is in the way:\n%s", why, content)
						if len(sentKeystrokes(aloneCmds)) == 0 {
							require.Empty(t, sentKeystrokes(cmds), "%s: the composer added a key:\n%s", why, content)
						}
					}
				}
			}
		}
	}
	for _, prompt := range composerModePrompts {
		for draftName, draft := range composerDrafts {
			for _, boxed := range []bool{false, true} {
				for _, below := range [][]string{{"  New"}, {"  new MCP"}, {"", "  new"}} {
					rows := append(drawnComposer(44, prompt, draft, boxed), below...)
					requireHeldWithoutAKey(t, strings.Join(rows, "\n"),
						fmt.Sprintf("%q under a composer in %q holding %s (boxed %v)", below, prompt, draftName, boxed))
				}
			}
		}
	}
}
