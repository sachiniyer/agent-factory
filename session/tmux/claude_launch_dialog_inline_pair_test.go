package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for the inline-option-row gap in #7349/#7584's narrow-pane
// rewrap reader (the bug the report flags). Every test drives
// CheckAndHandleTrustPrompt through pollStaticPane: a false return is the
// create path's permission to type the user's prompt and press Enter, which
// the legacy dialog takes on its preselected "Yes". The same dialog with
// stacked options was already pinned to hold at every width by
// TestCheckAndHandleTrustPrompt_LegacyDialogIsNeverClearUnderItsQuestionWidth
// and TestIssue7584LegacyDialogHoldsUntilItsFooterIsWhole; the matching
// fixtures here pin the inline form (`❯ Yes  No`, with or without the
// "1."/"2." ordinals) to the same property.

// legacyInlinePairDialogRows renders the legacy folder-trust dialog with its
// options painted as the single-row inline picker (claudeLegacyInlinePair):
// plain ("❯ Yes  No") when ordinal is false, "❯ 1. Yes  2. No" when true. The
// rows are wrapped at the pane width the way Claude Code's renderer wraps a
// row at its spaces. footer controls whether the full footer is appended.
func legacyInlinePairDialogRows(width int, ordinal, footer bool) []string {
	rows := wrapAtSpaces("Do you trust the files in this folder?", width)
	rows = append(rows, "")
	rows = append(rows, wrapAtSpaces("/home/u/proj", width)...)
	rows = append(rows, "")
	rows = append(rows, wrapAtSpaces("Claude Code may read files in this folder.", width)...)
	rows = append(rows, "")
	picker := "❯ Yes  No"
	if ordinal {
		picker = "❯ 1. Yes  2. No"
	}
	rows = append(rows, wrapAtSpaces(picker, width)...)
	if footer {
		rows = append(rows, "")
		rows = append(rows, wrapAtSpaces(legacyTrustFooter, width)...)
	}
	return rows
}

// legacyInlinePairOptionBlock renders just the inline picker, for the
// scrolled case.
func legacyInlinePairOptionBlock(ordinal bool) []string {
	if ordinal {
		return []string{"❯ 1. Yes  2. No"}
	}
	return []string{"❯ Yes  No"}
}

// inlineVariant names the picker shape under test.
type inlineVariant struct {
	name    string
	ordinal bool
}

var inlineVariants = []inlineVariant{
	{"plain", false},
	{"ordinal", true},
}

// TestCheckAndHandleTrustPrompt_InlinePairLegacyDialogIsNeverClearUnderItsQuestionWidth
// pins the property TestCheckAndHandleTrustPrompt_LegacyDialogIsNeverClearUnderItsQuestionWidth
// already asserts for the stacked legacy options, this time against the
// inline pair. The inline pair painted with the question wrapped (so the
// picker shares a row), at any width from the narrowest up through the width
// the question itself fits, must never read clear and must hold with no key:
// it had been CLEAR from the inline-pair-on-one-row widths up through 37
// columns, the #3579-class failure the trust gate exists to prevent.
func TestCheckAndHandleTrustPrompt_InlinePairLegacyDialogIsNeverClearUnderItsQuestionWidth(t *testing.T) {
	for _, v := range inlineVariants {
		for width := 6; width < len("Do you trust the files in this folder?"); width++ {
			whole := legacyInlinePairDialogRows(width, v.ordinal, true)
			footer := len(wrapAtSpaces(legacyTrustFooter, width))
			// Scrolled: every suffix that still shows a row of the option block.
			for height := footer + 2; height <= len(whole); height++ {
				content := strings.Join(whole[len(whole)-height:], "\n")
				if strings.TrimSpace(whole[len(whole)-height]) == "" {
					continue
				}
				name := fmt.Sprintf("%s/%d cols last %d rows", v.name, width, height)
				handled, cmds := pollStaticPane(t, content, 2)
				require.True(t, handled, "%s reads clear:\n%s", name, content)
				require.Empty(t, sentKeystrokes(cmds), "%s gets no key:\n%s", name, content)
			}
			// Mid-paint: every proper prefix, from the question's second row on,
			// including the rows below where the picker has not been painted.
			painting := legacyInlinePairDialogRows(width, v.ordinal, false)
			question := len(wrapAtSpaces("Do you trust the files in this folder?", width))
			for painted := min(2, question); painted <= len(painting); painted++ {
				content := strings.Join(painting[:painted], "\n")
				name := fmt.Sprintf("%s/%d cols %d rows painted", v.name, width, painted)
				handled, cmds := pollStaticPane(t, content, 2)
				require.True(t, handled, "%s reads clear:\n%s", name, content)
				require.Empty(t, sentKeystrokes(cmds), "%s gets no key:\n%s", name, content)
			}
		}
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairTailHolds pins the scrolled-tail
// rewrap hold for the inline pair, the way
// TestCheckAndHandleTrustPrompt_FolderTrustTailHolds already does for
// stacked options. The inline option block over its whole footer must hold
// without a key, naming the legacy dialog in the refusal line.
func TestCheckAndHandleTrustPrompt_InlinePairTailHolds(t *testing.T) {
	for _, v := range inlineVariants {
		for _, tt := range []struct{ name, content string }{
			{"the inline pair over the footer", strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n\n" + legacyTrustFooter},
			{"no blank row above the footer", strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n" + legacyTrustFooter},
			{"with a body row above", "Claude Code may read files in this folder.\n\n" + strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n\n" + legacyTrustFooter},
			{"under the reworded footer", strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n\n" + rewordedTrustFooter},
		} {
			t.Run(v.name+"/"+tt.name, func(t *testing.T) {
				_, _, errLog := captureTrustPromptLogs(t)
				handled, cmds := pollStaticPane(t, tt.content, 4)
				require.True(t, handled,
					"a folder-trust dialog with inline options is on screen; a clear pane lets the create "+
						"type its prompt into it (#7349's gap):\n%s", tt.content)
				require.Empty(t, sentKeystrokes(cmds),
					"a dialog af cannot read whole gets no key; got %v", cmds)
				require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"),
					"the hold is reported once, not once per poll; got:\n%s", errLog.String())
			})
		}
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairTailIsNeverClearAtAnyWidth pins
// the same property as
// TestCheckAndHandleTrustPrompt_FolderTrustTailIsNeverClearAtAnyWidth, this
// time against the inline picker. From any row of the option block down, at
// every narrow width up through the wide one, the pane never reads clear and
// receives no key.
func TestCheckAndHandleTrustPrompt_InlinePairTailIsNeverClearAtAnyWidth(t *testing.T) {
	for _, v := range inlineVariants {
		options := legacyInlinePairOptionBlock(v.ordinal)
		for width := 6; width <= 60; width++ {
			var rows []string
			for _, option := range options {
				rows = append(rows, wrapAtSpaces(option, width)...)
			}
			rows = append(rows, "")
			footer := wrapAtSpaces(legacyTrustFooter, width)
			rows = append(rows, footer...)
			for height := len(footer) + 2; height <= len(rows); height++ {
				content := strings.Join(rows[len(rows)-height:], "\n")
				name := fmt.Sprintf("%s/%d columns last %d rows", v.name, width, height)
				handled, cmds := pollStaticPane(t, content, 2)
				require.True(t, handled, "%s reads clear:\n%s", name, content)
				require.Empty(t, sentKeystrokes(cmds), "%s gets no key:\n%s", name, content)
			}
		}
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairFooterPaintingHolds pins the
// footer-still-being-painted hold for the inline pair, the way
// TestIssue7584LegacyDialogHoldsUntilItsFooterIsWhole does for the stacked
// shape. The whole option block over the start of the footer (the footer
// has only begun, or is cut short by a pane too narrow for it) must hold
// without a key, regardless of how narrow the pane is.
func TestCheckAndHandleTrustPrompt_InlinePairFooterPaintingHolds(t *testing.T) {
	for _, v := range inlineVariants {
		for _, setup := range []struct {
			name  string
			below string // the row(s) below the option block
		}{
			{"the footer whole", "\n\n" + legacyTrustFooter},
			{"no blank row then the footer whole", "\n" + legacyTrustFooter},
			{"the footer just begun", "\n\nEnter to confirm"},
			{"the footer first word only", "\n\nEnter"},
			{"the footer cut short mid-row", "\n\nEnter to confirm · Esc to e"},
		} {
			t.Run(v.name+"/"+setup.name, func(t *testing.T) {
				content := strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + setup.below
				handled, cmds := pollStaticPane(t, content, 4)
				require.True(t, handled, "%s reads clear:\n%s", t.Name(), content)
				require.Empty(t, sentKeystrokes(cmds), "%s gets no key:\n%s", t.Name(), content)
			})
		}
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairRefusalNamesTheLegacyDialog pins
// that the gate's refusal line names the LEGACY dialog (not the reworded
// one) when the inline pair is what it sees, the way
// TestCheckAndHandleTrustPrompt_LegacyDialogRefusalNamesTheLegacyDialog
// does for the stacked form. This pins the inline pair's classification in
// claudeFolderTrustOptionsRead.
func TestCheckAndHandleTrustPrompt_InlinePairRefusalNamesTheLegacyDialog(t *testing.T) {
	for _, v := range inlineVariants {
		t.Run(v.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			content := "Claude Code may read files in this folder.\n\n" +
				strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n\n" + legacyTrustFooter
			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled)
			require.Empty(t, sentKeystrokes(cmds),
				"af never answers a dialog it cannot read whole; got %v", cmds)
			logged := errLog.String()
			require.Equal(t, 1, strings.Count(logged, "refusing to answer"),
				"got:\n%s", logged)
			require.Contains(t, logged, "refusing to answer the "+legacyTrustDialogName+" dialog",
				"the inline pair is the legacy dialog's affine — name it as such")
			require.NotContains(t, logged, "the question is not on screen",
				"the whole dialog scrolls the question off, that is not this case")
		})
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairWithWrappedQuestionHolds pins the
// wrapped-question clause (claudeLaunchDialogPainting via
// claudeLaunchDialogComposerBelow) against the inline pair, where the cursor
// row squashes to "YesNo"/"Yes2.No" and would otherwise read as a composer
// draft. Each fixture paints the question wrapped onto several rows, then
// the inline pair on one row, optionally followed by a partial footer.
func TestCheckAndHandleTrustPrompt_InlinePairWithWrappedQuestionHolds(t *testing.T) {
	for _, v := range inlineVariants {
		for _, tt := range []struct{ name, content string }{
			{
				"the question wrapped, the inline pair on one row",
				"Do you trust the\nfiles in this\nfolder?\n\n/home/u/proj\n\nClaude Code may read\nfiles in this\nfolder.\n\n" +
					strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n\nEnter to confirm ·\nEsc to exit",
			},
			{
				"the question wrapped, no body, the inline pair on one row",
				"Do you trust the\nfiles in this\nfolder?\n\n" +
					strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n\nEnter to confirm ·\nEsc to exit",
			},
			{
				"the question wrapped, the inline pair, the footer not yet painted",
				"Do you trust the\nfiles in this\nfolder?\n\n/home/u/proj\n\n" +
					strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n"),
			},
			{
				"the question wrapped, the inline pair, the footer only begun",
				"Do you trust the\nfiles in this\nfolder?\n\n/home/u/proj\n\n" +
					strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n") + "\n\nEnter to confirm",
			},
		} {
			t.Run(v.name+"/"+tt.name, func(t *testing.T) {
				_, _, errLog := captureTrustPromptLogs(t)
				handled, cmds := pollStaticPane(t, tt.content, 4)
				require.True(t, handled,
					"the inline pair on one row under a wrapped question must hold; it had read clear "+
						"(#7349's gap):\n%s", tt.content)
				require.Empty(t, sentKeystrokes(cmds),
					"af holds the inline picker without a key; got %v", cmds)
				require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"),
					"the hold is reported once, not once per poll; got:\n%s", errLog.String())
			})
		}
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairComposerDraftStaysClear pins the
// no-regression guard the bug report calls out: the inline pair held as a
// dialog ONLY in the trailing-region position the dialogs own. The same
// pair under a quoted question with the agent's composer below it (boxed or
// ruled, in any of the composer's modes, or unframed with prose above the
// question) must keep reading clear, the way
// TestCheckAndHandleTrustPrompt_YesNoComposerDraftIsNotTheLegacyPicker and
// the #7584 quote/draft tests require. A new hold that fires on the inline
// pair in the composer's own position would type Enter into a working agent.
func TestCheckAndHandleTrustPrompt_InlinePairComposerDraftStaysClear(t *testing.T) {
	for _, v := range inlineVariants {
		picker := strings.Join(legacyInlinePairOptionBlock(v.ordinal), "\n")
		// The exact composer-draft frames this gate has historically had to
		// leave alone, with the inline pair on the composer's prompt row and
		// the legacy question quoted above it.
		for _, tt := range []struct{ name, content string }{
			{
				"the composer draft in a box under the whole-row question",
				"Do you trust the files in this folder?\n" +
					"╭────────────────────────────────────╮\n" +
					"│ " + picker[:len(picker)] + "                           │\n" +
					"╰────────────────────────────────────╯\n",
			},
			{
				"the composer draft between rules under prose",
				"I was asked: \"Do you trust the files in this folder?\"\nDo you trust the files in this folder?\n" +
					picker + "\n",
			},
			{
				"the composer draft between rules with the question above",
				"Do you trust the files in this folder?\n" +
					"────────────────────────────────────\n" +
					picker + "\n────────────────────────────────────\n",
			},
			{
				"prose above the question then the draft unframed",
				"agent output above the quote\nDo you trust the files in this folder?\n" + picker + "\n",
			},
		} {
			t.Run(v.name+"/"+tt.name, func(t *testing.T) {
				handled, cmds := pollStaticPane(t, tt.content, 4)
				require.False(t, handled,
					"a composer draft quoting the pair under the question is not the legacy picker: %q", tt.content)
				require.Empty(t, sentKeystrokes(cmds),
					"no key may reach a working composer; got %v", cmds)
			})
		}
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairOrdinalComposerDraftUnderWrappedQuestionStaysClear
// pins the wrapped-question branch (claudeLaunchDialogPainting via
// claudeLaunchDialogComposerBelow) against a boxed composer whose ❯ draft
// quotes the inline pair with its No ordinal ("1. Yes  2. No"). claudeComposersCanonical
// leaves such a composer on screen — its prompt could be a dialog's option — so the
// squashed draft "Yes2.No" matched the inline-pair label this matcher accepts and
// the live composer row read as the dialog's option, holding a working agent (#7661
// review). The composer under the wrapped question must keep reading clear.
func TestCheckAndHandleTrustPrompt_InlinePairOrdinalComposerDraftUnderWrappedQuestionStaysClear(t *testing.T) {
	wrapped := "Do you trust the\nfiles in this\nfolder?"
	for _, boxed := range []bool{true} {
		for _, prompt := range []string{"❯ ", "❯\u00a0"} {
			for _, draft := range []string{"1. Yes  2. No", "Yes  No"} {
				composer := drawnComposer(40, prompt, []string{draft}, boxed)
				content := wrapped + "\n" + strings.Join(composer, "\n")
				handled, cmds := pollStaticPane(t, content, 4)
				require.Falsef(t, handled,
					"a boxed composer draft %q under a wrapped question is not the legacy picker:\n%s", draft, content)
				require.Emptyf(t, sentKeystrokes(cmds),
					"no key may reach a working composer; got %v", cmds)
			}
		}
	}
}

// TestCheckAndHandleTrustPrompt_InlinePairQuotedAboveAComposerStaysClear
// pins the larger #7584 guarantee with the inline pair: a draft quoting the
// pair (or the dialog's question + pair, or the block on one row) drawn
// inside a real composer, ruled or boxed and in any composer mode, stays
// CLEAR. This mirrors the body of TestIssue7584SettledScreensAreLeftAlone
// that pins the same property for the stacked labels.
func TestCheckAndHandleTrustPrompt_InlinePairQuotedAboveAComposerStaysClear(t *testing.T) {
	quoteDrafts := map[string][]string{
		"Yes No":               {"Yes No"},
		"Yes 2. No":            {"Yes 2. No"},
		"the block on one row": {"1. Yes, proceed 2. No, exit"},
	}
	for _, lead := range []string{
		"Do you trust the files in this folder?",
		"I was asked: \"Do you trust the files in this folder?\"\nDo you trust the files in this folder?",
	} {
		for name, draft := range quoteDrafts {
			for _, prompt := range composerModePrompts {
				for _, boxed := range []bool{false, true} {
					content := lead + "\n" + strings.Join(drawnComposer(40, prompt, draft, boxed), "\n")
					requireClearWithoutAKey(t, content,
						fmt.Sprintf("an inline-pair draft quoting %s under %q, boxed %v", name, prompt, boxed))
				}
			}
		}
	}
}
