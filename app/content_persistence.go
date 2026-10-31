package app

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

func (m *home) handleQuit() (tea.Model, tea.Cmd) {
	m.quitAttempted = true
	// Save any dirty task/hooks state. On failure the panes were reloaded to
	// match disk; abort the quit and surface the error so the user sees the
	// dropped edit instead of losing it silently on the way out.
	if err := m.saveContentPaneState(); err != nil {
		// The first refusal aborts the quit and says why. An identical failure on
		// the very next quit means the user chose to leave: a save that keeps
		// failing must not make the TUI impossible to exit, so the unsaved edits
		// are discarded and the quit goes through (#5860).
		if m.quitSaveFailure != err.Error() {
			m.quitSaveFailure = err.Error()
			return m, m.handleError(fmt.Errorf("%w — press the quit key again to discard the unsaved edits and quit", err))
		}
		m.discardUnsavedPaneEdits()
	}
	m.quitSaveFailure = ""
	if notice := m.takePendingPaneNotice(); notice != "" {
		return m, m.showTransientMessage(notice)
	}
	// The save may have dropped a draft whose task was deleted (#4798). Its
	// notice is otherwise raised by the next snapshot poll, which never comes
	// once af exits, so show it now and let the next quit key go through.
	if notice := m.automations.TaskPane().TakeDiscardedDraftNotice(); notice != "" {
		return m, m.showTransientMessage(notice)
	}
	// A kept edit whose save could not be confirmed is never re-sent on the
	// way out (#4824), so leaving drops it; say so once before letting go.
	if notice := m.automations.TaskPane().TakeUnconfirmedQuitNotice(); notice != "" {
		return m, m.showTransientMessage(notice)
	}
	// A retarget that moved a task out of the active project sink its row from
	// the pane (#1836). The save landed; without this notice leaving looks
	// like the task was dropped. Drained last because a deleted-draft or an
	// unconfirmed-edit notice names lost work, which is more urgent to surface
	// before quitting than a retarget that succeeded.
	if notice := m.automations.TaskPane().TakeCrossRepoRetargetNotice(); notice != "" {
		return m, m.showTransientMessage(notice)
	}
	m.flushTUIViewStateBestEffort()

	// No instances.json write on quit: the daemon is the sole writer (#960 PR 4)
	// and every session/tab mutation already persisted through it as it
	// happened. The TUI holds no authoritative instance state to flush.
	//
	// Do NOT tear down tab sessions on quit: as of #930 PR 2 each instance owns
	// its agent and shell tab tmux sessions, and they must survive an af restart
	// so the user reconnects to them on next launch (Sachin's persistence
	// requirement). Killing an instance still tears its tabs down via
	// LocalBackend.Kill.
	//
	// The live termpane attachments are the one exception: close every WS
	// subscription (the sessions survive, exactly like a detach) so no stream
	// goroutine outlives the TUI (#1089/#1592).
	m.closeAllLiveTermPanes()
	m.quitting = true
	return m, cleanQuitCmd()
}

// invalidateQuitConfirmationAfter drops an armed discard confirmation when the
// input it wraps was anything but another quit attempt. The confirmation is the
// user's answer to the edits that failed to save, so any input in between — a
// keystroke that resumes editing, a click — may change that edit set, and a
// later identical error must warn again instead of discarding edits the user
// never saw refused (#5898). Dismissing the task-save recovery notice is NOT such
// input — it leaves the unsaved edit in place, so Update returns for it before
// this is armed and the confirmation survives to the next quit. Call as `defer m.invalidateQuitConfirmationAfter()()`.
func (m *home) invalidateQuitConfirmationAfter() func() {
	if m.quitSaveFailure == "" {
		return func() {}
	}
	m.quitAttempted = false
	return func() {
		if !m.quitAttempted {
			m.quitSaveFailure = ""
		}
	}
}

// beginUserInput is the shared prologue of a key or mouse message. An input
// that merely dismisses the recovery notice is consumed by it (dismissed) and
// touches nothing else, the quit confirmation included; any other input runs
// with the confirmation disarmed afterwards unless it was itself a quit.
func (m *home) beginUserInput() (dismissed bool, end func()) {
	if m.recovery != nil {
		m.recovery = nil
		return true, func() {}
	}
	return false, m.invalidateQuitConfirmationAfter()
}

// saveContentPaneState persists any changes from the hooks/task panes and
// returns a non-nil error if any persist operation failed. Both panes'
// failures are accumulated so neither is dropped when both are dirty at once.
//
// Recovery semantics on a hooks-save failure (#1001): we leave the HooksPane
// dirty and deliberately do NOT reload it from disk. The edit the user is
// trying to save lives only in memory, so reloading would discard the very
// edit they care about — the silent data loss this fix exists to prevent.
// Returning the error lets callers (handleQuit / focus release) abort the
// destructive action and surface it via handleError; the dirty pane preserves
// the edit so the user can retry from where they left off.
//
// Task-save failures retain edited values and the dirty field-level patch for
// retry. Both panes still reload: the sidebar shows committed data, and the
// TaskPane reconciles with it around the retained draft (#4487).
func (m *home) saveContentPaneState() error {
	// Accumulate failures across both panes so a hooks error and a task error
	// can never clobber one another (#1001).
	var saveErr error
	failedEdit := false

	hp := m.hooksPane
	if hp.IsDirty() && isRemoteTarget() {
		// No save can ever commit this edit (the pane is read-only remotely, so this
		// is a safety net for one that got dirty anyway). Returning an error would
		// abort quit and every task action on every attempt (#5860), so discard it,
		// say so once, and carry on: nothing is lost that could have been kept.
		hp.Discard()
		m.pendingPaneNotice = errRemoteHooksSave().Error() + " — the unsaved hook edit was discarded"
	} else if hp.IsDirty() {
		// Hook edits are written to the in-repo .agent-factory/config.json —
		// the canonical location for post_worktree_commands since #800. The
		// legacy ~/.agent-factory/repos/<id>/config.json stays untouched as a
		// read-only fallback; the saved in-repo key (even when emptied)
		// shadows it.
		if err := saveInRepoPostWorktreeCommandsFn(m.repoRoot, hp.GetCommands()); err != nil {
			log.ErrorLog.Printf("failed to save hooks: %v", err)
			// Surface the failure instead of swallowing it (#1001): callers
			// abort the quit / focus release and show the error overlay rather
			// than silently dropping the edit. The HooksPane stays dirty (see
			// the recovery note above) so the in-memory edit survives for retry.
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to save hooks: %w", err))
		} else {
			m.store.SetHookCount(len(hp.GetCommands()))
		}
	}

	sp := m.automations.TaskPane()
	if !sp.IsDirty() {
		return saveErr
	}

	// Collect every persist failure instead of swallowing them: a partial
	// failure must still surface so the user knows their edit didn't fully
	// land (matches api/tasks.go, which propagates these errors).
	//
	// The writes route through the daemon (#1029 PR 6): the daemon is the sole
	// writer of tasks.json among clients (#960), so a TUI edit/delete goes
	// through the same RPC wrappers the CLI uses instead of touching the file
	// directly. Each CRUD RPC re-arms the daemon's scheduler + watchers
	// in-process, so there is no separate ReloadTasks poke here. The write lands
	// before the refresh; a post-commit refresh failure is classified below so
	// the error remains visible without retrying a durable edit.
	//
	// Persist ONLY the tasks the user actually edited (ConsumeDirty), and only
	// the FIELDS they changed: each edit carries a field-level patch (diffed
	// against the copy the pane loaded), so a save of one field never rewrites a
	// field another writer (CLI/daemon) changed out-of-band while the pane was
	// open — the #1700 clobber, of which #1213's whole-task guard was only a
	// partial fix. A patch that turns out empty (edited then reverted) is a
	// harmless no-op the daemon still validates.
	//
	// retargetCandidates tracks edits that changed ProjectPath so the reload
	// below can tell which ones crossed repos. AcknowledgeSavedEdit advances
	// the pane's baseline but does NOT touch s.tasks, so the post-edit name is
	// still on the in-memory row right here — capture it before the reload
	// replaces the pane's list with the active scope's rows.
	type retargetCandidate struct{ id, name, newPath string }
	var retargetCandidates []retargetCandidate
	// recordRetargetIfMoved queues a ProjectPath change for the post-reload
	// cross-repo notice. The save has committed the edit (nil error or
	// IsMutationCommitted), so the post-edit name is still on the in-memory
	// row right here — AcknowledgeSavedEdit advances the baseline but does not
	// touch s.tasks, so the name must be captured before the reload replaces
	// the pane's list. Record it for both outcomes: a committed-with-warning
	// save landed the binding too, and the active-scope reload below drops the
	// row either way, so without a candidate the warning branch would leave the
	// user with a generic "failed to save task" and no idea where it went.
	recordRetargetIfMoved := func(edit task.TaskEdit) {
		if edit.Update.ProjectPath == nil {
			return
		}
		name := edit.ID
		for _, t := range sp.GetTasks() {
			if t.ID == edit.ID {
				if t.Name != "" {
					name = t.Name
				}
				break
			}
		}
		retargetCandidates = append(retargetCandidates, retargetCandidate{edit.ID, name, *edit.Update.ProjectPath})
	}
	for _, edit := range sp.ConsumeDirty() {
		err := updateTaskThroughDaemon(edit.ID, edit.Update, edit.Expect)
		m.noteRemoteUpdate(edit.ID, edit.Update, edit.Expect, err)
		if err != nil {
			if updateProvedTaskDeleted(err, edit.ID) {
				// Another writer deleted the task (`af tasks remove`), so no
				// retry can ever land this edit; retaining it retried the save
				// and reported "not found" on every close (#4798). Drop it and
				// let the snapshot poll raise the pane's notice naming it.
				log.InfoLog.Printf("discarding unsaved edit to deleted task %q: %v", edit.ID, err)
				sp.DiscardDeletedDraft(edit.ID)
				continue
			}
			if apiclient.IsMutationCommitted(err) {
				// The task write landed; only the daemon's schedule refresh
				// failed. Keep surfacing that failure, but advance this task's
				// baseline so a later edit is diffed against durable state.
				sp.AcknowledgeSavedEdit(edit.ID)
				// The binding change committed with the task, so the reload
				// below drops a retargeted row the same way — record it so the
				// cross-repo notice still names where it landed.
				recordRetargetIfMoved(edit)
			} else if mutationOutcomeUnknown(err) {
				// The update may have landed with only its reply lost (#4824).
				// The draft is kept, since only a positive not-found may drop one
				// (#4798), but it is held out of automatic saves: re-sending it on
				// the next overlay close could overwrite a change another writer
				// made in between. The reload below re-reads the task; if it now
				// carries the edit, the pane settles it and says so, and if not,
				// the user saves it again by editing the task.
				sp.HoldUnconfirmedEdit(edit.ID)
				log.WarningLog.Printf("task update outcome unknown: %v", err)
				saveErr = errors.Join(saveErr, mutationOutcomeError(
					fmt.Sprintf("saving task %q (the edit is kept, not re-sent)", edit.ID), "the task list", err))
				continue
			} else {
				sp.RestoreFailedEdit(edit.ID)
				failedEdit = true
			}
			log.ErrorLog.Printf("failed to update task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to save task %q: %w", edit.ID, err))
			continue
		}
		sp.AcknowledgeSavedEdit(edit.ID)
		// A ProjectPath patch retargets the task's project binding. The save
		// committed it; the reload below decides whether the new binding
		// stayed in the active project or moved it elsewhere.
		recordRetargetIfMoved(edit)
	}
	for _, tsk := range sp.ConsumeDeleted() {
		// tsk is the record as the pane displayed it; pin its project binding
		// so a delete authorized under this project cannot land on a task
		// another client rebound elsewhere while the pane was open (#3230).
		err := removeTaskThroughDaemon(tsk.ID, task.ExpectProject(tsk))
		m.noteRemoteRemove(tsk, err)
		if err != nil {
			if apiclient.IsMutationCommitted(err) {
				// The durable removal landed and the reload below will project
				// it. Keep the schedule failure visible without telling the user
				// to retry a deletion that already happened.
				log.WarningLog.Printf("task removal committed but schedule refresh failed: %v", err)
				saveErr = errors.Join(saveErr, fmt.Errorf(
					"task %q was removed, but the daemon could not refresh its schedules: %w", tsk.Name, err))
				continue
			}
			// A removal that may have landed is not reported as a failure
			// (#4824); the reload below shows whether the task is still there.
			if mutationOutcomeUnknown(err) {
				log.WarningLog.Printf("task removal outcome unknown: %v", err)
				saveErr = errors.Join(saveErr, mutationOutcomeError(
					fmt.Sprintf("removing task %q", tsk.Name), "the task list", err))
				continue
			}
			log.ErrorLog.Printf("failed to remove task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to remove task %q: %w", tsk.Name, err))
		}
	}
	// Reload BOTH panes from disk so the TaskPane and sidebar can never diverge
	// (#934): whatever actually committed, both panes now show it. The TaskPane
	// reload is unconditional because SetTasks keeps the drafts of failed edits
	// (#4487); skipping it while one was retained hid a task whose deletion
	// failed alongside it (#4257).
	//
	// Scope the reload by the TUI's ACTIVE project (m.repoRoot/m.repoID), not
	// the process cwd, so an edit saved after an in-place project switch
	// (#1461) keeps the switched-to project's rows rather than being clobbered
	// by the launch repo's list — the same scope the snapshot poll reads. A
	// retarget that crossed repos is intentionally outside this list and is
	// surfaced below so the silent drop does not look like a failed save
	// (#1836, mirroring the cross-repo create path's notice in home_tasks.go).
	tasks, err := m.activeTasks(m.repoRoot, m.repoID)
	if err == nil {
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
		// The task count feeds the rail's automations-section height (#1126);
		// reflow so an add/delete grows or shrinks the section immediately.
		m.relayout()
		// A retargeted task whose ID the reloaded list no longer carries moved
		// out of the active scope: the save committed and the task lives at its
		// new project, but the pane drops its row. Echo the new binding the way
		// the CLI does (#1891) — without this, the silent drop looks like a
		// failed save and the user re-edits a task they can no longer see.
		// Subdirectory/path retargets that stay in the active repo stay in the
		// list and produce no notice.
		if len(retargetCandidates) > 0 {
			inScope := make(map[string]bool, len(tasks))
			for _, t := range tasks {
				inScope[t.ID] = true
			}
			for _, rc := range retargetCandidates {
				if inScope[rc.id] {
					continue
				}
				sp.RecordCrossRepoRetarget(rc.name, rc.newPath)
			}
		}
	} else {
		saveErr = errors.Join(saveErr, fmt.Errorf("failed to reload tasks after save: %w", err))
		// The reload failed, so the in-scope list above is unavailable to
		// classify each retarget. The save already committed the new
		// ProjectPath, and the next successful snapshot poll will drop a
		// cross-repo retarget's row from the pane — without recording the
		// notice here, the task vanishes with no explanation (the same #1836
		// silent drop this change exists to surface). Classify the destination
		// directly from the committed path: a retarget whose newPath resolves
		// to a different repo than the active scope has left the pane, so echo
		// its binding the way the successful-reload branch does. A same-repo
		// subdirectory retarget resolves to the same id and produces no
		// notice, matching the in-scope check above; with no active scope
		// (registry mode) every retarget leaves the pane and is announced,
		// matching the empty list LoadTasksForKnownRepo returns there.
		for _, rc := range retargetCandidates {
			if m.repoID != "" && sameScopeAfterSave(m, rc.newPath) {
				continue
			}
			sp.RecordCrossRepoRetarget(rc.name, rc.newPath)
		}
	}
	if failedEdit {
		m.recovery = &recoveryNotice{"Cannot save task", "Your changes are retained. " + saveErr.Error(), "Press any key to continue."}
	}
	return saveErr
}

// updateProvedTaskDeleted reports whether a failed task update positively says
// the task does not exist. Only the daemon's own answer counts: an unconfirmed
// HTTP response (a proxy's 404 standing in for a lost reply), an unserved
// route, or a transport failure says nothing about the task, and a committed
// mutation proves the opposite. Anything short of a positive answer keeps the
// draft and reports the failure as before (#4798).
func updateProvedTaskDeleted(err error, id string) bool {
	var unconfirmed *apiclient.UnconfirmedHTTPResponseError
	var notServed *apiclient.RouteNotServedError
	var transport *apiclient.TransportError
	if errors.As(err, &unconfirmed) || errors.As(err, &notServed) || errors.As(err, &transport) ||
		apiclient.IsMutationCommitted(err) {
		return false
	}
	return task.IsNotFound(err, id)
}

// saveInRepoPostWorktreeCommandsFn is indirected so TUI tests can force a
// hooks-save failure deterministically — without relying on filesystem
// permission tricks that a root test runner would bypass (#1001).
var saveInRepoPostWorktreeCommandsFn = config.SaveInRepoPostWorktreeCommands

// sameScopeAfterSave reports whether a saved task path still belongs to the
// active project. Remotely the path is on the daemon's host and is compared
// textually; a local git probe of it would describe the wrong machine.
func sameScopeAfterSave(m *home, newPath string) bool {
	if isRemoteTarget() {
		return newPath == m.repoRoot
	}
	return config.RepoIDForPath(newPath) == m.repoID
}
