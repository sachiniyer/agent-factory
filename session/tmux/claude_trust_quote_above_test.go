package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for the routing bug in answerClaudeTrustPrompt. The route to
// the Enter tap vs the label-picker was chosen by claudeFolderTrustDialogPresent,
// a whole-pane strings.Contains of "Yes, I trust this folder". When earlier agent
// output on the pane quotes that label anywhere, a live legacy or old-MCP modal
// — which the picker cannot read — was misrouted into the picker path, which
// refused without sending a key on every poll. CheckAndHandleTrustPrompt kept
// returning handled=true, so task.DismissTrustPrompt never saw a dialog-free
// frame and failed after MaxTrustPromptAttempts with "trust prompt did not
// dismiss after 20 attempts" on a dialog af is designed to answer with one Enter.
//
// The fix routes on claudeFolderTrustDialogOf, a structural read of the reworded
// dialog's trailing region: only a COMPLETE reworded dialog goes to the picker;
// everything else keeps the historical Enter tap. A quote of the label above a
// live legacy/MCP modal does not satisfy the reworded-dialog structure, so the
// modal reaches the Enter branch that was built for it. The reworded dialog with
// the label quoted above still passes through the picker (with the parser scoped
// to the same trailing region, so the quote above cannot make affirmatives > 1).
//
// Every case below is the byte-identical live modal from the real-dialog tests
// alongside it, with one extra line quoting the affirmative label. The outcome
// must NOT flip from "dismissed with Enter" to "refused forever": the same modal,
// with or without the quote, gets the same key.
func TestCheckAndHandleTrustPrompt_QuotedLabelAboveLiveModalStillFiresEnter(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{
			// The legacy folder-trust dialog with the affirmative label quoted on
			// the line above its question. claudeFolderTrustDialogPresent fired
			// on the quote and the live modal was misrouted into the picker.
			name: "legacy folder-trust",
			content: "Yes, I trust this folder\n" +
				"Do you trust the files in this folder?\n" +
				"❯ 1. Yes, proceed\n" +
				"  2. No, exit\n" +
				"Enter to confirm · Esc to exit\n",
		},
		{
			// The old single-server MCP modal with the label quoted above its
			// question. claudeFolderTrustDialogPresent fired on the quote and
			// the live modal was misrouted into the picker.
			name: "old MCP trust",
			content: "Yes, I trust this folder\n" +
				"New MCP server found. Do you trust this new MCP server?\n" +
				"❯ 1. Yes\n  2. No\n" +
				"Enter to confirm\n",
		},
		{
			// The label quoted in a sentence (not a bare row) above the MCP
			// modal — a raw strings.Contains fires on the sentence too.
			name: "old MCP trust, label quoted in a sentence above",
			content: "af must confirm \"Yes, I trust this folder\" on the folder dialog.\n" +
				"New MCP server found. Do you trust this new MCP server?\n" +
				"❯ 1. Yes\n  2. No\n" +
				"Enter to confirm\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramClaude, tt.content)
			require.True(t, handled,
				"the live modal with a quoted label above is still in the way")
			require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
				"the live modal must keep the historical Enter tap, not be misrouted into the picker path that refuses; got %v", cmds)
		})
	}
}

// The same guard from the other side: a live legacy or old-MCP modal that
// contains NO label quote — byte-identical to the cases above minus the quote —
// still fires exactly one Enter. This is the control that proves the routing
// change did not stop af answering the genuine legacy/MCP gates, and that the
// quote-above cases above flip on the quote alone, not on some other difference.
func TestCheckAndHandleTrustPrompt_LiveModalWithoutQuoteStillFiresEnter(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{
			name: "legacy folder-trust, no quote",
			content: "Do you trust the files in this folder?\n" +
				"❯ 1. Yes, proceed\n" +
				"  2. No, exit\n" +
				"Enter to confirm · Esc to exit\n",
		},
		{
			name: "old MCP trust, no quote",
			content: "New MCP server found. Do you trust this new MCP server?\n" +
				"❯ 1. Yes\n  2. No\n" +
				"Enter to confirm\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramClaude, tt.content)
			require.True(t, handled, "the live modal is in the way")
			require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
				"the live modal keeps the historical Enter tap; got %v", cmds)
		})
	}
}

// The reworded folder-trust dialog WITH the label quoted above it is still
// driven by label, not refused. This exercises the parse-level twin of the
// routing bug: parseClaudeFolderTrustDialog used to scan the whole pane, so a
// bare "Yes, I trust this folder" row above the dialog made affirmatives > 1
// and the picker refused without a key. With the parser scoped to the same
// trailing region claudeFolderTrustDialogOf reads, the quote above the question
// is outside the count, and the live dialog parses and is driven.
func TestCheckAndHandleTrustPrompt_QuotedLabelAboveLiveRewordedDialogIsDriven(t *testing.T) {
	pane := &claudeFolderTrustPane{
		options: []string{claudeTrustNoLabel, claudeTrustYesLabel},
		prefix:  "Yes, I trust this folder\n",
	}
	keys := driveClaudeTrustPane(t, pane, 6)
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(),
		"the reworded dialog with the label quoted above must be answered, not refused; keystrokes were %v", keys)
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(keys),
		"the label-picker must still move onto the affirmative row and confirm; got %v", keys)
}
