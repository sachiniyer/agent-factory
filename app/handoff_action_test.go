package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func handoffActionInstance(t *testing.T, title, program string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: program})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	return inst
}

// The picker must never offer the agent that is already running: choosing it
// would stop and restart the agent to arrive back where it started. Asserting
// on the CONTENT rather than the length is what makes this catch an off-by-one,
// since SupportedPrograms is positionally load-bearing.
func TestHandoffAgentChoices_ExcludesTheRunningAgent(t *testing.T) {
	choices := handoffAgentChoices(tmux.ProgramClaude)

	require.NotEmpty(t, choices)
	require.NotContains(t, choices, tmux.ProgramClaude, "the running agent must not be offered as a handoff target")
	for _, want := range []string{tmux.ProgramCodex, tmux.ProgramGemini, tmux.ProgramAider, tmux.ProgramAmp, tmux.ProgramOpencode} {
		require.Contains(t, choices, want, "every other supported agent is a valid target")
	}
}

// Opening the picker must not dispatch anything: the swap happens only after the
// user picks an agent AND confirms.
func TestHandleHandoff_OpensPickerWithoutDispatching(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", tmux.ProgramClaude))
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetHandoffRunnerForTest(func(string, string, string) (string, error) {
		called = true
		return "", nil
	})
	defer restore()

	_, _ = h.handleHandoff()

	require.Equal(t, stateSelectHandoffAgent, h.state, "H must open the agent picker")
	require.NotNil(t, h.selectionOverlay)
	require.False(t, called, "opening the picker must not swap the agent")
}

// The picker's selected index must be resolved against the FILTERED choice list
// it was built from. Resolving it against tmux.SupportedPrograms instead would
// hand off to the wrong agent — silently, and to a plausible-looking one.
func TestHandleStateSelectHandoffAgent_ConfirmsThenSwapsTheChosenAgent(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", tmux.ProgramClaude))
	h.sidebar.SetSelectedInstance(0)

	var gotTitle, gotTarget string
	restore := SetHandoffRunnerForTest(func(title, _, target string) (string, error) {
		gotTitle, gotTarget = title, target
		return tmux.ProgramClaude, nil
	})
	defer restore()

	_, _ = h.handleHandoff()
	// Index 2 of the filtered list (claude removed) is gemini; index 2 of the
	// unfiltered SupportedPrograms is aider. A regression that indexes the wrong
	// slice picks aider and fails here.
	want := handoffAgentChoices(tmux.ProgramClaude)[2]
	require.Equal(t, tmux.ProgramGemini, want, "fixture assumption: filtered[2] is gemini")

	h.selectionOverlay.SetSelectedIndex(2)
	_, _ = h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	// Picking does not swap — it raises the confirmation.
	require.Equal(t, stateConfirm, h.state, "a handoff must be confirmed before it runs")
	require.Empty(t, gotTarget, "the swap must not fire before confirmation")
	require.NotNil(t, h.confirmationOverlay)

	// Press the confirm key, which forwards the stashed message into the loop.
	_, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd, "confirming must forward the handoff message")
	start, ok := cmd().(startHandoffMsg)
	require.True(t, ok, "confirming must emit startHandoffMsg")
	require.Equal(t, tmux.ProgramGemini, start.target)

	msg := h.handoffCmd(start.title, start.target)()
	done, ok := msg.(handoffDoneMsg)
	require.True(t, ok, "expected handoffDoneMsg, got %T", msg)
	require.NoError(t, done.err)
	require.Equal(t, "worker", gotTitle)
	require.Equal(t, tmux.ProgramGemini, gotTarget, "the swap must target the agent the user picked")
}

// Cancelling the picker must leave the session untouched.
func TestHandleStateSelectHandoffAgent_CancelDoesNotSwap(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", tmux.ProgramClaude))
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetHandoffRunnerForTest(func(string, string, string) (string, error) {
		called = true
		return "", nil
	})
	defer restore()

	_, _ = h.handleHandoff()
	_, _ = h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEsc})

	require.Equal(t, stateDefault, h.state, "cancelling returns to the default state")
	require.Nil(t, h.confirmationOverlay, "cancelling must not raise a confirmation")
	require.False(t, called)
}
