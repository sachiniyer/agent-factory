package app

import (
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The #4824 call sites: each mutation whose reply may have been lost must not
// read as a refusal. The message says the outcome could not be confirmed, no
// form or local projection claims either outcome, and authoritative state — the
// standing snapshot poll, a task reload, or a projects re-read — decides. Each
// test pairs the uncertain case with a never-sent control that still reads as
// the plain failure it is. replyLost and neverSent are in mutation_outcome_test.go.

const outcomeUnknown = "could not be confirmed"

func TestInstanceKilled_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) (*home, *session.Instance) {
		h := newTestHome(t)
		inst := newKillableInstance(t, "kill-maybe")
		require.NoError(t, inst.Transition(session.BeginKill()))
		h.store.AddInstance(inst)
		_, _ = h.handleInstanceKilled(instanceKilledMsg{target: captureSessionActionTarget(inst, h.repoID), err: err})
		return h, inst
	}

	h, inst := run(t, replyLost())
	assert.Nil(t, h.recovery, "no 'the session is retained' recovery for a kill that may have landed")
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)
	assert.Contains(t, h.errBox.FullError(), "before trying again")
	assert.NotEqual(t, session.OpKilling, inst.GetInFlightOp(),
		"the fence still reverts so the row cannot strand; the snapshot removes it if the kill landed")
	assert.Contains(t, collectTitles(h.store.GetInstances()), "kill-maybe",
		"the row is not removed on a guess; the snapshot decides")

	h, _ = run(t, neverSent())
	require.NotNil(t, h.recovery, "a kill the daemon never saw is a plain failure")
	assert.Contains(t, h.recovery.detail, "The session is retained.")
}

func TestInstanceArchived_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) (*home, *session.Instance) {
		h := newTestHome(t)
		inst := archiveActionInstance(t, "archive-maybe", session.Ready)
		require.NoError(t, inst.Transition(session.BeginArchive()))
		h.store.AddInstance(inst)
		_, _ = h.handleInstanceArchived(instanceArchivedMsg{target: captureSessionActionTarget(inst, h.repoID), err: err})
		return h, inst
	}

	h, inst := run(t, replyLost())
	assert.Nil(t, h.recovery, "no 'the session is retained' recovery for an archive that may have landed")
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the optimistic op clears so the row cannot strand")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"the row is not marked archived on a guess; the snapshot reports it if the archive landed")

	h, _ = run(t, neverSent())
	require.NotNil(t, h.recovery, "an archive the daemon never saw is a plain failure")
	assert.Contains(t, h.recovery.detail, "The session is retained.")
}

func TestInstanceRestored_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) (*home, *session.Instance) {
		h := newTestHome(t)
		inst := archiveActionInstance(t, "restore-maybe", session.Archived)
		inst.SetInFlightOpForTest(session.OpRestoring)
		h.store.AddInstance(inst)
		_, _ = h.handleInstanceRestored(instanceRestoredMsg{target: captureSessionActionTarget(inst, h.repoID), err: err})
		return h, inst
	}

	h, inst := run(t, replyLost())
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)
	assert.NotContains(t, h.errBox.FullError(), "failed to restore")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the overlay clears so the row cannot strand")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness(),
		"liveness is left alone, so a restore that landed still reaches the reconcile's Archived→live rebuild")

	h, _ = run(t, neverSent())
	assert.Contains(t, h.errBox.FullError(), "failed to restore", "a restore the daemon never saw is a plain failure")
}

// A resume re-delivers the pending prompt, so an uncertain one must not invite
// a second `c`.
func TestLimitRetried_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) (*home, *session.Instance) {
		h := newTestHome(t)
		inst := limitActionInstance(t, "resume-maybe", time.Now().Add(time.Hour))
		h.store.AddInstance(inst)
		_, _ = h.handleLimitRetried(limitRetriedMsg{target: captureSessionActionTarget(inst, h.repoID), err: err})
		return h, inst
	}

	h, inst := run(t, replyLost())
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)
	assert.Contains(t, h.errBox.FullError(), "the session's pane")
	assert.NotContains(t, h.errBox.FullError(), "failed to resume")
	assert.True(t, inst.LimitReached(), "the badge is not cleared on a guess; the snapshot clears it if the resume landed")

	h, _ = run(t, neverSent())
	assert.Contains(t, h.errBox.FullError(), "failed to resume", "a resume the daemon never saw is a plain failure")
}

// A second handoff after an uncertain one swaps the agent again and delivers
// the brief twice.
func TestHandoffDone_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) *home {
		h := newTestHome(t)
		t.Cleanup(SetHandoffRunnerForTest(func(daemon.HandoffSessionRequest) (daemon.HandoffSessionResponse, error) {
			return daemon.HandoffSessionResponse{}, err
		}))
		msg := h.handoffCmd(daemon.HandoffSessionRequest{Title: "worker", To: "codex"})().(handoffDoneMsg)
		_, _ = h.handleHandoffDone(msg)
		return h
	}

	h := run(t, replyLost())
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)
	assert.Contains(t, h.errBox.FullError(), "before trying again")
	assert.NotContains(t, h.errBox.FullError(), "failed")

	h = run(t, neverSent())
	assert.Contains(t, h.errBox.FullError(), "handoff of 'worker' to codex failed", "a handoff the daemon never saw is a plain failure")
}

func TestCloseTab_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) (*home, *session.Instance) {
		h := newTestHome(t)
		inst := startedLocalInstance(t, "close-maybe")
		selectInstance(h, inst)
		t.Cleanup(SetTabCloserForTest(func(daemon.CloseTabRequest) error { return err }))
		_, _ = h.deleteConfirmedTab(inst, 1)
		return h, inst
	}

	h, inst := run(t, replyLost())
	assert.Equal(t, 2, inst.TabCount(), "nothing is dropped locally; the snapshot removes the tab if it is gone")
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)

	h, inst = run(t, neverSent())
	assert.Equal(t, 2, inst.TabCount())
	assert.NotContains(t, h.errBox.FullError(), outcomeUnknown, "a close the daemon never saw is a plain failure")
	assert.Contains(t, h.errBox.FullError(), "connection refused")
}

// Retrying an uncertain rename could clobber a concurrent one, so it is neither
// projected nor re-opened as refused.
func TestRenameTab_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) (*home, *session.Instance) {
		h := newTestHome(t)
		inst := freshLocalInstance(t, "rename-maybe")
		inst.AddWebTabForTest("web", "https://example.com")
		selectInstance(h, inst)
		h.store.SetActiveTab(1)
		t.Cleanup(SetTabRenamerForTest(func(daemon.RenameTabRequest) (string, error) { return "", err }))
		_, _ = h.showRenameTabPrompt()
		typeIntoPrompt(h, "-new")
		_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})
		return h, inst
	}

	h, inst := run(t, replyLost())
	assert.Equal(t, stateDefault, h.state, "the rename field does not reopen")
	assert.Nil(t, h.promptOverlay)
	assert.Equal(t, "web", inst.GetTabs()[1].Name, "nothing is projected locally; the snapshot shows the name the daemon holds")
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)

	h, _ = run(t, neverSent())
	assert.NotContains(t, h.errBox.FullError(), outcomeUnknown, "a rename the daemon never saw is a plain failure")
}

// taskOnDisk seeds one enabled task in a real repo, loads it into the pane and
// the rail, and focuses the pane, so a save's reload reads real disk state.
func taskOnDisk(t *testing.T) (*home, task.Task) {
	t.Helper()
	h := newTestHome(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	tk := task.Task{
		ID: "task-4824", Name: "maybe", Prompt: "p", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(tk))
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)
	tp.SetFocus(true)
	return h, tk
}

// An update whose reply was lost is kept as a draft, since only a positive
// not-found may drop one (#4798), but it is not re-sent by the next automatic
// save (#4824). The same save re-reads the task list: when the update did not
// land the draft stays, and when it did, the re-read settles it with a notice.
func TestSaveTaskUpdate_UncertainOutcomeIsKeptNotResentAndRereads(t *testing.T) {
	t.Run("did not land", func(t *testing.T) {
		h, _ := taskOnDisk(t)
		updates := 0
		t.Cleanup(SetTaskUpdaterForTest(func(string, task.TaskUpdate, task.ProjectExpectation) error {
			updates++
			return replyLost()
		}))
		tp := h.automations.TaskPane()
		require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))

		err := h.saveContentPaneState()
		require.ErrorContains(t, err, outcomeUnknown)
		assert.NotContains(t, err.Error(), "failed to save task")
		assert.Nil(t, h.recovery, "no 'cannot save' recovery for an edit that may have landed")
		assert.True(t, tp.IsDirty(), "the draft is kept")
		assert.False(t, tp.GetTasks()[0].Enabled, "the edited value is still shown")
		assert.True(t, h.store.GetTasks()[0].Enabled, "the rail is re-read from disk and shows what the daemon holds")

		require.NoError(t, h.saveContentPaneState())
		assert.Equal(t, 1, updates, "the next automatic save does not re-send the uncertain update")
		assert.True(t, tp.IsDirty(), "and the draft is still kept")

		_, _ = h.handleQuit()
		require.False(t, h.quitting, "the first quit stops to say the kept edit is left unsaved")
		assert.Equal(t, 1, updates, "quitting does not re-send it either")
		_, _ = h.handleQuit()
		assert.True(t, h.quitting, "the next quit goes through")
	})
	t.Run("landed", func(t *testing.T) {
		h, _ := taskOnDisk(t)
		t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate, expect task.ProjectExpectation) error {
			_, err := task.UpdateTask(id, update, expect)
			require.NoError(t, err)
			return replyLost()
		}))
		tp := h.automations.TaskPane()
		require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))

		err := h.saveContentPaneState()
		require.ErrorContains(t, err, outcomeUnknown)
		assert.False(t, tp.IsDirty(), "the re-read carries the edit, so the draft settles as clean")
		assert.False(t, tp.GetTasks()[0].Enabled)
		assert.Contains(t, tp.TakeSettledDraftNotice(), `Saved edits to "maybe"`, "and never silently")
	})
}

// The control: an update the daemon never saw stays a retained, retryable draft.
func TestSaveTaskUpdate_NeverSentStillRetained(t *testing.T) {
	h, _ := taskOnDisk(t)
	t.Cleanup(SetTaskUpdaterForTest(func(string, task.TaskUpdate, task.ProjectExpectation) error {
		return neverSent()
	}))
	tp := h.automations.TaskPane()
	require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))

	err := h.saveContentPaneState()
	require.ErrorContains(t, err, "failed to save task")
	assert.True(t, tp.IsDirty(), "a never-sent edit stays retryable")
	require.NotNil(t, h.recovery)
}

func TestSaveTaskRemove_UncertainOutcomeIsNotAFailure(t *testing.T) {
	run := func(t *testing.T, err error) (*home, error) {
		h, tk := taskOnDisk(t)
		t.Cleanup(SetTaskRemoverForTest(func(string, task.ProjectExpectation) error { return err }))
		tp := h.automations.TaskPane()
		require.True(t, tp.DeleteTask(tk.ID))
		require.Empty(t, tp.GetTasks(), "precondition: the queued deletion hides the row")
		return h, h.saveContentPaneState()
	}

	h, err := run(t, replyLost())
	require.ErrorContains(t, err, outcomeUnknown)
	assert.NotContains(t, err.Error(), "failed to remove task")
	assert.Equal(t, []string{"task-4824"}, taskIDs(h.automations.TaskPane().GetTasks()),
		"the pane is re-read from disk: the removal did not land here, so the task is shown")

	_, err = run(t, neverSent())
	require.ErrorContains(t, err, "failed to remove task", "a removal the daemon never saw is a plain failure")
}

// countAllReposReads counts the cross-repo snapshot reads refreshSidebarProjects
// makes — the re-read of the Projects section.
func countAllReposReads(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		n.Add(1)
		return nil, nil
	}))
	return &n
}

func TestProjectAdded_UncertainOutcomeRereadsAndSaysSo(t *testing.T) {
	h := newTestHome(t)
	reads := countAllReposReads(t)
	_, _ = h.handleProjectAdded(projectAddedMsg{root: "/repo/maybe", err: replyLost()})
	assert.Positive(t, reads.Load(), "the Projects section is re-read")
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)

	h = newTestHome(t)
	reads = countAllReposReads(t)
	_, _ = h.handleProjectAdded(projectAddedMsg{root: "/repo/refused", err: neverSent()})
	assert.Zero(t, reads.Load())
	assert.Empty(t, h.errBox.FullError(), "a registration the daemon never saw stays the quiet warning it was")
}

// A delete that may have archived the project's sessions must not re-scope the
// TUI on a guess, nor read as refused.
func TestProjectDeleted_UncertainOutcomeRereadsAndSaysSo(t *testing.T) {
	run := func(t *testing.T, err error) (*home, *atomic.Int32, string) {
		h := newTestHome(t)
		repoID := config.RepoIDFromRoot(deleteProjectTestRoot)
		h.repoRoot = deleteProjectTestRoot
		h.repoID = repoID
		reads := countAllReposReads(t)
		_, _ = h.handleProjectDeleted(projectDeletedMsg{root: deleteProjectTestRoot, repoID: repoID, name: "acme", err: err})
		return h, reads, repoID
	}

	h, reads, repoID := run(t, replyLost())
	assert.Positive(t, reads.Load(), "the Projects section is re-read")
	assert.Equal(t, repoID, h.repoID, "the TUI is not re-scoped on a guess")
	assert.Contains(t, h.errBox.FullError(), outcomeUnknown)
	assert.Contains(t, h.errBox.FullError(), "before trying again")

	h, reads, _ = run(t, neverSent())
	assert.Zero(t, reads.Load())
	assert.Contains(t, h.errBox.FullError(), "failed to delete project", "a delete the daemon never saw is a plain failure")
}

// Pause/resume need no call-site handling (session_control.go): the heartbeat
// re-sends the pause every renew tick whatever the last outcome was, so an
// uncertain pause is simply renewed. This pins that it keeps renewing.
func TestStatusPollPauseHeartbeat_RenewsAfterUncertainOutcome(t *testing.T) {
	calls := make(chan struct{}, 8)
	done := make(chan struct{})
	exited := make(chan struct{})
	go runStatusPollPauseHeartbeat(func(daemon.PauseStatusPollRequest) error {
		calls <- struct{}{}
		return replyLost()
	}, daemon.PauseStatusPollRequest{Title: "attached"}, done, exited)

	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(10 * statusPollRenewInterval):
			t.Fatalf("pause %d was never sent: an uncertain outcome stopped the heartbeat", i+1)
		}
	}
	close(done)
	<-exited
}
