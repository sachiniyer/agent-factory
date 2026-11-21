package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
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

// Real aider doc-trust dialog: offer_url renders its URL subject in reverse
// video before the caller-controlled question and confirmation suffix.
const aiderDocTrustDialog = "\x1b[7mhttps://aider.chat/docs/faq.html\n" +
	"\x1b[0m\x1b[38;5;21mOpen documentation url for more info? " +
	"(Y)es/(N)o/(D)on't ask again [Yes]:  \x1b[39m\n\x1b[38;5;21m   \x1b[39m"

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

// A capture taken while Codex has cleared its menu but has not finished
// repainting it. The footer that makes the pane recognizable as the picker is
// not on screen yet, so this frame matches no Codex shape at all — it is
// neither the picker nor the ordinary composer.
const codexCurrentSafetyBufferingPartialRedraw = `  Our systems are thinking a bit more about this request before responding.
  Hang tight or retry with a faster model for a quicker response, though it
  may be less capable of handling complex requests.

› 1. Retry with a faster model
  2. Dismiss and keep waiting
  3. Learn more`

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
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return []byte("0 0 0"), nil
			}
			return []byte(content), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), program, NewMockPtyFactory(t), cmdExec)
	return session.CheckAndHandleTrustPrompt(), cmds
}

// trustPromptFrame is one pane capture paired with the cursor state tmux would
// report while it is on screen. The pair is what the safety-picker path reads:
// Codex's ListSelectionView implements no cursor position and so hides the
// terminal cursor, while the ordinary composer exposes one. Cursor visibility is
// therefore the only POSITIVE evidence that the modal is gone — the absence of
// its chrome is not, because a mid-repaint capture also has none (#2760).
type trustPromptFrame struct {
	content       string
	cursorVisible bool
}

// runTrustPromptSequence feeds one pane capture per Output call while keeping
// one TmuxSession alive across checks. The safety-buffering path is stateful in
// production: it remembers the model footer from a normal poll, navigates and
// re-captures the picker, then verifies the model on a later poll.
//
// Every frame reports a hidden cursor, matching a pane the picker owns. Use
// runTrustPromptFrames when a test needs the composer's visible cursor back.
func runTrustPromptSequence(t *testing.T, program string, contents ...string) (*TmuxSession, *[]string) {
	t.Helper()
	frames := make([]trustPromptFrame, len(contents))
	for idx, content := range contents {
		frames[idx] = trustPromptFrame{content: content}
	}
	return runTrustPromptFrames(t, program, frames...)
}

// runTrustPromptFrames is runTrustPromptSequence with per-capture cursor state.
// A frame's cursor answer stays in force until the next capture, matching
// production order: the pane is captured first, then queried for its cursor.
func runTrustPromptFrames(t *testing.T, program string, frames ...trustPromptFrame) (*TmuxSession, *[]string) {
	t.Helper()
	var (
		cmds       []string
		captureIdx int
		cursor     = "0 0 0"
	)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			cmds = append(cmds, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return []byte(cursor), nil
			}
			require.Less(t, captureIdx, len(frames), "unexpected extra pane capture")
			frame := frames[captureIdx]
			captureIdx++
			cursor = "0 0 0"
			if frame.cursorVisible {
				cursor = "0 0 1"
			}
			return []byte(frame.content), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), program, NewMockPtyFactory(t), cmdExec)
	return session, &cmds
}

func captureTrustPromptLogs(t *testing.T) (info, warnings, errors *logtest.Buffer) {
	t.Helper()
	info, warnings, errors = &logtest.Buffer{}, &logtest.Buffer{}, &logtest.Buffer{}
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

func TestCheckAndHandleTrustPrompt_CodexSafetyWaitsForDelayedSelectionRender(t *testing.T) {
	_, _, errors := captureTrustPromptLogs(t)
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		normalPane,
		codexCurrentSafetyBufferingDialog,
		codexCurrentSafetyBufferingDialog, // immediate post-navigation capture is stale
		codexCurrentSafetyBufferingWaitSelected,
		normalPane,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"a stale post-navigation frame must leave the picker pending")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
	}, sentKeystrokes(*commands),
		"Enter must wait until a later pane capture proves the literal target row is selected")
	require.Empty(t, errors.String(),
		"normal terminal rendering lag is not a failed safety-picker operation")

	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the next daemon poll observes the delayed selected frame and accepts it")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the following poll verifies the unchanged model")
	require.Empty(t, errors.String(),
		"a recovered selection-render delay must not leave a false ERROR behind")
}

// A partial redraw matches no Codex shape at all, so it is not evidence that
// the picker closed. Dropping the pending selection there lets the NEXT
// complete-but-stale frame re-enter navigation and send the same Down a second
// time: two rows of movement for one intended step, which parks the cursor on
// "Learn more" and then accepts it — the overshoot this whole path exists to
// prevent (#2181).
func TestCheckAndHandleTrustPrompt_CodexSafetyPartialRedrawDoesNotRepeatNavigation(t *testing.T) {
	info, _, errors := captureTrustPromptLogs(t)
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		normalPane,
		codexCurrentSafetyBufferingDialog,
		codexCurrentSafetyBufferingPartialRedraw, // immediate post-navigation capture, mid-repaint
		codexCurrentSafetyBufferingDialog,        // chrome is back, selection still stale
		codexCurrentSafetyBufferingWaitSelected,
		normalPane,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"a mid-repaint frame leaves the picker pending, not resolved")
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
	}, sentKeystrokes(*commands),
		"one intended row of movement must be sent exactly once, however the pane repaints")

	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the target row finally renders as selected and is accepted")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
	require.False(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, errors.String())
	require.Contains(t, info.String(), "verified model unchanged: gpt-5.6-sol max")
}

// The counterpart: Codex closes the picker itself ("this menu will close when
// the response is ready"). The composer's visible cursor is the positive signal
// that releases the pending selection, so a picker that resolves under af still
// hands the session straight back.
func TestCheckAndHandleTrustPrompt_CodexSafetyReleasesPendingSelectionWhenPickerCloses(t *testing.T) {
	info, _, errors := captureTrustPromptLogs(t)
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	session, commands := runTrustPromptFrames(t, ProgramCodex,
		trustPromptFrame{content: normalPane},
		trustPromptFrame{content: codexCurrentSafetyBufferingDialog},
		trustPromptFrame{content: normalPane, cursorVisible: true},
		trustPromptFrame{content: normalPane, cursorVisible: true},
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
	}, sentKeystrokes(*commands), "Enter must never reach a picker that closed itself")
	require.Contains(t, info.String(), "resolved before af accepted a choice")
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the released session is an ordinary pane again")
	require.Empty(t, errors.String())
}

// The verification budget is a duration, not a capture count. daemon_poll_interval
// is operator-configurable, so counting polls makes the budget mean whatever the
// operator set: at 100ms a three-capture budget expires before a 500ms repaint can
// land, reporting ordinary rendering lag as a failed safety operation.
func TestCheckAndHandleTrustPrompt_CodexSafetyBudgetsRenderTimeNotPollCount(t *testing.T) {
	_, _, errors := captureTrustPromptLogs(t)
	clock := fakeCodexSafetyClock(t)
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		normalPane,
		codexCurrentSafetyBufferingDialog,
		codexCurrentSafetyBufferingDialog, // immediate post-navigation capture
		codexCurrentSafetyBufferingDialog, // fast daemon polls, all inside the window
		codexCurrentSafetyBufferingDialog,
		codexCurrentSafetyBufferingDialog,
		codexCurrentSafetyBufferingWaitSelected,
		normalPane,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	for poll := 0; poll < 3; poll++ {
		*clock = clock.Add(100 * time.Millisecond)
		require.True(t, session.CheckAndHandleTrustPrompt())
		require.Empty(t, errors.String(),
			"a 100ms daemon_poll_interval must not spend the render budget in %d polls", poll+1)
	}

	*clock = clock.Add(500 * time.Millisecond)
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the repaint the real fixture exercises lands well inside the window")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
	require.False(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, errors.String())
}

func TestCheckAndHandleTrustPrompt_CodexSafetyEscalatesUnverifiedSelection(t *testing.T) {
	info, _, errors := captureTrustPromptLogs(t)
	clock := fakeCodexSafetyClock(t)
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		normalPane,
		codexCurrentSafetyBufferingDialog,
		codexCurrentSafetyBufferingDialog, // immediate post-navigation capture
		codexCurrentSafetyBufferingDialog, // first later daemon poll
		codexCurrentSafetyBufferingDialog, // bounded render window expires
		codexCurrentSafetyBufferingWaitSelected,
		normalPane,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, errors.String(),
		"one stale render after navigation is expected and remains pending")
	*clock = clock.Add(codexSafetySelectionVerificationWindow - time.Millisecond)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, errors.String(),
		"the bounded verification window must not escalate early")
	*clock = clock.Add(time.Millisecond)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Contains(t, errors.String(),
		`could not verify "Dismiss and keep waiting" as the selected row after 5s`,
		"a picker that never renders the safe selection is a real operator-visible failure")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
	}, sentKeystrokes(*commands),
		"af must fail closed without repeating navigation or pressing Enter")

	// Escalation is a report, not a terminal state. A picker that finally paints
	// the safe row is still accepted, and the operator gets the correction next
	// to the ERROR rather than being left with a warning that never resolved.
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Contains(t, info.String(), "selection became verifiable")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
	require.False(t, session.CheckAndHandleTrustPrompt())
}

// fakeCodexSafetyClock freezes the selection-verification clock and returns the
// handle a test advances to model daemon polls arriving at a given interval.
func fakeCodexSafetyClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	previous := codexSafetyNow
	codexSafetyNow = func() time.Time { return now }
	t.Cleanup(func() { codexSafetyNow = previous })
	return &now
}

func TestCheckAndHandleTrustPrompt_CodexSafetyModelDowngradeIsSurfaced(t *testing.T) {
	_, _, errors := captureTrustPromptLogs(t)
	session, _ := runTrustPromptSequence(t, ProgramCodex,
		"gpt-5.6-sol max · ~/agent-factory",
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		"gpt-5.6-luna low · ~/agent-factory",
		"gpt-5.6-luna low · ~/agent-factory",
		"gpt-5.6-sol max · ~/agent-factory",
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.False(t, session.CheckAndHandleTrustPrompt())
	require.Contains(t, errors.String(), `model changed after additional safety checks: before "gpt-5.6-sol max", after "gpt-5.6-luna low"`)
	before, after, changed := session.CodexSafetyModelChange()
	require.True(t, changed, "a logged model change must survive as observable session state")
	require.Equal(t, "gpt-5.6-sol max", before)
	require.Equal(t, "gpt-5.6-luna low", after)

	require.False(t, session.CheckAndHandleTrustPrompt())
	before, after, changed = session.CodexSafetyModelChange()
	require.True(t, changed, "ordinary polls on the changed model must retain the warning")
	require.Equal(t, "gpt-5.6-sol max", before)
	require.Equal(t, "gpt-5.6-luna low", after)

	require.False(t, session.CheckAndHandleTrustPrompt())
	_, _, changed = session.CodexSafetyModelChange()
	require.False(t, changed, "seeing the original model again resolves the live warning")
}

func TestCodexSafetyModelChangeClearsWhenRuntimeIsReplaced(t *testing.T) {
	session, _ := runTrustPromptSequence(t, ProgramCodex,
		"gpt-5.6-sol max · ~/agent-factory",
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		"gpt-5.6-luna low · ~/agent-factory",
	)
	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.False(t, session.CheckAndHandleTrustPrompt())
	_, _, changed := session.CodexSafetyModelChange()
	require.True(t, changed)

	forceNewSessionEnvMarkers(t, false)
	session.ptyFactory = failingPtyFactory{}
	session.cmdExec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	require.Error(t, session.Start(t.TempDir()))
	_, _, changed = session.CodexSafetyModelChange()
	require.False(t, changed, "a replacement process must not inherit the old runtime's warning")
}

func TestCheckAndHandleTrustPrompt_CodexChangedSafetyPickerIsSurfacedWithoutInput(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	changed := strings.Replace(codexSafetyBufferingDialog, "2. Keep waiting", "2. Continue waiting", 1)
	handled, commands := runTrustPromptCheck(t, ProgramCodex, changed)

	require.True(t, handled,
		"an active picker that af cannot answer must block startup prompt delivery")
	require.Empty(t, sentKeystrokes(commands))
	require.Contains(t, warnings.String(), "could not safely select \"Keep waiting\"")
}

func TestCheckAndHandleTrustPrompt_CodexSafetyPickerStillVisibleBlocksDelivery(t *testing.T) {
	session, _ := runTrustPromptSequence(t, ProgramCodex,
		"gpt-5.6-sol max · ~/agent-factory",
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		codexSafetyBufferingKeepWaitingSelected,
	)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the accepted picker is still visible, so startup must not paste through it")
}

func TestCheckAndHandleTrustPrompt_QuotedCodexSafetyPickerWithComposerCursorInjectsNothing(t *testing.T) {
	const modelFooter = "  gpt-5.6-sol max · ~/agent-factory"
	content := codexSafetyBufferingKeepWaitingSelected + "\n\n" + modelFooter
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				// The ordinary Codex composer owns a visible cursor. The picker
				// text above is quoted transcript, not the active bottom pane.
				return []byte("20 0 1"), nil
			}
			return []byte(content), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramCodex, NewMockPtyFactory(t), cmdExec)

	require.False(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, sentKeystrokes(commands),
		"quoted picker text above a live composer must never receive navigation or Enter")
}

func TestCheckAndHandleTrustPrompt_SerializesPickerInputWithPromptDelivery(t *testing.T) {
	var (
		captureCount atomic.Int32
		pasted       atomic.Bool
		loadOnce     sync.Once
	)
	selectionCaptureStarted := make(chan struct{})
	releaseSelectionCapture := make(chan struct{})
	promptDeliveryStarted := make(chan struct{})

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			command := strings.Join(c.Args, " ")
			if strings.Contains(command, "load-buffer") {
				loadOnce.Do(func() { close(promptDeliveryStarted) })
			}
			if strings.Contains(command, "paste-buffer") {
				pasted.Store(true)
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			command := strings.Join(c.Args, " ")
			if strings.Contains(command, "display-message") {
				return []byte("0 0 0"), nil
			}
			switch captureCount.Add(1) {
			case 1:
				return []byte(codexSafetyBufferingDialog), nil
			case 2:
				close(selectionCaptureStarted)
				<-releaseSelectionCapture
				return []byte(codexSafetyBufferingKeepWaitingSelected), nil
			default:
				if pasted.Load() {
					return []byte("concurrent prompt"), nil
				}
				return nil, nil
			}
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramCodex, NewMockPtyFactory(t), cmdExec)

	handlerDone := make(chan bool, 1)
	go func() { handlerDone <- session.CheckAndHandleTrustPrompt() }()
	select {
	case <-selectionCaptureStarted:
	case <-time.After(time.Second):
		t.Fatal("picker handler did not reach its selection verification capture")
	}

	deliveryDone := make(chan error, 1)
	go func() { deliveryDone <- session.SendKeysCommand("concurrent prompt") }()
	interleaved := false
	select {
	case <-promptDeliveryStarted:
		interleaved = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSelectionCapture)

	select {
	case handled := <-handlerDone:
		require.True(t, handled)
	case <-time.After(time.Second):
		t.Fatal("picker handler did not finish")
	}
	select {
	case err := <-deliveryDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("prompt delivery did not finish")
	}
	require.False(t, interleaved,
		"prompt delivery started between picker selection and Enter")
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
	// Prose fragments that are NOT the directory-trust modal's selected row
	// or anything after it: the question alone, and the option labels plus
	// footer without the selection marker. Neither is the trailing shape a
	// live modal leaves mid-paint, so the live-session poll must never inject
	// Enter on either. The question followed by the selected Yes row — the
	// modal's own mid-paint shape — is a different, positive modal-owned
	// signal codexDirectoryTrustTrailingRows holds instead; see
	// TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintHolds.
	fragments := []string{
		"Do you trust the contents of this directory?",
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

// ef6c955e shipped CodexTrustPromptPresent as a pure text match. The dialog's
// prose ending the visible pane — quoted in logs/diffs or echoed while the
// composer is active — made the daemon's continuous poll inject Enter into a
// running agent. A visible terminal cursor proves no ListSelectionView modal
// owns the pane: Codex hides the cursor for modals, the composer shows one.
// This is the same guard inspectCodexSafetyPrompt already required (#3302).
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogAtEndWithVisibleCursorInjectsNothing(t *testing.T) {
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				// The ordinary Codex composer owns a visible cursor. The
				// directory-trust dialog text is quoted transcript, not an
				// active modal.
				return []byte("20 0 1"), nil
			}
			return []byte(codexDirectoryTrustDialog), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramCodex, NewMockPtyFactory(t), cmdExec)

	require.False(t, session.CheckAndHandleTrustPrompt(),
		"a visible cursor means the composer is active, not a directory-trust modal")
	require.Empty(t, sentKeystrokes(commands),
		"directory-trust text above a live composer must never receive Enter; got %v", commands)
}

// A cursor-read failure is not proof the modal is gone, so the directory-trust
// handler fails closed like inspectCodexSafetyPrompt: it reports the dialog as
// still in the way (true) WITHOUT injecting Enter, letting the next poll retry.
// Returning false would let task.DismissTrustPrompt type the user's prompt into
// a real modal af failed to confirm (#3302).
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogCursorReadErrorFailsClosed(t *testing.T) {
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return nil, errors.New("display-message refused")
			}
			return []byte(codexDirectoryTrustDialog), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramCodex, NewMockPtyFactory(t), cmdExec)

	require.True(t, session.CheckAndHandleTrustPrompt(),
		"a cursor-read error must be treated as a blocking dialog, not a dialog-free pane")
	require.Empty(t, sentKeystrokes(commands),
		"no Enter may be injected when the modal state cannot be confirmed; got %v", commands)
}

// Codex's directory-trust modal as rust-v0.156.0 on paints it — "Folder
// access", the question, "› 1. Trust and continue" / "  2. Quit", and the
// "enter continue · esc quit" footer — checked verbatim in upstream
// codex-rs/tui/src/onboarding/trust_directory.rs at rust-v0.156.1,
// rust-v0.160.0, and rust-v0.171.0.
const codexDirectoryTrustDialogNew = `  Folder access
  /tmp/af-home

  Trust this folder? Codex can read, edit, and run files here, subject to
  your permission settings. Folder settings can run code automatically,
  even without a model request. Continue only if you trust these files.
  Your trust decision will be saved.

› 1. Trust and continue
  2. Quit

  enter continue · esc quit`

// The same modal on a folder the user marked untrusted: the affirmative row
// becomes "Open restricted", which grants no trust, so af holds rather than
// answers it.
const codexDirectoryTrustDialogRestricted = `  Folder access
  /tmp/af-home

  This folder is in your untrusted list. Config, hooks, and exec policies
  from untrusted folders stay disabled. Your trust decision will be saved.

› 1. Open restricted
  2. Quit

  enter continue · esc quit`

// wrapTrustPaneForTest reflows text the way ratatui's wrap leaves it at a
// narrow pane width: words broken at spaces, each continuation back at the
// line's own indent.
func wrapTrustPaneForTest(text string, w int) string {
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		ws := raw[:indent]
		words := strings.Fields(raw)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ws + words[0]
		for _, word := range words[1:] {
			if len(line)+1+len(word) <= w {
				line += " " + word
			} else {
				out = append(out, line)
				line = ws + word
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// codexTrustPaintFrames returns the pane captures a directory-trust modal
// produces while it paints top-down at the given width: every prefix ending
// at or after the selected option row's "›" marker lands but before the
// modal's footer is complete. The gate must hold every one of them.
func codexTrustPaintFrames(t *testing.T, modal string, width int) []string {
	t.Helper()
	wrapped := wrapTrustPaneForTest(modal, width)
	selIdx := strings.Index(wrapped, "›")
	require.GreaterOrEqual(t, selIdx, 0, "test modal must paint a selected row")
	var frames []string
	for end := selIdx + len("›"); end < len(wrapped); end++ {
		frames = append(frames, wrapped[:end])
	}
	return frames
}

func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogNewWordingIsDismissed(t *testing.T) {
	handled, cmds := runTrustPromptCheck(t, ProgramCodex, codexDirectoryTrustDialogNew)
	require.True(t, handled, "the observed new-wording Codex directory-trust dialog must be reported handled")
	require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: Enter",
		"the selected 'Trust and continue' option must be accepted before briefing delivery; got %v", cmds)
}

// The directory-trust modal paints top-down, so a capture that lands after
// the selected option row's "›" marker is on screen but before the footer is
// whole ends inside the modal's remaining rows — the selected row partial or
// whole, the second-option row partial or whole, the blank between, or a
// partial footer. Every such frame declines the complete-modal predicate,
// and the "›" row's glyph is also Codex's composer glyph, so isReadyContent's
// hasCodexComposerLine arm (task/runner.go) accepts the very same frame:
// reporting it clear lets DismissTrustPrompt paste the briefing into the live
// modal and its trailing Enter select the preselected option — the #2220
// failure this branch exists to prevent.
//
// The hold covers both wordings upstream paints (0.144.6–0.155.x and
// rust-v0.156.0 on) at every paint stage and every width, including the
// "question followed by selected Yes row" fragment this file's prose test
// dropped and this test restores. It pairs the trailing-rows shape with the
// hidden terminal cursor Codex's ListSelectionView exposes — the same oracle
// the complete-modal branch and inspectCodexSafetyPrompt both rely on — and
// sends no key: Enter stays gated on the complete-modal predicate.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintHolds(t *testing.T) {
	// The fragment TestCheckAndHandleTrustPrompt_CodexTrustProseAloneInjectsNothing
	// previously carried as injects-nothing prose: the old modal's question
	// followed by its selected Yes row, the last content on screen. That is
	// the modal's own mid-paint shape, not arbitrary prose — the gate must
	// hold it and send no key.
	handled, cmds := runTrustPromptCheck(t, ProgramCodex,
		"Do you trust the contents of this directory?\n› 1. Yes, continue")
	require.True(t, handled,
		"the question followed by the selected Yes row is a live modal mid-paint, not clear pane")
	require.Empty(t, sentKeystrokes(cmds),
		"the hold path sends no key on the mid-paint fragment; got %v", cmds)

	modals := []struct{ name, text string }{
		{"old wording", codexDirectoryTrustDialog},
		{"new wording", codexDirectoryTrustDialogNew},
		{"restricted wording", codexDirectoryTrustDialogRestricted},
	}
	for _, modal := range modals {
		for _, width := range []int{12, 17, 40, 80} {
			frames := codexTrustPaintFrames(t, modal.text, width)
			for idx, frame := range frames {
				handled, cmds := runTrustPromptCheck(t, ProgramCodex, frame)
				require.True(t, handled,
					"%s modal mid-paint at %d cols (frame %d) must not be reported clear:\n%s",
					modal.name, width, idx, frame)
				require.Empty(t, sentKeystrokes(cmds),
					"%s modal mid-paint at %d cols (frame %d) must receive no key — Enter stays gated on the complete-modal predicate; got %v",
					modal.name, width, idx, cmds)
			}
		}
	}
}

// A pane too short for the directory-trust modal's question scrolls it off
// and leaves the settled option-and-footer tail as the last content. That
// tail positively marks the modal — the question is required only for the
// Enter the complete-modal branch sends — so the gate must hold it, and hold
// it WITHOUT dismissing: an Enter sent on the strength of the options alone
// would answer a modal af could not fully confirm. The restricted wording is
// held the same way whether or not its question survives.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogSettledTailHolds(t *testing.T) {
	frames := []struct{ name, content string }{
		{"old wording, question scrolled", "› 1. Yes, continue\n  2. No, quit\n\n  Press enter to continue"},
		{"old wording, options only", "› 1. Yes, continue\n  2. No, quit"},
		{"new wording, question scrolled", "› 1. Trust and continue\n  2. Quit\n\n  enter continue · esc quit"},
		{"new wording, options only", "› 1. Trust and continue\n  2. Quit"},
		{"restricted wording, settled", "› 1. Open restricted\n  2. Quit\n\n  enter continue · esc quit"},
	}
	for _, frame := range frames {
		t.Run(frame.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramCodex, frame.content)
			require.True(t, handled,
				"a settled directory-trust tail with a hidden cursor marks a modal-owned pane")
			require.Empty(t, sentKeystrokes(cmds),
				"the hold path sends no key — Enter stays gated on the complete-modal predicate; got %v", cmds)
		})
	}
}

// The directory-trust modal's rows can wrap at a narrow pane width. The
// complete-modal match must answer the same at every width — including the
// question that no longer fits on one line — because a dismissal Enter
// missed at 40 or 12 columns leaves the modal live through the same
// hasCodexComposerLine wedge the paint-hold covers.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogNarrowWidthsDismissed(t *testing.T) {
	modals := []struct{ name, text string }{
		{"old wording", codexDirectoryTrustDialog},
		{"new wording", codexDirectoryTrustDialogNew},
	}
	for _, modal := range modals {
		for _, width := range []int{12, 24, 40} {
			t.Run(fmt.Sprintf("%s at %d cols", modal.name, width), func(t *testing.T) {
				handled, cmds := runTrustPromptCheck(t, ProgramCodex,
					wrapTrustPaneForTest(modal.text, width))
				require.True(t, handled,
					"the complete %s modal must be dismissed however narrow the pane wraps it", modal.name)
				require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: Enter",
					"the selected trust option must be accepted at every width; got %v", cmds)
			})
		}
	}
}

// The trailing rows alone are not proof a modal owns the pane: the dialog
// text can sit verbatim in quoted agent output above an active composer. The
// ordinary Codex composer exposes a terminal cursor, the modal hides one, so
// a visible cursor must leave the create path clear to deliver the briefing —
// the same quoted-prose protection the complete-modal branch already keeps,
// preserved here by the hidden-cursor oracle that gates the hold. No key is
// sent on either path: a visible cursor fails open only by falling through to
// return false, never by sending a key.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintWithVisibleCursorInjectsNothing(t *testing.T) {
	const quotedAboveComposer = `Earlier output quoted the modal text:

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit`

	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				// The ordinary Codex composer owns a visible cursor. The
				// directory-trust dialog text is quoted transcript above an
				// active composer, not a live modal mid-paint.
				return []byte("20 0 1"), nil
			}
			return []byte(quotedAboveComposer), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramCodex, NewMockPtyFactory(t), cmdExec)

	require.False(t, session.CheckAndHandleTrustPrompt(),
		"a visible cursor means the composer is active, not a directory-trust modal mid-paint")
	require.Empty(t, sentKeystrokes(commands),
		"quoted option rows above a live composer must never receive Enter; got %v", commands)
}

// A cursor-read failure on a partial frame fails closed, mirroring the
// complete-modal branch and inspectCodexSafetyPrompt: blocking costs one poll
// of the ~20s dismissal budget, but returning false would let
// task.DismissTrustPrompt deliver the briefing into a real modal af failed to
// confirm (#3302). No keystroke is sent.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintCursorReadErrorFailsClosed(t *testing.T) {
	const midPaint = `  Folder access
  /tmp/af-home

  Trust this folder? Codex can read, edit, and run files here, subject to
  your permission settings.

› 1. Trust and continue
  2. Qu`

	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return nil, errors.New("display-message refused")
			}
			return []byte(midPaint), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramCodex, NewMockPtyFactory(t), cmdExec)

	require.True(t, session.CheckAndHandleTrustPrompt(),
		"a cursor-read error on a live modal's trailing rows must be treated as a blocking dialog")
	require.Empty(t, sentKeystrokes(commands),
		"no Enter may be injected when the partial modal's state cannot be confirmed; got %v", commands)
}

// End-to-end at the trust-prompt gate: a partial mid-paint frame holds long
// enough for the next poll to observe the complete modal, which the
// complete-modal branch dismisses with Enter. The poll after that sees the
// released composer and reports the pane clear, so the create path may
// finally deliver the briefing — in either wording.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintThenCompleteDismisses(t *testing.T) {
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	const midPaintNew = `  Folder access
  /tmp/af-home

  Trust this folder? Codex can read, edit, and run files here, subject to
  your permission settings.

› 1. Trust and continue`
	const midPaintOld = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue`

	for _, tc := range []struct{ name, midPaint, complete string }{
		{"old wording", midPaintOld, codexDirectoryTrustDialog},
		{"new wording", midPaintNew, codexDirectoryTrustDialogNew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, commands := runTrustPromptSequence(t, ProgramCodex,
				tc.midPaint,
				tc.complete,
				normalPane,
			)

			require.True(t, session.CheckAndHandleTrustPrompt(),
				"the partial mid-paint frame holds; delivery must stay blocked")
			require.Empty(t, sentKeystrokes(*commands),
				"the hold path sends no key — the complete-modal branch dispatches Enter; got %v", *commands)

			require.True(t, session.CheckAndHandleTrustPrompt(),
				"the complete modal is handled on the next poll")
			require.Contains(t, sentKeystrokes(*commands), "tmux send-keys -t =af_trust: Enter",
				"the complete modal gets exactly the Enter; got %v", *commands)

			require.False(t, session.CheckAndHandleTrustPrompt(),
				"the released composer reports clear, so delivery proceeds")
		})
	}
}

// Ordinary Codex panes that share the modal's "›" glyph or its prose must
// stay clear at every width — the hold may never over-fire on the composer
// the create path is waiting for.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogOrdinaryPanesStayClear(t *testing.T) {
	const start = `  >_ OpenAI Codex (v0.171.0)

  model:     gpt-5.6-sol max   /model to change
  directory: ~/agent-factory

› Ask Codex to do anything`
	const idleComposer = `›
  gpt-5.6-sol max · ~/agent-factory`

	for _, tc := range []struct{ name, content string }{
		{"start screen", start},
		{"start screen narrow", wrapTrustPaneForTest(start, 12)},
		{"idle composer", idleComposer},
		{"idle composer narrow", wrapTrustPaneForTest(idleComposer, 12)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramCodex, tc.content)
			require.False(t, handled,
				"an ordinary Codex pane must not be held by the directory-trust gate")
			require.Empty(t, sentKeystrokes(cmds), "got %v", cmds)
		})
	}
}

// codexDirectoryTrustTrailingRows recognizes the pane tails a directory-trust
// modal leaves while it paints — the selected option row, then any prefix of
// what follows — and declines everything else.
func TestCodexDirectoryTrustTrailingRows(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"selected Yes row last", "› 1. Yes, continue", true},
		{"partial selected row", "› 1. Yes, con", true},
		{"selection marker alone", "›", true},
		{"question above selected row", "Do you trust the contents of this directory?\n› 1. Yes, continue", true},
		{"trailing blanks after Yes row", "› 1. Yes, continue\n\n   \n", true},
		{"No row last with Yes row above", "› 1. Yes, continue\n  2. No, quit", true},
		{"partial No row", "› 1. Yes, continue\n  2. No", true},
		{"trailing blanks after No row", "› 1. Yes, continue\n  2. No, quit\n\n", true},
		{"partial footer", "› 1. Yes, continue\n  2. No, quit\n\n  Press enter to cont", true},
		{"settled tail, question scrolled", "› 1. Yes, continue\n  2. No, quit\n\n  Press enter to continue", true},
		{"new wording selected row last", "› 1. Trust and continue", true},
		{"new wording partial selected row", "› 1. Trust and con", true},
		{"new wording partial Quit row", "› 1. Trust and continue\n  2. Qu", true},
		{"new wording partial footer", "› 1. Trust and continue\n  2. Quit\n\n  enter continue · esc qu", true},
		{"new wording settled tail", "› 1. Trust and continue\n  2. Quit\n\n  enter continue · esc quit", true},
		{"restricted selected row last", "› 1. Open restricted", true},
		{"restricted settled tail", "› 1. Open restricted\n  2. Quit\n\n  enter continue · esc quit", true},
		{"existing-task selected row last", "› 1. Open existing task", true},
		{"sandbox footer variant", "› 1. Trust and continue\n  2. Quit\n\n  enter continue and create sandbox · esc quit", true},
		{"old sandbox footer variant", "› 1. Yes, continue\n  2. No, quit\n\n  Press enter to continue and create a sandbox...", true},
		{"back-verb footer variant", "› 1. Trust and continue\n  2. Back to Agent Command Center\n\n  enter continue · esc back", true},
		{"cancel-verb footer variant", "› 1. Trust and continue\n  2. Keep current directory\n\n  enter continue · esc cancel", true},
		{"wrapped selected row at 12 cols", "› 1. Yes,\n  continue", true},
		{"wrapped new selected row at 12 cols", "› 1. Trust\n  and\n  continue", true},
		{"styled Yes row carries SGR escapes", "\x1b[1m\x1b[36m› 1. Yes, continue\x1b[0m", true},
		{"CRLF endings", "Do you trust the contents of this directory?\r\n› 1. Yes, continue\r\n  2. No", true},
		{"complete modal is not a partial tail", codexDirectoryTrustDialog, true},
		{"complete new modal is not a partial tail", codexDirectoryTrustDialogNew, true},
		// Everything below must NOT match: prose, ordinary panes, and rows
		// the modal never paints after its selected row.
		{"empty pane", "", false},
		{"blank lines only", "\n\n\n", false},
		{"unrelated composer status", "gpt-5.6-sol max · ~/agent-factory", false},
		{"question alone", "Do you trust the contents of this directory?", false},
		{"No row alone", "2. No, quit", false},
		{"question above No row without selected row", "Do you trust the contents of this directory?\n  2. No, quit", false},
		{"unselected Yes label", "  1. Yes, continue", false},
		{"Yes row embedded mid-line is prose", "the picker reads › 1. Yes, continue in its body", false},
		{"trailing prose after Yes row", "› 1. Yes, continue\nfollowed by an ordinary line", false},
		{"composer prompt line", "› Ask Codex to do anything", false},
		{"composer with draft text", "› some draft prompt\n  gpt-5.6-sol max · ~/agent-factory", false},
		{"second-option marker row", "› 2. No, quit", false},
		{"safety-picker selected row", "› 1. Retry with a faster model", false},
		{"update-picker selected row", "› 1. Update now\n  2. Skip until next version\n\n  Press enter to continue", false},
		{"extra prose inside the tail", "› 1. Yes, continue\nnote this\n  2. No, quit", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexDirectoryTrustTrailingRows(tt.content); got != tt.want {
				t.Errorf("codexDirectoryTrustTrailingRows() = %v, want %v", got, tt.want)
			}
		})
	}
}

// CodexTrustPromptPresent stays the complete-modal gate the Enter path
// answers: both wordings at any width, the footer last, and nothing less.
func TestCodexTrustPromptPresent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"complete old modal", codexDirectoryTrustDialog, true},
		{"complete new modal", codexDirectoryTrustDialogNew, true},
		{"old modal wrapped at 12 cols", wrapTrustPaneForTest(codexDirectoryTrustDialog, 12), true},
		{"old modal wrapped at 40 cols", wrapTrustPaneForTest(codexDirectoryTrustDialog, 40), true},
		{"new modal wrapped at 12 cols", wrapTrustPaneForTest(codexDirectoryTrustDialogNew, 12), true},
		{"new modal wrapped at 17 cols", wrapTrustPaneForTest(codexDirectoryTrustDialogNew, 17), true},
		{"error paragraph between options and footer",
			"> You are in /tmp/af-home\n\n  Do you trust the contents of this directory?\n\n› 1. Yes, continue\n  2. No, quit\n\n  some error row\n\n  Press enter to continue", true},
		{"old modal under transcript", "earlier work output\n\n" + codexDirectoryTrustDialog, true},
		{"old modal above trailing whitespace", codexDirectoryTrustDialog + "\n\n   \n", true},
		// Partials and prose must not match — the paint-hold owns those.
		{"old modal missing footer",
			"Do you trust the contents of this directory?\n\n› 1. Yes, continue\n  2. No, quit", false},
		{"old modal missing question", "› 1. Yes, continue\n  2. No, quit\n\n  Press enter to continue", false},
		{"old modal missing No row", "Do you trust the contents of this directory?\n\n› 1. Yes, continue\n\n  Press enter to continue", false},
		{"old modal missing selected row", "Do you trust the contents of this directory?\n\n  2. No, quit\n\n  Press enter to continue", false},
		{"old modal plus trailing prose", codexDirectoryTrustDialog + "\nmore transcript after", false},
		{"new modal missing footer", "Trust this folder?\n\n› 1. Trust and continue\n  2. Quit", false},
		{"unselected Yes row", strings.Replace(codexDirectoryTrustDialog, "› 1. Yes, continue", "  1. Yes, continue", 1), false},
		{"restricted modal is not answerable", codexDirectoryTrustDialogRestricted, false},
		{"question alone", "Do you trust the contents of this directory?", false},
		{"options without question", "Yes, continue\n2. No, quit\nPress enter to continue", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodexTrustPromptPresent(tt.content); got != tt.want {
				t.Errorf("CodexTrustPromptPresent() = %v, want %v", got, tt.want)
			}
		})
	}
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

func TestCheckAndHandleTrustPrompt_OverriddenDocMessageIsDismissed(t *testing.T) {
	content := "\x1b[7mhttps://aider.chat/docs/faq.html\x1b[0m\n" +
		"Review these docs before continuing? (Y)es/(N)o/(D)on't ask again [Yes]:"
	for _, program := range []string{ProgramAider, ProgramGemini} {
		t.Run(program, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, program, content)
			require.True(t, handled, "the renderer invariant must identify the active URL prompt")
			require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: D Enter")
		})
	}
}

func TestCheckAndHandleTrustPrompt_PrintedURLThenUnrelatedConfirmInjectsNothing(t *testing.T) {
	content := `See https://aider.chat/docs/faq.html for the details.
Add src/main.go to the chat? (Y)es/(N)o/(D)on't ask again [Yes]:`
	for _, program := range []string{ProgramAider, ProgramGemini} {
		t.Run(program, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, program, content)
			require.False(t, handled,
				"a recently printed URL does not turn the next generic confirmation into a URL-offer prompt")
			require.Empty(t, sentKeystrokes(cmds), "got %v", cmds)
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

func TestCheckAndHandleTrustPrompt_DocMatcherSourceCommentInjectsNothing(t *testing.T) {
	content := `// DocTrustPromptPresent reports whether content shows the documentation-link
// trust dialog shared by aider/gemini:
//
//	Open documentation url for more info? (Y)es/(N)o/(D)on't ask again [Yes]:
//
// ACTION HERE REQUIRES POSITIVE EVIDENCE.
func DocTrustPromptPresent(content string) bool {
	return strings.Contains(content, "Open documentation url for more info") &&
		strings.Contains(content, "(D)on't ask again")
}`

	for _, program := range []string{ProgramAider, ProgramGemini} {
		t.Run(program, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, program, content)
			require.False(t, handled,
				"source text describing the prompt is not evidence that the prompt is active")
			require.Empty(t, sentKeystrokes(cmds),
				"repo-controlled source visible in the pane must not trigger daemon input; got %v", cmds)
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
