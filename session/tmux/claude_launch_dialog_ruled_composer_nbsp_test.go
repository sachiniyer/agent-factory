package tmux

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7454. Claude Code 2.1.322 paints U+00A0 after its
// composer's mode glyph. The box test read it since #7412; the plain-rule test
// read only an ASCII space. So a ruled composer in bash, memory or ">" mode, in
// 2.1.322's own spacing, straight over bare quoted options held, while the
// same frame with an ASCII space read clear. Both tests read it now, under
// #7388's anchor: the composer decides only when it explains every row on
// screen outside it.

// nbspComposerPrompts are a composer's prompt row in each of its modes, in
// 2.1.322's spacing: the glyph, U+00A0, then a draft or nothing.
var nbspComposerPrompts = []string{"!\u00a0ls", "#\u00a0note", ">\u00a0fix it", "!\u00a0", "#\u00a0", ">\u00a0"}

// quotedOptionBlocks are the launch dialogs' option blocks with their footers,
// as a transcript quotes them: the reworded folder dialog's, the legacy one's
// and the MCP server dialog's.
var quotedOptionBlocks = map[string][]string{
	"reworded":   {"❯ 1. Yes, I trust this folder", "  2. No, exit", "", "Enter to confirm · Esc to cancel"},
	"legacy":     {"❯ 1. Yes, proceed", "  2. No, exit", "", "Enter to confirm · Esc to exit"},
	"MCP server": dialogHeads[3].options,
}

// The issue's frames, #7412 round 1's n1 and n2, and every mode in 2.1.322's
// spacing, between plain rules or under its opening rule alone, straight over
// each dialog's bare quoted options: a composer over text it quotes, as with an
// ASCII space.
func TestCheckAndHandleTrustPrompt_Issue7454RuledComposerAfterANonBreakingSpaceOverQuotedOptionsStaysClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"n1: a ruled bash-mode composer", "────────────\n!\u00a0ls\n────────────\n❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel"},
		{"n2: a ruled > composer", "────────────\n>\u00a0fix it\n────────────\n❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "a composer over quoted options is no dialog:\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
	for form, composer := range ruledComposers {
		for name, options := range quotedOptionBlocks {
			t.Run(fmt.Sprintf("%s, over the %s options", form, name), func(t *testing.T) {
				for _, prompt := range nbspComposerPrompts {
					for _, width := range []int{12, 16, 24, 40, 60} {
						for _, gap := range [][]string{nil, {""}} {
							rows := slices.Concat(composer(width, prompt), gap, indentedRows(width, options...))
							content := strings.Join(rows, "\n")
							handled, cmds := pollStaticPane(t, content, 4)
							require.False(t, handled, "%d columns, prompt %q: a composer over quoted options is no dialog:\n%s", width, prompt, content)
							require.Empty(t, sentKeystrokes(cmds))
						}
					}
				}
			})
		}
	}
}

// The same composers decide nothing over a dialog with any of its own rows on
// screen, below the composer or above it (#7388; #7412 review, R2, R3, R7): a
// live dialog under a stale composer still holds, and af sends it no key.
func TestCheckAndHandleTrustPrompt_Issue7454RuledComposerAfterANonBreakingSpaceOverADialogsOwnRowsHolds(t *testing.T) {
	for form, composer := range ruledComposers {
		for _, prompt := range nbspComposerPrompts {
			t.Run(fmt.Sprintf("%s %q", form, prompt), func(t *testing.T) {
				for _, width := range []int{16, 40} {
					below := map[string][]string{
						"a new-wording dialog":              indentedRows(width, newWordingDialog...),
						"a new-wording dialog's top rule":   ruleTopDialog(width, newWordingDialog),
						"a framed dialog, footer inside":    framedRewordedDialogRows(width, "inside"),
						"a framed dialog, footer outside":   framedRewordedDialogRows(width, "outside"),
						"a heading over the folder options": indentedRows(width, append([]string{"Security guide", ""}, quotedOptionBlocks["reworded"]...)...),
					}
					for _, dialog := range dialogHeads {
						if dialog.name == "the legacy question" {
							// Whole, the legacy dialog is one af reads and
							// answers by its label (claudeTrustPromptPresent),
							// under a composer or not.
							continue
						}
						below[dialog.name] = indentedRows(width, append(slices.Clone(dialog.head), dialog.options...)...)
					}
					for name, dialog := range below {
						for _, gap := range [][]string{nil, {""}} {
							content := strings.Join(slices.Concat(composer(width, prompt), gap, dialog), "\n")
							requireHeldWithoutAKey(t, content, fmt.Sprintf("%d columns: %s under a stale composer is in the way", width, name))
						}
					}
					for _, dialog := range dialogHeads {
						// The dialog's head above the composer, its options below.
						content := strings.Join(slices.Concat(indentedRows(width, dialog.head...), composer(width, prompt), indentedRows(width, dialog.options...)), "\n")
						requireHeldWithoutAKey(t, content, fmt.Sprintf("%d columns: %s above a composer is still in the way", width, dialog.name))
					}
				}
			})
		}
	}
}
