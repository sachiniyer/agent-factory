package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for the styled-rendering gate defect.
//
// claudeTrustPromptPresent is the gate task.DismissTrustPrompt consults: a
// false there is the permission to type the user's prompt into the pane. The
// gate's reworded folder-trust clause and MCP question half were matched with
// raw strings.Contains/strings.ToLower against capture-pane -e output, so a
// dialog Claude Code paints with per-word SGR styling (the measured render in
// testdata/claude_2_1_322_mcp_server_dialog.ansi) broke the byte-contiguity of
// every multi-word phrase and the gate reported the pane dialog-free. The
// prompt was then typed into the live modal and Enter landed on Claude Code's
// preselected row — the exact #3579 catastrophe the file exists to prevent.
//
// The fix strips ANSI before matching, the way every sibling does; since #7271
// the gate's clauses all read one visible text, stripped once per capture
// (visibleTextOf). These tests feed the gate (and the end-to-end
// CheckAndHandleTrustPrompt path) per-word styled renders the suite never had.
// Every folder-trust render here is SYNTHETIC, modelled on the measured 2.1.322
// MCP-server capture: no capture of the folder-trust dialog exists in testdata.

// styleMCPQuestionPerWord wraps each word of the MCP question in its own SGR
// span, mirroring the per-word styling measured in the 2.1.322 captures. The
// footer is left plain so claudeMCPTrustFooterIsLast is exercised against the
// same styled question the raw clause failed on.
func styleMCPQuestionPerWord(s string) string { return styleWordsPerWord(s) }

// styledRewordedFolderTrustDialog renders the full reworded folder-trust modal
// with per-word SGR on the question and the affirmative label, exactly the
// styling that broke the gate's raw clauses.
func styledRewordedFolderTrustDialog() string {
	question := styleWordsPerWord("Is this a project you created or one you trust?")
	yes := styleWordsPerWord("Yes, I trust this folder")
	no := "No, cancel"
	return "Quick safety check:\n" +
		question + "\n\n" +
		"❯ 1. " + yes + "\n" +
		"  2. " + no + "\n\n" +
		"Enter to confirm · Esc to cancel\n"
}

// styledRewordedFolderTrustDialogStyledNo renders the dialog with the question
// and BOTH option labels styled per-word. The cursor is on the No row, so the
// style that defeats the gate is NOT on the selected row — matching the report's
// finding that styling the question row alone is enough.
func styledRewordedFolderTrustDialogStyledNo() string {
	question := styleWordsPerWord("Is this a project you created or one you trust?")
	yes := styleWordsPerWord("Yes, I trust this folder")
	no := styleWordsPerWord("No, cancel")
	return "Quick safety check:\n" +
		question + "\n\n" +
		"  1. " + yes + "\n" +
		"❯ 2. " + no + "\n\n" +
		"Enter to confirm · Esc to cancel\n"
}

// contiguousSpanFolderTrustDialog wraps the WHOLE question and the whole label
// in a single contiguous SGR span (not per-word). A single span does not break
// byte-contiguity, so this is the control: the raw clause matched this case
// even before the fix, and must continue to match after it.
func contiguousSpanFolderTrustDialog() string {
	question := "\x1b[1m\x1b[38;5;220mIs this a project you created or one you trust?\x1b[0m"
	yes := "\x1b[32mYes, I trust this folder\x1b[0m"
	return "Quick safety check:\n" +
		question + "\n\n" +
		"❯ 1. " + yes + "\n" +
		"  2. No, cancel\n\n" +
		"Enter to confirm · Esc to cancel\n"
}

// styledMCPDialog renders the older MCP trust modal with the per-word styled
// question and the footer last, the shape the gate's MCP question half missed.
func styledMCPDialog() string {
	question := styleMCPQuestionPerWord("New MCP server found. Do you trust this new MCP server?")
	return question + "\n" +
		"❯ 1. Yes\n" +
		"  2. No\n" +
		"Enter to confirm"
}

// styledMCPDialogLowercase is the lowercase variant, for the case-insensitive
// question half.
func styledMCPDialogLowercase() string {
	question := styleMCPQuestionPerWord("new mcp server found. do you trust this new mcp server?")
	return question + "\n" +
		"❯ 1. yes\n" +
		"  2. no\n" +
		"enter to confirm"
}

// styledMCPDialogQuotedAboveComposer is the negative: the styled MCP phrase with
// the agent's composer painted below it. The footer is NOT last, so the gate
// must still refuse even though the question half now matches.
func styledMCPDialogQuotedAboveComposer() string {
	return styledMCPDialog() + "\n\n" +
		"╭────────────────────────────────────╮\n" +
		"│ > Type your message here           │\n" +
		"╰────────────────────────────────────╯\n" +
		"? for shortcuts\n"
}

// styledRewordedMention is the negative: the styled question appears in
// scrollback/agent output with no dialog chrome — must NOT be treated as a
// live prompt.
func styledRewordedMention() string {
	return styleWordsPerWord("The onboarding docs ask: \"Is this a project you created or one you trust?\"") + "\n" +
		"Here is a summary of what I changed in the repo...\n"
}

// TestClaudeTrustPromptPresent_StyledRenderingIsDetected is the core regression:
// the gate must identify a dialog whose multi-word phrases are broken by
// per-word SGR spans, the input the suite never fed it. Each styled variant
// must be positively present.
func TestClaudeTrustPromptPresent_StyledRenderingIsDetected(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "reworded folder-trust, per-word styled question and Yes label, cursor on Yes",
			content: styledRewordedFolderTrustDialog(),
			want:    true,
		},
		{
			name:    "reworded folder-trust, per-word styled, cursor on No (style on the unselected row too)",
			content: styledRewordedFolderTrustDialogStyledNo(),
			want:    true,
		},
		{
			name:    "reworded folder-trust, single contiguous SGR span (control — never broken)",
			content: contiguousSpanFolderTrustDialog(),
			want:    true,
		},
		{
			name:    "older MCP dialog, per-word styled question, footer last",
			content: styledMCPDialog(),
			want:    true,
		},
		{
			name:    "older MCP dialog, per-word styled question, lowercase, footer last",
			content: styledMCPDialogLowercase(),
			want:    true,
		},
		{
			name:    "styled MCP phrase quoted above composer — footer NOT last",
			content: styledMCPDialogQuotedAboveComposer(),
			want:    false,
		},
		{
			name:    "styled reworded phrase without dialog chrome",
			content: styledRewordedMention(),
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claudeTrustPromptPresent(visibleTextOf(tt.content))
			require.Equal(t, tt.want, got,
				"claudeTrustPromptPresent must see through per-word SGR styling the way its siblings do")
		})
	}
}

// TestClaudeTrustPromptPresent_StyledGateAgreesWithSiblings asserts the
// consistency the report names: every ANSI-stripping sibling already
// identified the styled dialog, so the gate must agree with them on the same
// input. Before the fix the gate was the lone hold-out.
func TestClaudeTrustPromptPresent_StyledGateAgreesWithSiblings(t *testing.T) {
	styledFolder := styledRewordedFolderTrustDialog()
	require.True(t, claudeTrustPromptPresent(visibleTextOf(styledFolder)),
		"the gate must agree with its siblings")
	require.True(t, claudeFolderTrustDialogPresent(visibleTextOf(styledFolder)),
		"claudeFolderTrustDialogPresent already strips ANSI and detects this")

	styledMCP := styledMCPDialog()
	require.True(t, claudeTrustPromptPresent(visibleTextOf(styledMCP)))
	require.True(t, claudeMCPTrustFooterIsLast(visibleTextOf(styledMCP)),
		"claudeMCPTrustFooterIsLast already strips ANSI and detects this")
}

// TestCheckAndHandleTrustPrompt_StyledMCPDialogSendsEnter is the end-to-end
// proof for the MCP clause. The older MCP modal keeps the historical Enter tap;
// a styled render must still reach that Enter rather than the gate reporting
// the pane clear and the caller typing the prompt into the live modal.
func TestCheckAndHandleTrustPrompt_StyledMCPDialogSendsEnter(t *testing.T) {
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, styledMCPDialog())
	require.True(t, handled, "the styled MCP modal is in the way")
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
		"the styled MCP modal must still fire Enter, not let the prompt through")
}

// TestCheckAndHandleTrustPrompt_StyledMCPDialogQuotedInjectsNothing is the
// end-to-end negative for the MCP clause: a styled phrase with the composer
// painted below must not fire Enter. Stripping the question half is not enough
// to cause a false positive because the footer-is-last guard still holds.
func TestCheckAndHandleTrustPrompt_StyledMCPDialogQuotedInjectsNothing(t *testing.T) {
	handled, cmds := pollStaticPane(t, styledMCPDialogQuotedAboveComposer(), 4)
	require.False(t, handled,
		"a styled MCP phrase quoted above the composer is not a live modal")
	require.Empty(t, sentKeystrokes(cmds),
		"no key may be injected into a pane that only quotes the styled MCP phrase; got %v", cmds)
}

// TestCheckAndHandleTrustPrompt_StyledFolderTrustDialogCommitsAffirmative is
// the end-to-end proof for the folder-trust clause: a per-word styled live
// picker must be detected, navigated by label, and the affirmative option
// confirmed — never the prompt typed into the modal. The synthetic pane styles
// the question and the selected row word by word, the way the measured 2.1.322
// capture styles its header and its ❯ row, so the style follows the cursor.
func TestCheckAndHandleTrustPrompt_StyledFolderTrustDialogCommitsAffirmative(t *testing.T) {
	for _, tt := range []struct {
		name string
		pane *claudeFolderTrustPane
	}{
		{
			name: "styled, No first preselected — af must move to Yes and confirm",
			pane: &claudeFolderTrustPane{
				options:  []string{claudeTrustNoLabel, claudeTrustYesLabel},
				selected: 0,
				styled:   true,
			},
		},
		{
			name: "styled, Yes first preselected — af confirms immediately",
			pane: &claudeFolderTrustPane{
				options:  []string{claudeTrustYesLabel, claudeTrustNoLabel},
				selected: 0,
				styled:   true,
			},
		},
		{
			name: "styled and boxed — frame chrome composes with per-word SGR",
			pane: &claudeFolderTrustPane{
				options:  []string{claudeTrustNoLabel, claudeTrustYesLabel},
				selected: 0,
				styled:   true,
				boxed:    true,
			},
		},
		{
			name: "styled and numbered — ordinals compose with per-word SGR",
			pane: &claudeFolderTrustPane{
				options:  []string{claudeTrustNoLabel, claudeTrustYesLabel},
				selected: 0,
				styled:   true,
				ordinals: true,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			keys := driveClaudeTrustPane(t, tt.pane, 6)
			require.Equal(t, []string{claudeTrustYesLabel}, tt.pane.committedLabels(),
				"af must confirm the affirmative option on the styled dialog; keystrokes were %v", keys)
		})
	}
}

// TestCheckAndHandleTrustPrompt_GateHoldsStyledFolderTrustDialogNotClear is the
// watch-the-bug-fail assertion: with the raw (un-fixed) clauses the same styled
// pane reproduces the report's fail-open — the gate reports false and
// CheckAndHandleTrustPrompt returns false with zero keystrokes, meaning the
// caller would type the prompt into the live modal. After the fix the styled
// dialog is held (true) and driven.
func TestCheckAndHandleTrustPrompt_GateHoldsStyledFolderTrustDialogNotClear(t *testing.T) {
	advance := setClaudeTrustClock(t)
	pane := &claudeFolderTrustPane{
		options:  []string{claudeTrustNoLabel, claudeTrustYesLabel},
		selected: 0,
		styled:   true,
	}
	session, keys := claudeTrustSession(t, pane)

	// First poll: the dialog was just seen, so af holds without typing.
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"a styled dialog af has only just seen is still in the way — the gate must NOT read it as clear")
	require.Empty(t, injectedKeyNames(*keys),
		"no key before the settle delay; got %v", *keys)

	// Settled: af drives the styled picker to the affirmative row and confirms.
	advance(claudeTrustSettleDelay)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(*keys),
		"af must navigate the styled dialog by label, not report it clear and let the prompt through")
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels())
}

// TestCheckAndHandleTrustPrompt_StyledFolderTrustDialogQuotedInjectsNothing
// is the end-to-end negative for the folder-trust clause: a styled dialog
// quoted with the composer below sends no key, and — since #7271 reads the
// dialog from the trailing region — reports the pane dialog-free, as the
// unstyled QuotedClaudeFolderTrustDialogInjectsNothing does. Stripping ANSI
// must not widen the gate to fire on quoted output that has content painted
// after the footer.
func TestCheckAndHandleTrustPrompt_StyledFolderTrustDialogQuotedInjectsNothing(t *testing.T) {
	dialog := styledRewordedFolderTrustDialog()
	quoted := dialog + "\n╭────────────────────────────────────╮\n" +
		"│ > Type your message here           │\n" +
		"╰────────────────────────────────────╯\n? for shortcuts\n"
	handled, cmds := pollStaticPane(t, quoted, 4)
	require.False(t, handled,
		"the styled dialog's text with the composer painted below it is an observed dialog-free pane (#7271)")
	require.Empty(t, sentKeystrokes(cmds),
		"no key may be injected into a pane that only quotes the styled dialog; got %v", cmds)
}

// TestCheckAndHandleTrustPrompt_RealMCPDialogFromMeasuredFixtureDetected feeds
// the measured 2.1.322 MCP-server capture (the per-word styled .ansi file)
// through the gate. Its clause stripped per row before #7265 too, so this pins
// the gate on measured bytes rather than reproducing that fix's bug.
func TestCheckAndHandleTrustPrompt_RealMCPDialogFromMeasuredFixtureDetected(t *testing.T) {
	content := readTrustFixture(t, mcpServerDialogFixture)
	// The 2.1.322 dialog is the newer MCP-server wording (no "do you trust
	// this new mcp server" question). It is detected by the mcpServer clause,
	// not the older mcpDialog clause; both read the same visible text.
	// The gate must report present on the real captured styled bytes.
	require.True(t, claudeTrustPromptPresent(visibleTextOf(content)),
		"the real measured styled MCP-server capture must be detected by the gate")
	require.True(t, claudeFolderTrustDialogPresent(visibleTextOf(content)) || claudeMCPServerDialogOf(visibleTextOf(content)).verdict == claudeMCPServerComplete,
		"a sibling already detects the measured capture")
}
