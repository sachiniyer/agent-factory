package tmux

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// The non-Claude branch of CheckAndHandleTrustPrompt dismisses the aider/gemini
// documentation-link trust dialog by injecting 'D'+Enter into a LIVE agent pane.
// It runs on the daemon's continuous 1-second poll against a visible-only
// capture, so `content` is whatever the agent has on screen right now — its own
// output, a log line, a file it cat'd, a diff of the issue describing this bug.
//
// These tests drive the real CheckAndHandleTrustPrompt, NOT the predicate: the
// injection is gated in production on DetectAgentFromCommand(t.programCmd()) !=
// ProgramClaude, and a test that called the predicate directly would hop over
// that gate and prove nothing about the path that actually types into the pane
// (#1952).

// Real aider doc-trust dialog: the prose line co-occurs with the dialog-only
// "(D)on't ask again" affordance.
const aiderDocTrustDialog = `Add https://aider.chat/docs/faq.html to the chat?
Open documentation url for more info
(Y)es/(N)o/(D)on't ask again [Yes]:`

// The SAME prose line as ordinary agent output — no dialog, nothing to dismiss.
// This is the #1952 repro: an agent that merely mentions the phrase must not be
// typed into.
const aiderDocPhraseInOutput = `I read through the aider docs to answer your question.
The CLI prints "Open documentation url for more info" when it wants to open a
link. Here is the summary you asked for, with no prompt waiting.`

const codexDirectoryTrustDialog = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit

  Press enter to continue`

// runTrustPromptCheck drives CheckAndHandleTrustPrompt for a pane running
// `program` and showing `content`, returning its verdict plus every tmux
// command it issued.
func runTrustPromptCheck(t *testing.T, program, content string) (handled bool, cmds []string) {
	t.Helper()
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			cmds = append(cmds, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return []byte(content), nil },
	}
	session := newTmuxSession(toTmuxName("trust", ""), program, NewMockPtyFactory(t), cmdExec)
	return session.CheckAndHandleTrustPrompt(), cmds
}

// sentKeystrokes returns the send-keys commands issued, i.e. the input actually
// injected into the user's agent.
func sentKeystrokes(cmds []string) []string {
	var keys []string
	for _, c := range cmds {
		if strings.Contains(c, "send-keys") {
			keys = append(keys, c)
		}
	}
	return keys
}

func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogIsDismissed(t *testing.T) {
	handled, cmds := runTrustPromptCheck(t, ProgramCodex, codexDirectoryTrustDialog)
	require.True(t, handled, "the observed Codex directory-trust dialog must be reported handled")
	require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: Enter",
		"the selected 'Yes, continue' option must be accepted before briefing delivery; got %v", cmds)
}

func TestCheckAndHandleTrustPrompt_CodexTrustProseAloneInjectsNothing(t *testing.T) {
	fragments := []string{
		"Do you trust the contents of this directory?",
		"Do you trust the contents of this directory?\n› 1. Yes, continue",
		"Yes, continue\n2. No, quit\nPress enter to continue",
	}
	for _, content := range fragments {
		handled, cmds := runTrustPromptCheck(t, ProgramCodex, content)
		require.False(t, handled, "a partial/modal prose fragment is not the anchored Codex dialog")
		require.Empty(t, sentKeystrokes(cmds),
			"a continuous live-session poll must not inject Enter from partial trust prose; got %v", cmds)
	}
}

func TestCheckAndHandleTrustPrompt_CodexModalSourceInOutputInjectsNothing(t *testing.T) {
	content := codexDirectoryTrustDialog + `

func CodexTrustPromptPresent(content string) bool {
    // The agent is displaying source after quoting the fixture above.
    return false
}`
	handled, cmds := runTrustPromptCheck(t, ProgramCodex, content)
	require.False(t, handled,
		"four phrases somewhere in ordinary output are not proof the selected modal is active")
	require.Empty(t, sentKeystrokes(cmds),
		"the live-session poll must not inject Enter into ordinary output containing the fixture; got %v", cmds)
}

func TestCheckAndHandleTrustPrompt_CodexUnselectedYesInjectsNothing(t *testing.T) {
	content := strings.Replace(codexDirectoryTrustDialog, "› 1. Yes, continue", "  1. Yes, continue", 1)
	handled, cmds := runTrustPromptCheck(t, ProgramCodex, content)
	require.False(t, handled, "the predicate must require the actual selected Yes row")
	require.Empty(t, sentKeystrokes(cmds))
}

// The bug: ordinary agent output that happens to contain the documentation
// phrase must NOT get keystrokes injected into it.
func TestCheckAndHandleTrustPrompt_DocPhraseInOutputInjectsNothing(t *testing.T) {
	for _, program := range []string{ProgramAider, ProgramGemini} {
		t.Run(program, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, program, aiderDocPhraseInOutput)
			// Assert the INJECTION first: that is the harm #1952 reports. The
			// bool is secondary (and is discarded by the daemon poll anyway).
			require.Empty(t, sentKeystrokes(cmds),
				"#1952: no keystroke may be injected into a running agent on the strength of a "+
					"prose phrase alone — a false positive types into the user's session and can modify files; got %v", cmds)
			require.False(t, handled,
				"the bare documentation phrase in ordinary output is not a trust dialog")
		})
	}
}

// The other direction: the real dialog must still be dismissed, or the fix is
// half-tested and startup hangs at the prompt.
func TestCheckAndHandleTrustPrompt_RealDocDialogIsDismissed(t *testing.T) {
	for _, program := range []string{ProgramAider, ProgramGemini} {
		t.Run(program, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, program, aiderDocTrustDialog)
			require.True(t, handled, "the real doc-trust dialog must be reported handled")
			require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: D Enter",
				"the real doc-trust dialog is dismissed with 'D'+Enter; got %v", cmds)
		})
	}
}

// The dialog marker alone is not the doc-trust dialog: aider renders
// "(D)on't ask again" on every confirmation prompt it asks. Dismissing an
// arbitrary confirmation with 'D' ("don't ask again") is exactly the unbidden
// action this gate exists to prevent.
func TestCheckAndHandleTrustPrompt_BareDialogMarkerInjectsNothing(t *testing.T) {
	content := `Add src/main.go to the chat?
(Y)es/(N)o/(D)on't ask again [Yes]:`
	handled, cmds := runTrustPromptCheck(t, ProgramAider, content)
	require.False(t, handled, "a confirmation prompt that is not the doc-trust dialog must not be auto-dismissed")
	require.Empty(t, sentKeystrokes(cmds), "got %v", cmds)
}

// The marker is NOT a sufficient second anchor on its own, because aider prints
// "(D)on't ask again" on EVERY confirmation it asks (see the test above). So the
// only thing separating the doc-trust dialog from every other aider prompt is the
// full prose question. A predicate that requires the marker plus a mere PREFIX of
// the prose is satisfied by: an unrelated confirmation that is up right now, plus
// the shorter string sitting anywhere on screen — in ordinary output, or in a
// source file the agent has open.
//
// That combination answers 'D' ("don't ask again") to a question we never read.
// This repo is self-hosted, so the shorter string is literally on screen whenever
// an agent has this file or task/runner.go open — which is how a real aider
// session hits it.
func TestCheckAndHandleTrustPrompt_DocPrefixPlusUnrelatedConfirmInjectsNothing(t *testing.T) {
	// The agent has this very file open (the short string is source text here),
	// and aider independently asks an ordinary confirmation.
	content := `	return strings.Contains(content, "Open documentation url") &&
		strings.Contains(content, "(D)on't ask again")

Add src/main.go to the chat?
(Y)es/(N)o/(D)on't ask again [Yes]:`

	for _, program := range []string{ProgramAider, ProgramGemini} {
		t.Run(program, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, program, content)
			require.Empty(t, sentKeystrokes(cmds),
				"the doc-trust prose PREFIX plus a marker that every aider confirmation "+
					"renders is not evidence the doc dialog is up — tapping 'D' here answers an "+
					"unrelated question; got %v", cmds)
			require.False(t, handled)
		})
	}
}

// The Claude branch is a separate gate and must keep its own behavior: the doc
// phrase never routes a claude pane into the 'D'+Enter path.
func TestCheckAndHandleTrustPrompt_ClaudeBranchUnaffectedByDocPhrase(t *testing.T) {
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, aiderDocTrustDialog)
	require.False(t, handled, "the doc-trust dialog is not a Claude launch gate")
	require.Empty(t, sentKeystrokes(cmds),
		"a claude pane must never take the 'D'+Enter path; got %v", cmds)
}
