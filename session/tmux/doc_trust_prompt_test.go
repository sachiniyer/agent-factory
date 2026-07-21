package tmux

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"
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

const codexSafetyBufferingDialog = `  Additional safety checks
  This request requires additional safety checks, which can take extra time.
  Hang tight or retry with a faster model for a quicker response, though it
  may be less capable of handling complex requests.

› 1. Retry with a faster model
  2. Keep waiting
  3. Learn more
  Press enter to confirm or esc to go back`

const codexSafetyBufferingKeepWaitingSelected = `  Additional safety checks
  This request requires additional safety checks, which can take extra time.
  Hang tight or retry with a faster model for a quicker response, though it
  may be less capable of handling complex requests.

  1. Retry with a faster model
› 2. Keep waiting
  3. Learn more
  Press enter to confirm or esc to go back`

const codexCurrentSafetyBufferingDialog = `  Our systems are thinking a bit more about this request before responding.
  Hang tight or retry with a faster model for a quicker response, though it
  may be less capable of handling complex requests.

› 1. Retry with a faster model
  2. Dismiss and keep waiting
  3. Learn more
  No action is required. Codex will keep waiting, and this menu will close when
  the response is ready.`

const codexCurrentSafetyBufferingWaitSelected = `  Our systems are thinking a bit more about this request before responding.
  Hang tight or retry with a faster model for a quicker response, though it
  may be less capable of handling complex requests.

  1. Retry with a faster model
› 2. Dismiss and keep waiting
  3. Learn more
  No action is required. Codex will keep waiting, and this menu will close when
  the response is ready.`

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

// runTrustPromptSequence feeds one pane capture per Output call while keeping
// one TmuxSession alive across checks. The safety-buffering path is stateful in
// production: it remembers the model footer from a normal poll, navigates and
// re-captures the picker, then verifies the model on a later poll.
func runTrustPromptSequence(t *testing.T, program string, contents ...string) (*TmuxSession, *[]string) {
	t.Helper()
	var (
		cmds       []string
		captureIdx int
	)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			cmds = append(cmds, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			require.Less(t, captureIdx, len(contents), "unexpected extra pane capture")
			content := contents[captureIdx]
			captureIdx++
			return []byte(content), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), program, NewMockPtyFactory(t), cmdExec)
	return session, &cmds
}

func captureTrustPromptLogs(t *testing.T) (info, warnings, errors *bytes.Buffer) {
	t.Helper()
	info, warnings, errors = &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	previousInfo := aflog.InfoLog.Writer()
	previousWarnings := aflog.WarningLog.Writer()
	previousErrors := aflog.ErrorLog.Writer()
	aflog.InfoLog.SetOutput(info)
	aflog.WarningLog.SetOutput(warnings)
	aflog.ErrorLog.SetOutput(errors)
	t.Cleanup(func() {
		aflog.InfoLog.SetOutput(previousInfo)
		aflog.WarningLog.SetOutput(previousWarnings)
		aflog.ErrorLog.SetOutput(previousErrors)
	})
	return info, warnings, errors
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

// Regression for #2181. The daemon's live Snapshot poll calls the real
// CheckAndHandleTrustPrompt every second. Codex 0.144.6 starts this picker on
// row 1. Delivering "2" as prompt text can leave that cursor untouched, after
// which the delivery path's trailing Enter accepts the downgrade. The
// unattended path must navigate by the literal target label, prove that row is
// selected before accepting it, and compare the model footer after the modal
// closes.
func TestCheckAndHandleTrustPrompt_CodexSafetyBufferingKeepsModel(t *testing.T) {
	info, warnings, _ := captureTrustPromptLogs(t)

	const normalPane = `• Working

  gpt-5.6-sol max · ~/agent-factory`
	session, commands := runTrustPromptSequence(t, "/usr/local/bin/codex --profile work",
		normalPane,
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		normalPane,
	)

	require.False(t, session.CheckAndHandleTrustPrompt(), "a normal Codex pane is not a modal")
	require.True(t, session.CheckAndHandleTrustPrompt(), "the safety-buffering picker must be handled")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands), "navigate to the label, verify it, then accept; never type an ordinal")
	require.NotContains(t, strings.Join(sentKeystrokes(*commands), "\n"), " 2",
		"a number key can submit Codex's current cursor row instead of selecting that numbered row")
	require.False(t, session.CheckAndHandleTrustPrompt(), "verification observes; it does not inject another key")
	require.Contains(t, warnings.String(), "additional safety checks")
	require.Contains(t, info.String(), "verified model unchanged: gpt-5.6-sol max")
}

func TestCheckAndHandleTrustPrompt_CodexSafetyBufferingAcceptsVisibleModelFooter(t *testing.T) {
	const modelFooter = "  gpt-5.6-sol max · ~/agent-factory"
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		modelFooter,
		codexSafetyBufferingDialog+"\n\n"+modelFooter,
		codexSafetyBufferingKeepWaitingSelected+"\n\n"+modelFooter,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the picker remains active when Codex renders its normal model footer beneath it")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
}

func TestCheckAndHandleTrustPrompt_CodexSafetyBufferingIgnoresNumberedTranscript(t *testing.T) {
	const transcript = `• Earlier output included an unrelated list:

  1. inspect the request
  2. explain the result

`
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		"gpt-5.6-sol max · ~/agent-factory",
		transcript+codexSafetyBufferingDialog,
		transcript+codexSafetyBufferingKeepWaitingSelected,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"numbered transcript lines above the active modal are not picker options")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
}

func TestCheckAndHandleTrustPrompt_CodexSafetyBufferingFindsLabelAfterReorder(t *testing.T) {
	const reordered = `  Additional safety checks
  This request requires additional safety checks, which can take extra time.
  Hang tight or retry with a faster model for a quicker response, though it may be less capable of handling complex requests.

› 1. Retry with a faster model
  2. Learn more
  3. Keep waiting
  Press enter to confirm or esc to go back`
	const waitSelected = `  Additional safety checks
  This request requires additional safety checks, which can take extra time.
  Hang tight or retry with a faster model for a quicker response, though it may be less capable of handling complex requests.

  1. Retry with a faster model
  2. Learn more
› 3. Keep waiting
  Press enter to confirm or esc to go back`
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		"gpt-5.6-sol max · ~/agent-factory",
		reordered,
		waitSelected,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands), "the label moved to row 3, so navigation must move with it")
}

func TestCheckAndHandleTrustPrompt_CurrentCodexSafetyWording(t *testing.T) {
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		"gpt-5.6-sol max · ~/agent-factory",
		codexCurrentSafetyBufferingDialog,
		codexCurrentSafetyBufferingWaitSelected,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
}

func TestCheckAndHandleTrustPrompt_CodexSafetyModelDowngradeIsSurfaced(t *testing.T) {
	_, _, errors := captureTrustPromptLogs(t)
	session, _ := runTrustPromptSequence(t, ProgramCodex,
		"gpt-5.6-sol max · ~/agent-factory",
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		"gpt-5.6-luna low · ~/agent-factory",
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.False(t, session.CheckAndHandleTrustPrompt())
	require.Contains(t, errors.String(), `model changed after additional safety checks: before "gpt-5.6-sol max", after "gpt-5.6-luna low"`)
}

func TestCheckAndHandleTrustPrompt_CodexChangedSafetyPickerIsSurfacedWithoutInput(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	changed := strings.Replace(codexSafetyBufferingDialog, "2. Keep waiting", "2. Continue waiting", 1)
	handled, commands := runTrustPromptCheck(t, ProgramCodex, changed)

	require.False(t, handled, "an unknown action label must stay for a human instead of guessing")
	require.Empty(t, sentKeystrokes(commands))
	require.Contains(t, warnings.String(), "could not safely select \"Keep waiting\"")
}

func TestCheckAndHandleTrustPrompt_CodexSafetyFixtureInOutputInjectsNothing(t *testing.T) {
	content := codexSafetyBufferingDialog + `

The block above is a fixture quoted in source, not an active picker.`
	handled, commands := runTrustPromptCheck(t, ProgramCodex, content)

	require.False(t, handled)
	require.Empty(t, sentKeystrokes(commands), "continued output after the modal footer proves the picker is not active")
}

func TestCheckAndHandleTrustPrompt_CodexSafetyDialogIsCodexOnly(t *testing.T) {
	for _, program := range []string{ProgramClaude, ProgramAider, ProgramGemini, ProgramAmp, ProgramOpencode} {
		handled, commands := runTrustPromptCheck(t, program, codexSafetyBufferingDialog)
		require.False(t, handled, "%s must not inherit Codex picker behavior", program)
		require.Empty(t, sentKeystrokes(commands), "%s received Codex-only input", program)
	}
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
