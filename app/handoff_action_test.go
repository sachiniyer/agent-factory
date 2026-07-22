package app

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
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
	restore := SetHandoffRunnerForTest(func(daemon.HandoffSessionRequest) (string, error) {
		called = true
		return "", nil
	})
	defer restore()

	_, _ = h.handleHandoff()

	require.Equal(t, stateSelectHandoffAgent, h.state, "H must open the agent picker")
	require.NotNil(t, h.selectionOverlay)
	require.False(t, called, "opening the picker must not swap the agent")
}

func TestHandleHandoff_RefusesArchivedSessionBeforePicker(t *testing.T) {
	h := newTestHome(t)
	inst := handoffActionInstance(t, "archived", tmux.ProgramClaude)
	inst.SetStatusForTest(session.Archived)
	inst.SetStartedForTest(false)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.handleHandoff()

	require.Equal(t, stateDefault, h.state, "an inert row must not enter the handoff flow")
	require.Nil(t, h.selectionOverlay, "the user should not pick a target for an impossible handoff")
	require.Contains(t, strings.ToLower(h.errBox.FullError()), "archived")
}

// The picker's selected index must be resolved against the FILTERED choice list
// it was built from. Resolving it against tmux.SupportedPrograms instead would
// hand off to the wrong agent — silently, and to a plausible-looking one.
func TestHandleStateSelectHandoffAgent_ConfirmsThenSwapsTheChosenAgent(t *testing.T) {
	h := newTestHome(t)
	worker := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	h.store.AddInstance(worker)
	h.sidebar.SetSelectedInstance(0)

	var gotRequest daemon.HandoffSessionRequest
	restore := SetHandoffRunnerForTest(func(req daemon.HandoffSessionRequest) (string, error) {
		gotRequest = req
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
	require.Empty(t, gotRequest.To, "the swap must not fire before confirmation")
	require.NotNil(t, h.confirmationOverlay)

	// Press the confirm key, which forwards the stashed message into the loop.
	_, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd, "confirming must forward the handoff message")
	start, ok := cmd().(startHandoffMsg)
	require.True(t, ok, "confirming must emit startHandoffMsg")
	require.Equal(t, tmux.ProgramGemini, start.request.To)

	msg := h.handoffCmd(start.request)()
	done, ok := msg.(handoffDoneMsg)
	require.True(t, ok, "expected handoffDoneMsg, got %T", msg)
	require.NoError(t, done.err)
	require.Equal(t, "worker", gotRequest.Title)
	require.Equal(t, h.repoID, gotRequest.RepoID)
	require.Equal(t, worker.ID, gotRequest.ID, "the TUI must carry the stable session identity to the daemon")
	require.Equal(t, tmux.ProgramGemini, gotRequest.To, "the swap must target the agent the user picked")
}

func TestHandleStateSelectHandoffAgent_KeepsTheSessionThatOpenedThePicker(t *testing.T) {
	h := newTestHome(t)
	alpha := handoffActionInstance(t, "alpha", tmux.ProgramClaude)
	beta := handoffActionInstance(t, "beta", tmux.ProgramCodex)
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.handleHandoff()
	require.Equal(t, tmux.ProgramCodex, h.handoffChoices[0], "fixture assumption: codex is offered for alpha")

	// A snapshot can reconcile the sidebar while the modal owns the keyboard.
	// Moving the cursor reproduces the consequential part of that update: the
	// current selection is now beta even though alpha opened the picker.
	h.sidebar.SetSelectedInstance(1)
	h.selectionOverlay.SetSelectedIndex(0)
	_, _ = h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stateConfirm, h.state)
	_, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd)
	start, ok := cmd().(startHandoffMsg)
	require.True(t, ok, "confirming must emit startHandoffMsg")
	require.Equal(t, alpha.Title, start.request.Title,
		"a background selection change must not retarget the handoff")
	require.Equal(t, alpha.ID, start.request.ID)
	require.Equal(t, h.repoID, start.request.RepoID)
}

func TestHandleStateSelectHandoffAgent_DropsARecreatedSameTitleTarget(t *testing.T) {
	h := newTestHome(t)
	original := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.handleHandoff()
	require.True(t, h.store.RemoveInstanceByTitle(original.Title))
	replacement := handoffActionInstance(t, original.Title, tmux.ProgramClaude)
	require.NotEqual(t, original.ID, replacement.ID, "fixture requires a replacement session identity")
	h.store.AddInstance(replacement)
	h.selectionOverlay.SetSelectedIndex(0)

	_, cmd := h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	require.Equal(t, stateDefault, h.state)
	require.Nil(t, h.confirmationOverlay,
		"a replacement that reused the title must not inherit the pending handoff")
}

func TestHandleStateSelectHandoffAgent_DropsARecreatedLegacyTarget(t *testing.T) {
	h := newTestHome(t)
	original := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	original.ID = "" // persisted before stable session IDs existed
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.handleHandoff()
	require.True(t, h.store.RemoveInstanceByTitle(original.Title))
	replacement := handoffActionInstance(t, original.Title, tmux.ProgramClaude)
	replacement.CreatedAt = original.CreatedAt.Add(time.Second)
	require.False(t, replacement.CreatedAt.Equal(original.CreatedAt),
		"fixture requires a distinct legacy identity")
	h.store.AddInstance(replacement)
	h.selectionOverlay.SetSelectedIndex(0)

	_, cmd := h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	require.Equal(t, stateDefault, h.state)
	require.Nil(t, h.confirmationOverlay,
		"a replacement must not inherit an id-less session's pending handoff")
}

func TestHandleStateSelectHandoffAgent_KeepsARebuiltLegacyTarget(t *testing.T) {
	h := newTestHome(t)
	original := handoffActionInstance(t, "worker", tmux.ProgramClaude)
	original.ID = ""
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.handleHandoff()
	rebuilt := handoffActionInstance(t, original.Title, tmux.ProgramClaude)
	rebuilt.ID = ""
	rebuilt.CreatedAt = original.CreatedAt
	require.True(t, h.store.ReplaceInstance(original, rebuilt))
	h.selectionOverlay.SetSelectedIndex(0)

	_, _ = h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stateConfirm, h.state,
		"a snapshot rebuild with the same legacy discriminator remains the same target")
	require.NotNil(t, h.confirmationOverlay)
}

// Cancelling the picker must leave the session untouched.
func TestHandleStateSelectHandoffAgent_CancelDoesNotSwap(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(handoffActionInstance(t, "worker", tmux.ProgramClaude))
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetHandoffRunnerForTest(func(daemon.HandoffSessionRequest) (string, error) {
		called = true
		return "", nil
	})
	defer restore()

	_, _ = h.handleHandoff()
	_, _ = h.handleStateSelectHandoffAgent(tea.KeyMsg{Type: tea.KeyEsc})

	require.Equal(t, stateDefault, h.state, "cancelling returns to the default state")
	require.Nil(t, h.confirmationOverlay, "cancelling must not raise a confirmation")
	require.Equal(t, handoffPickerTarget{}, h.handoffTarget)
	require.False(t, called)
}

// TestHandoffConfirmMessage_OmitsAnUnknownOutgoingAgent covers the copy defect
// found by driving the real TUI: with an unidentifiable agent the dialog
// rendered "Hand 'alpha' from  to claude?" — a double space where the outgoing
// agent should be.
//
// The identity fix (Instance.CurrentAgentName) makes the empty case rare, but
// not impossible: a session whose program is a bare non-agent command has no
// agent name to report, and this is the dialog a user reads before letting af
// replace a running agent. It should read as a deliberate sentence, not a
// failed interpolation.
func TestHandoffConfirmMessage_OmitsAnUnknownOutgoingAgent(t *testing.T) {
	withFrom := handoffConfirmMessage("alpha", "codex", "claude")
	if withFrom != "Hand 'alpha' from codex to claude?" {
		t.Fatalf("message = %q, want the outgoing agent named", withFrom)
	}

	unknown := handoffConfirmMessage("alpha", "", "claude")
	if strings.Contains(unknown, "  ") {
		t.Fatalf("message = %q, contains a double space from interpolating an empty agent name", unknown)
	}
	if strings.Contains(unknown, "from") {
		t.Fatalf("message = %q, keeps a dangling \"from\" clause with no agent to name", unknown)
	}
	if !strings.Contains(unknown, "claude") || !strings.Contains(unknown, "alpha") {
		t.Fatalf("message = %q, must still name the session and the target", unknown)
	}
}
