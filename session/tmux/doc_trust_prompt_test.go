package tmux

import (
	"errors"
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
	// Fragments that do NOT end in the directory-trust modal's selected Yes
	// row: the question alone, the option labels and footer alone. None is
	// the trailing shape a live modal leaves mid-paint; each is a prose
	// fragment the live-session poll must never tap Enter on. The modal's
	// partial mid-paint shape — its question followed by the selected Yes row
	// as the last content — is a separate, positive modal-owned signal that
	// codexDirectoryTrustTrailingRows now holds; see
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

// The directory-trust modal paints top-down: question, then selected Yes row,
// then the No row, then the "Press enter to continue" footer. CodexTrustPromptPresent
// requires the complete four-row hierarchy, so a capture that lands mid-paint —
// before the No row and the footer are drawn — ends with the selected Yes row
// as the last visible content and is declined by the complete-modal predicate.
// That same row's leading `›` glyph is also Codex's composer glyph, so
// isReadyContent's hasCodexComposerLine arm (task/runner.go) accepts the very
// same partial frame, and DismissTrustPrompt's old fall-through false was
// misread as "dialog-free" — the briefing was pasted into the live modal and
// the delivery's trailing Enter selected Yes, the #2220 failure this branch
// was written to prevent.
//
// The hold requires the hidden terminal cursor Codex's ListSelectionView
// exposes — the same oracle the complete-modal branch and
// inspectCodexSafetyPrompt both rely on — and sends no key. Delivery stays
// blocked until the next poll observes the complete modal (dismissed above) or
// the released composer.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintHolds(t *testing.T) {
	// The modal's top rows painted; the No row and "Press enter to continue"
	// footer have not landed yet — the exact mid-paint shape the repro
	// captured. The trailing content is the selected Yes row, so
	// CodexTrustPromptPresent declines it.
	const midPaint = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue`

	handled, cmds := runTrustPromptCheck(t, ProgramCodex, midPaint)
	require.True(t, handled,
		"a live modal mid-paint that positively owns the pane must not be reported clear")
	require.Empty(t, sentKeystrokes(cmds),
		"the hold path sends no key — Enter stays gated on the complete-modal predicate; got %v", cmds)
}

// A pane too short for the directory-trust modal's question scrolls the
// question off and leaves only the selected Yes row visible. The trailing Yes
// row, paired with the hidden cursor that marks the ListSelectionView, is
// still positive evidence the modal owns the pane, so the hold fires here too:
// reporting the pane clear on this frame would let task.DismissTrustPrompt
// deliver the briefing into the live modal the very next poll.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogYesRowAloneHolds(t *testing.T) {
	handled, cmds := runTrustPromptCheck(t, ProgramCodex, "› 1. Yes, continue")
	require.True(t, handled,
		"the selected Yes row alone with a hidden cursor still positively marks a modal-owned pane")
	require.Empty(t, sentKeystrokes(cmds),
		"the hold path sends no key; got %v", cmds)
}

// The directory-trust modal paints top-down: question, selected Yes row, No
// row, then the "Press enter to continue" footer. A capture can land one row
// later than the Yes-row-trailing frame above — after the No row is painted
// but before the footer — leaving the No row as the last visible content with
// the selected Yes row above it. CodexTrustPromptPresent declines this frame
// too (the footer is absent, so affordance != last), and the Yes row above
// keeps isReadyContent's hasCodexComposerLine arm accepting it, so the hold
// must cover this later partial-paint frame as well: reporting the pane clear
// would let task.DismissTrustPrompt paste the briefing into the live modal the
// very next poll (the same #2220 wedge the Yes-row hold closes).
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogNoRowMidPaintHolds(t *testing.T) {
	// The top rows and the No row have painted; only the "Press enter to
	// continue" footer is still outstanding — the later mid-paint shape. The
	// trailing content is the No row with the selected Yes row above it, so
	// CodexTrustPromptPresent declines it.
	const noRowMidPaint = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit`

	handled, cmds := runTrustPromptCheck(t, ProgramCodex, noRowMidPaint)
	require.True(t, handled,
		"a live modal whose No row has painted but whose footer has not must not be reported clear")
	require.Empty(t, sentKeystrokes(cmds),
		"the hold path sends no key — Enter stays gated on the complete-modal predicate; got %v", cmds)
}

// The No-row-trailing frame's quoted-prose counterpart: the modal text (through
// the No row, no footer) sits in scrollback above an active composer, whose
// visible cursor marks the pane as composer-owned. The hidden-cursor oracle
// that gates the partial-frame hold must leave the create path clear to
// deliver the briefing — the same quoted-prose protection the Yes-row hold
// keeps. No Enter is injected: a visible cursor fails open only by falling
// through to return false, never by sending a key.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogNoRowMidPaintWithVisibleCursorInjectsNothing(t *testing.T) {
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
				// directory-trust dialog text through the No row is quoted
				// transcript above an active composer, not a live modal
				// mid-paint.
				return []byte("20 0 1"), nil
			}
			return []byte(quotedAboveComposer), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramCodex, NewMockPtyFactory(t), cmdExec)

	require.False(t, session.CheckAndHandleTrustPrompt(),
		"a visible cursor means the composer is active, not a directory-trust modal mid-paint")
	require.Empty(t, sentKeystrokes(commands),
		"quoted Yes+No rows above a live composer must never receive Enter; got %v", commands)
}

// The trailing Yes row alone is not proof a modal owns the pane: the dialog
// text can sit verbatim in quoted agent output above an active composer. The
// ordinary Codex composer exposes a terminal cursor, the modal hides one, so a
// visible cursor must leave the create path clear to deliver the briefing —
// the same quoted-prose protection the complete-modal branch already keeps,
// preserved here by the hidden-cursor oracle that gates the partial-frame hold.
// No Enter is injected on either path: a visible cursor fails open only by
// falling through to return false, never by sending a key.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintWithVisibleCursorInjectsNothing(t *testing.T) {
	const quotedAboveComposer = `Earlier output quoted the modal text:

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue`

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
		"quoted Yes row above a live composer must never receive Enter; got %v", commands)
}

// A cursor-read failure on the partial frame fails closed, mirroring the
// complete-modal branch and inspectCodexSafetyPrompt: blocking costs one poll
// of the ~20s dismissal budget, but returning false would let
// task.DismissTrustPrompt deliver the briefing into a real modal af failed to
// confirm (#3302). No keystroke is sent.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintCursorReadErrorFailsClosed(t *testing.T) {
	const midPaint = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue`

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
// released composer and reports the pane clear, so the create path may finally
// deliver the briefing. Reproduces the create-path frame sequence
// (WaitForReady → DismissTrustPrompt → SendPromptWithEvidence) at the gate
// level, with the partial frame inserted at the head to expose the #2220 wedge
// the hold now closes.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogMidPaintThenCompleteDismisses(t *testing.T) {
	const midPaint = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue`

	const normalPane = "gpt-5.6-sol max · ~/agent-factory"

	session, commands := runTrustPromptSequence(t, ProgramCodex,
		midPaint,
		codexDirectoryTrustDialog,
		normalPane,
	)

	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the partial mid-paint frame holds; delivery must stay blocked")
	require.Empty(t, sentKeystrokes(*commands),
		"the hold path sends no key — the complete-modal branch dispatches Enter; got %v", *commands)

	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the next poll observes the complete modal and dismisses it with Enter")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands),
		"the complete-modal branch is unchanged: it dispatches Enter only after the full predicate fires")

	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the released pane is clear; the create path may deliver the briefing")
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

// codexDirectoryTrustTrailingRows recognizes the directory-trust modal's
// trailing mid-paint rows: the selected Yes row as the last content (No row
// and footer not yet painted) OR the No row as the last content with the
// selected Yes row above it (footer not yet painted), whether the question is
// painted above, the row sits alone (a pane too short for the question), the
// row is styled, or the pane carries CRLF line endings. It declines the
// modal's other rows, the complete modal (whose footer is last), a bare No row
// without the selected Yes row above, and unrelated content. The hidden-cursor
// oracle the caller pairs with this predicate is what keeps the quoted-prose
// shape from being a false positive; the predicate's own contract is only "the
// pane ends with a mid-paint trailing row of the modal".
func TestCodexDirectoryTrustTrailingRows(t *testing.T) {
	const midPaint = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue`

	const noRowMidPaint = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit`

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"partial mid-paint with question above", midPaint, true},
		{"selected Yes row alone", "› 1. Yes, continue", true},
		{"question alone", "Do you trust the contents of this directory?", false},
		{"trailing blank lines after Yes row", "Do you trust the contents of this directory?\n› 1. Yes, continue\n\n   \n", true},
		// The footer landed, so the complete-modal predicate owns the frame
		// instead.
		{"complete modal ends in footer", codexDirectoryTrustDialog, false},
		// The modal painted one row further than the Yes-row-trailing frame:
		// the No row is now last, with the selected Yes row above it (footer
		// still absent). CodexTrustPromptPresent declines this frame too, and
		// the Yes row above keeps isReadyContent accepting it, so the hold
		// must cover it.
		{"No row last with selected Yes row above", noRowMidPaint, true},
		{"Yes row followed by No row", "› 1. Yes, continue\n  2. No, quit", true},
		{"trailing blank lines after No row", "› 1. Yes, continue\n  2. No, quit\n\n   \n", true},
		{"CRLF with No row last and Yes row above", "Do you trust the contents of this directory?\r\n› 1. Yes, continue\r\n  2. No, quit", true},
		// A bare No row without the selected Yes row above is just an option
		// label and can appear in quoted prose; it is not positive evidence a
		// modal owns the pane.
		{"No row alone", "2. No, quit", false},
		{"question above No row without selected Yes row", "Do you trust the contents of this directory?\n  2. No, quit", false},
		{"empty pane", "", false},
		{"blank lines only", "\n\n\n", false},
		{"unrelated composer status line", "gpt-5.6-sol max · ~/agent-factory", false},
		{"styled Yes row carries SGR escapes", "\x1b[1m\x1b[36m› 1. Yes, continue\x1b[0m", true},
		{"CRLF line endings", "Do you trust the contents of this directory?\r\n› 1. Yes, continue", true},
		{"Yes row embedded mid-line is prose", "the picker reads › 1. Yes, continue in its body", false},
		{"unselected Yes row label is not the trailing row", "  1. Yes, continue", false},
		{"Yes row with trailing prose below declines", "› 1. Yes, continue\nfollowed by an ordinary line", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexDirectoryTrustTrailingRows(tt.content); got != tt.want {
				t.Errorf("codexDirectoryTrustTrailingRows() = %v, want %v", got, tt.want)
			}
		})
	}
}
