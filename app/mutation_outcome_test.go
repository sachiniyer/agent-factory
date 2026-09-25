package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// replyLost is the error withDaemonHTTPMutation returns when the daemon may have
// run the handler and the reply was lost (#4820): a TransportError that is not
// NotSent.
func replyLost() error {
	return &apiclient.TransportError{Err: errors.New("read: connection reset by peer")}
}

// neverSent is a transport failure that provably never reached the daemon — the
// one transport failure a call site may still present as a plain failure.
func neverSent() error {
	return &apiclient.TransportError{Err: errors.New("dial unix: connect: connection refused"), NotSent: true}
}

func TestMutationMayHaveLanded(t *testing.T) {
	require.True(t, mutationMayHaveLanded(replyLost()))
	require.True(t, mutationMayHaveLanded(committedTaskMutationTestError{}))
	require.False(t, mutationMayHaveLanded(neverSent()))
	require.False(t, mutationMayHaveLanded(errors.New("session not found")))
}

// A create whose reply was lost must not re-arm the naming form: resubmitting
// the retained draft would create a second session. The user is told the
// outcome is unknown and the snapshot decides.
func TestInstanceStarted_UncertainCreateDoesNotReArmDraft(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/project"
	inst := newLoadingInstance(t, "maybe-created")
	inst.Path = h.repoRoot
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	req := sessionStartRequest{Title: inst.Title, RepoPath: h.repoRoot, Program: "claude", Prompt: "p"}

	_, _ = h.Update(instanceStartedMsg{instance: inst, draft: &req, rawPrompt: "p", err: replyLost()})

	require.Nil(t, h.failedCreate, "an uncertain create must not be kept as a draft for the next create to reopen")
	require.Nil(t, h.namingInstance, "the naming form must not reopen")
	require.Equal(t, stateDefault, h.state)
	require.Nil(t, h.recovery, "no 'return to the form' recovery for a create that may exist")
	require.NotContains(t, collectTitles(h.store.GetInstances()), "maybe-created",
		"the placeholder is dropped; the next snapshot adds the session back if the daemon made it")
	assert.Contains(t, h.errBox.FullError(), "could not be confirmed")
}

// The control: a create that never reached the daemon is a real failure and
// still restores the draft, as before #4820.
func TestInstanceStarted_NeverSentCreateStillReArmsDraft(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/project"
	inst := newLoadingInstance(t, "not-created")
	inst.Path = h.repoRoot
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	req := sessionStartRequest{Title: inst.Title, RepoPath: h.repoRoot, Program: "claude", Prompt: "p"}

	_, _ = h.Update(instanceStartedMsg{instance: inst, draft: &req, rawPrompt: "p", err: neverSent()})

	// restoreFailedCreate consumes failedCreate as it re-arms the form, so the
	// form itself is the evidence.
	require.Same(t, inst, h.namingInstance, "a create the daemon never saw is safe to retry from the form")
	require.Equal(t, stateNew, h.state)
	require.Equal(t, "p", h.pendingPrompt, "the draft's prompt is retained")
}

func TestCreateNewTab_UncertainOutcomeIsNotAFailure(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "tab-maybe")
	selectInstance(h, inst)
	before := inst.TabCount()
	t.Cleanup(SetTabCreatorForTest(func(daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		return daemon.CreateTabResponse{}, replyLost()
	}))

	_, _ = h.createNewTab(inst, session.TabKindShell)

	require.Equal(t, before, inst.TabCount(), "nothing is projected locally; the snapshot brings the tab in if it exists")
	assert.Contains(t, h.errBox.FullError(), "could not be confirmed")
}

func TestTaskTriggered_UncertainOutcomeIsNotAFailure(t *testing.T) {
	h := newTestHome(t)
	_, _ = h.Update(taskTriggeredMsg{title: "hello-task", err: replyLost()})
	assert.Contains(t, h.errBox.FullError(), "could not be confirmed")
	assert.NotContains(t, h.errBox.FullError(), "failed to trigger")

	_, _ = h.Update(taskTriggeredMsg{title: "hello-task", err: neverSent()})
	assert.Contains(t, h.errBox.FullError(), "failed to trigger", "a trigger the daemon never saw is a plain failure")
}

// The daemon saved the task and the reply was lost. The form must close like a
// success (so the user cannot submit a duplicate), the list must be re-read to
// show the task, and the message must say the outcome was unconfirmed.
func TestHandleTaskCreate_UncertainOutcomeClosesFormAndRereads(t *testing.T) {
	h := newTestHome(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	t.Cleanup(SetTaskAdderForTest(func(tk task.Task) error {
		require.NoError(t, task.AddTask(tk))
		return replyLost()
	}))

	tp := h.automations.TaskPane()
	tp.SetTasks(nil)
	_, _ = h.showTasksOverlay()
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("maybe-task")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // schedule picker
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do a thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // save
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsCreating(), "an add that may have landed must not leave a retryable form")
	require.Nil(t, h.recovery, "no 'return to the form' recovery for a task that may exist")
	require.Len(t, h.store.GetTasks(), 1, "the list is re-read from disk and shows the task the daemon saved")
	assert.Contains(t, h.errBox.FullError(), "could not be confirmed")
	assert.NotContains(t, h.errBox.FullError(), "failed to save task")
}

// A move whose reply was lost is not projected locally and not reported as a
// refusal; the snapshot shows where the tab ended up.
func TestMoveTab_UncertainOutcomeIsNotAFailure(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(1)
	before := tabNames(alpha)
	t.Cleanup(SetTabReordererForTest(func(daemon.ReorderTabRequest) (daemon.ReorderTabResponse, error) {
		return daemon.ReorderTabResponse{}, replyLost()
	}))

	pressNav(t, h, ">")

	require.Equal(t, before, tabNames(alpha), "nothing is projected locally for an unconfirmed move")
	assert.Contains(t, h.errBox.FullError(), "could not be confirmed")
}
