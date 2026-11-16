package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7346. Every test drives CheckAndHandleTrustPrompt: a
// false return is the create path's permission to type the user's prompt and
// press Enter, which the legacy dialog takes on its preselected "Yes, proceed".

const (
	// legacyTrustDialogName is the name a refusal line gives the legacy
	// dialog. Spelled out here so the name is pinned, not read back.
	legacyTrustDialogName = "Claude legacy folder-trust"
	legacyTrustFooter     = "Enter to confirm · Esc to exit"
	rewordedTrustFooter   = "Enter to confirm · Esc to cancel"
)

// legacyTrustDialogRows and rewordedTrustDialogRows render each dialog's rows
// as a pane width columns wide wraps them, at its spaces.
func legacyTrustDialogRows(width int, footer bool) []string {
	rows := wrapAtSpaces("Do you trust the files in this folder?", width)
	rows = append(rows, "")
	rows = append(rows, wrapAtSpaces("/home/u/proj", width)...)
	rows = append(rows, "")
	rows = append(rows, wrapAtSpaces("Claude Code may read files in this folder.", width)...)
	rows = append(rows, "")
	rows = append(rows, wrapAtSpaces("❯ 1. Yes, proceed", width)...)
	rows = append(rows, wrapAtSpaces("2. No, exit", width)...)
	if footer {
		rows = append(rows, "")
		rows = append(rows, wrapAtSpaces(legacyTrustFooter, width)...)
	}
	return rows
}

func rewordedTrustBodyAndOptionRows(width int) []string {
	rows := wrapAtSpaces("Security guide", width)
	rows = append(rows, "")
	rows = append(rows, wrapAtSpaces("❯ 1. Yes, I trust this folder", width)...)
	rows = append(rows, wrapAtSpaces("2. No, exit", width)...)
	rows = append(rows, "")
	return append(rows, wrapAtSpaces(rewordedTrustFooter, width)...)
}

func TestCheckAndHandleTrustPrompt_FolderTrustDialogAtAnyWrapHolds(t *testing.T) {
	for _, tt := range []struct{ name, content, dialog string }{
		// #7346 R1: the legacy dialog under 30 columns, a body row visible.
		{"legacy at 26 columns, a body row above the options", "Claude Code may read files\nin this folder.\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit", legacyTrustDialogName},
		{"the whole legacy dialog at 20 columns", "Do you trust the\nfiles in this\nfolder?\n\n/home/u/proj\n\nClaude Code may read\nfiles in this\nfolder.\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm ·\nEsc to exit", legacyTrustDialogName},
		{"legacy, words cut inside at 6 columns", "folder\n.\n\n❯ 1.\nYes,\nprocee\nd\n2. No,\nexit\n\nEnter\nto\nconfir\nm ·\nEsc to\nexit", legacyTrustDialogName},
		{"legacy options under the other footer, a body row above", "Claude Code may read files\nin this folder.\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\ncancel", legacyTrustDialogName},
		// #7346 R2: the reworded dialog under 16 columns, a body row visible.
		{"reworded at 13 columns, a body row above the options", "Security guide\n\n❯ 1. Yes, I\ntrust this\nfolder\n  2. No, exit\n\nEnter to\nconfirm · Esc\nto cancel", claudeFolderTrustDialogName},
		{"reworded, the closing frame above a wrapped footer", "Security guide\n\n❯ 1. Yes, I\ntrust this\nfolder\n  2. No, exit\n╰───────────╯\nEnter to\nconfirm · Esc\nto cancel", claudeFolderTrustDialogName},
		// The legacy dialog mid-paint, its question wrapped.
		{"legacy mid-paint, cut inside the wrapped question", "Do you trust the\nfiles in this", legacyTrustDialogName},
		{"the first words the legacy and the MCP question share", "Do you\ntrust", claudeLaunchGateDialogName},
		{"legacy mid-paint, the wrapped question and a body row", "Do you trust the\nfiles in this\nfolder?\n\n/home/u/proj", legacyTrustDialogName},
		{"legacy mid-paint, one option row painted", "Do you trust the\nfiles in this\nfolder?\n\n/home/u/proj\n\n❯ 1. Yes, proceed", legacyTrustDialogName},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "a folder-trust dialog is on screen; a clear pane lets the create "+
				"type its prompt into it (#7346):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a dialog af cannot read whole gets no key; got %v", cmds)
			require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"),
				"the hold is reported once, not once per poll; got:\n%s", errLog.String())
			require.Contains(t, errLog.String(), "refusing to answer the "+tt.dialog+" dialog",
				"the refusal names the dialog the pane shows")
		})
	}
}

// At every width, the legacy dialog is never a clear pane: not whole with its
// question wrapped, not scrolled with a body row still showing, and not at any
// row of its paint. Under 38 columns the question does not fit a row, so af
// cannot answer it and sends no key.
func TestCheckAndHandleTrustPrompt_LegacyDialogIsNeverClearUnderItsQuestionWidth(t *testing.T) {
	for width := 6; width < len("Do you trust the files in this folder?"); width++ {
		whole := legacyTrustDialogRows(width, true)
		question := len(wrapAtSpaces("Do you trust the files in this folder?", width))
		footer := len(wrapAtSpaces(legacyTrustFooter, width))
		// Scrolled: every suffix that still shows a row of the option block.
		for height := footer + 2; height <= len(whole); height++ {
			content := strings.Join(whole[len(whole)-height:], "\n")
			if strings.TrimSpace(whole[len(whole)-height]) == "" {
				continue
			}
			handled, cmds := pollStaticPane(t, content, 2)
			require.True(t, handled, "%d columns, the last %d rows read clear:\n%s", width, height, content)
			require.Empty(t, sentKeystrokes(cmds), "%d columns, the last %d rows get no key:\n%s", width, height, content)
		}
		// Mid-paint: every proper prefix, from the question's second row on.
		painting := legacyTrustDialogRows(width, false)
		for painted := min(2, question); painted <= len(painting); painted++ {
			content := strings.Join(painting[:painted], "\n")
			handled, cmds := pollStaticPane(t, content, 2)
			require.True(t, handled, "%d columns, %d rows painted read clear:\n%s", width, painted, content)
			require.Empty(t, sentKeystrokes(cmds), "%d columns, %d rows painted get no key:\n%s", width, painted, content)
		}
	}
}

// The reworded dialog scrolled so that a body row still shows above its
// options is never a clear pane, at any width.
func TestCheckAndHandleTrustPrompt_RewordedDialogBodyAndOptionsNeverClear(t *testing.T) {
	for width := 6; width <= 60; width++ {
		rows := rewordedTrustBodyAndOptionRows(width)
		footer := len(wrapAtSpaces(rewordedTrustFooter, width))
		for height := footer + 2; height <= len(rows); height++ {
			if strings.TrimSpace(rows[len(rows)-height]) == "" {
				continue
			}
			content := strings.Join(rows[len(rows)-height:], "\n")
			name := fmt.Sprintf("%d columns, the last %d rows", width, height)
			handled, cmds := pollStaticPane(t, content, 2)
			require.True(t, handled, "%s read clear:\n%s", name, content)
			require.Empty(t, sentKeystrokes(cmds), "%s get no key:\n%s", name, content)
		}
	}
}

// framedRewordedDialogRows renders the whole reworded folder-trust dialog
// painted inside a box that some Claude Code builds draw (claude 2.1.258 does
// not, but the framed form "must parse identically" — claudeTrustBoxDrawing).
// variant "inside" paints the footer within the box, above its bottom border;
// variant "outside" paints the box closed directly above an outside footer.
// The top border is the line that the old case-3 scan rejected, reading the
// fully painted dialog clear at several narrow widths.
func framedRewordedDialogRows(width int, variant string) []string {
	content := width - 4
	if content < 1 {
		content = 1
	}
	frame := func(row string) string {
		for len([]rune(row)) < content {
			row += " "
		}
		return "│ " + row + " │"
	}
	top := "╭" + strings.Repeat("─", width-2) + "╮"
	bottom := "╰" + strings.Repeat("─", width-2) + "╯"
	logical := []string{
		"Quick safety check: Is this a project you created or one you trust? (Like your own code, …).",
		"",
		"Security guide",
		"",
		"❯ 1. Yes, I trust this folder",
		"2. No, exit",
	}
	var rows []string
	rows = append(rows, top)
	for _, l := range logical {
		if l == "" {
			rows = append(rows, frame(""))
			continue
		}
		for _, piece := range wrapAtSpaces(l, content) {
			rows = append(rows, frame(piece))
		}
	}
	switch variant {
	case "inside":
		for _, piece := range wrapAtSpaces(rewordedTrustFooter, content) {
			rows = append(rows, frame(piece))
		}
		rows = append(rows, bottom)
	case "outside":
		rows = append(rows, bottom)
		for _, piece := range wrapAtSpaces(rewordedTrustFooter, width) {
			rows = append(rows, piece)
		}
	}
	return rows
}

// A fully painted FRAMED reworded dialog holds at every width, the way its
// unframed form does (TestCheckAndHandleTrustPrompt_RewordedDialogBodyAndOptionsNeverClear).
// The case-3 scan in claudeLaunchDialogTail reused claudeTrustFrameRule, which
// matches the dialog's own top border ╭───╮, so at several narrow widths the
// matcher rejected the dialog's own frame and the pane read clear — letting the
// create path type the user's prompt into the live modal. With the fix, a frame
// rule above the options only rejects when it bounds a boxed composer (a row
// reading ">" or "> "), not the dialog's own top border.
func TestCheckAndHandleTrustPrompt_FramedRewordedDialogHoldsAtAnyWidth(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	for width := 8; width <= 60; width++ {
		for _, variant := range []string{"inside", "outside"} {
			rows := framedRewordedDialogRows(width, variant)
			content := strings.Join(rows, "\n")
			name := fmt.Sprintf("%d columns, %s the box", width, variant)
			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled, "%s read clear; a fully painted framed dialog holds (#7349 top-border regression):\n%s", name, content)
			require.Empty(t, sentKeystrokes(cmds), "%s sends no key:\n%s", name, content)
		}
	}
	require.Contains(t, errLog.String(), "refusing to answer the "+claudeFolderTrustDialogName+" dialog",
		"the hold is reported as the reworded folder-trust dialog; got:\n%s", errLog.String())
}

// A framed dialog whose body has scrolled off the top — so only the top border
// is NOT visible — already held before the fix. This pins that the fix does not
// regress the headless case: removing the top border must leave the pane held
// (the case-3 scan finds no frame rule above the options at all), and adding it
// back must hold too now.
func TestCheckAndHandleTrustPrompt_FramedRewordedDialogTopScrolledOffHolds(t *testing.T) {
	for width := 8; width <= 30; width++ {
		rows := framedRewordedDialogRows(width, "inside")
		// Drop the top border line: the question is scrolled off, the body
		// (Security guide) still shows above the options.
		scrolled := rows[1:]
		content := strings.Join(scrolled, "\n")
		handled, cmds := pollStaticPane(t, content, 4)
		require.True(t, handled, "%d columns, top border scrolled off still holds:\n%s", width, content)
		require.Empty(t, sentKeystrokes(cmds), "%d columns, top scrolled sends no key:\n%s", width, content)
	}
}

// A working agent below the rows, or rows that are not the dialog's, and the
// pane stays clear, as before.
func TestCheckAndHandleTrustPrompt_FolderTrustDialogTextThatIsNoDialogStaysClear(t *testing.T) {
	legacy := "Claude Code may read files\nin this folder.\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"
	for _, tt := range []struct{ name, content string }{
		{"the ruled composer below", legacy + ruledComposerBelow},
		{"the boxed composer below", legacy + boxedComposerBelow},
		// A boxed composer painted ABOVE the dialog's option rows and footer
		// (a quoted dialog with the agent's live composer above it) must stay
		// clear: the case-3 scan rejects the composer's box edge because the
		// row next to it reads "> ", not because every frame rule rejects.
		// This is the false-hold a plain-rule-only scan would produce, so it
		// pins that the fix did not weaken the boxed-composer guard.
		{"the boxed composer above the options", "╭────────────────────────────────────╮\n│ > Type your message here           │\n╰────────────────────────────────────╯\n❯ 1. Yes, I trust this folder\n2. No, exit\n\nEnter to confirm · Esc to cancel"},
		{"the boxed composer above the legacy options", "╭────────────────────────────────────╮\n│ > Type your message here           │\n╰────────────────────────────────────╯\n❯ 1. Yes, proceed\n2. No, exit\n\nEnter to confirm · Esc to exit"},
		{"a hint row below the footer", legacy + "\n? for shortcuts"},
		{"a ❯ row above the options", "❯ fix it\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"},
		{"a plain rule above the options", "────────────\nsome output\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"},
		{"the footer's words closing a sentence", "Claude Code may read files\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nThen press Enter to confirm · Esc to\nexit"},
		{"body rows over the exit row alone", "Claude Code may read files\nin this folder.\n\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"},
		{"the wrapped legacy question quoted above the composer", "Do you trust the\nfiles in this\nfolder?\n\nThat is what it asks." + ruledComposerBelow},
		{"the wrapped legacy question above a ❯ draft", "Do you trust the\nfiles in this\nfolder?\n❯ yes please"},
		{"the legacy question inside a sentence", "It asked: Do you trust the\nfiles in this folder?"},
		{"the start of the legacy question, then other words", "Do you trust the\nprocess?"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// #7346 R3: a refusal line describes what the pane shows. The whole legacy
// dialog with its cursor on No is refused as the legacy dialog, with its own
// affirmative row, and is not said to have its question off screen.
func TestCheckAndHandleTrustPrompt_LegacyDialogRefusalNamesTheLegacyDialog(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	content := "Do you trust the files in this folder?\n\n/home/u/proj\n\n  1. Yes, proceed\n❯ 2. No, exit\n\nEnter to confirm · Esc to exit"
	handled, cmds := pollStaticPane(t, content, 4)
	require.True(t, handled)
	require.Empty(t, sentKeystrokes(cmds), "af does not answer a legacy dialog whose cursor is not on Yes")
	logged := errLog.String()
	require.Equal(t, 1, strings.Count(logged, "refusing to answer"), "got:\n%s", logged)
	require.Contains(t, logged, "refusing to answer the "+legacyTrustDialogName+" dialog")
	require.Contains(t, logged, `"Yes, proceed"`)
	require.NotContains(t, logged, "the question is not on screen", "the question is on screen")
	require.NotContains(t, logged, "Yes, I trust this folder", "that is the other dialog's row")
}

// The whole legacy dialog on a pane wide enough for its question is still
// answered with its one Enter.
func TestCheckAndHandleTrustPrompt_WholeLegacyDialogAtFullWidthIsStillAnswered(t *testing.T) {
	content := strings.Join(legacyTrustDialogRows(60, true), "\n")
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, content)
	require.True(t, handled)
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)))
}
