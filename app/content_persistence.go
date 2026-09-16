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
	// Save any dirty task/hooks state. On failure the panes were reloaded to
	// match disk; abort the quit and surface the error so the user sees the
	// dropped edit instead of losing it silently on the way out.
	if err := m.saveContentPaneState(); err != nil {
		return m, m.handleError(err)
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
// retry. The sidebar still reloads committed data; the editor remains the draft.
func (m *home) saveContentPaneState() error {
	// Accumulate failures across both panes so a hooks error and a task error
	// can never clobber one another (#1001).
	var saveErr error
	failedEdit := false
	// Deletions whose removal did not commit. Their restore decision is settled
	// after the reload below, against the freshly loaded repo-scoped set.
	var failedDeletes []task.Task

	hp := m.hooksPane
	if hp.IsDirty() {
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
	for _, edit := range sp.ConsumeDirty() {
		if err := updateTaskThroughDaemon(edit.ID, edit.Update, edit.Expect); err != nil {
			if apiclient.IsMutationCommitted(err) {
				// The task write landed; only the daemon's schedule refresh
				// failed. Keep surfacing that failure, but advance this task's
				// baseline so a later edit is diffed against durable state.
				sp.AcknowledgeSavedEdit(edit.ID)
			} else {
				sp.RestoreFailedEdit(edit.ID)
				failedEdit = true
			}
			log.ErrorLog.Printf("failed to update task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to save task %q: %w", edit.ID, err))
			continue
		}
		sp.AcknowledgeSavedEdit(edit.ID)
	}
	for _, tsk := range sp.ConsumeDeleted() {
		// tsk is the record as the pane displayed it; pin its project binding
		// so a delete authorized under this project cannot land on a task
		// another client rebound elsewhere while the pane was open (#3230).
		if err := removeTaskThroughDaemon(tsk.ID, task.ExpectProject(tsk)); err != nil {
			if apiclient.IsMutationCommitted(err) {
				// The durable removal landed and the reload below will project
				// it. Keep the schedule failure visible without telling the user
				// to retry a deletion that already happened.
				log.WarningLog.Printf("task removal committed but schedule refresh failed: %v", err)
				sp.AcknowledgeDeletedRestored(tsk.ID)
				saveErr = errors.Join(saveErr, fmt.Errorf(
					"task %q was removed, but the daemon could not refresh its schedules: %w", tsk.Name, err))
				continue
			}
			log.ErrorLog.Printf("failed to remove task: %v", err)
			// Whether to restore this row depends on whether the record still
			// belongs to this repo — which the ERROR cannot answer. Defer the
			// decision to the reload below, which loads the repo-scoped set and
			// can simply be asked. Deriving it from the error text was wrong in
			// both directions: an intermediary's "404 page not found" claimed an
			// absence that never happened, and a rebind WITHIN this repo (root
			// to a subdirectory or a linked worktree) produces the project-rebind
			// error while repoScope.matches still resolves the task into this
			// repo by identity (task/repo_scope.go:76-98), so a task that is
			// still listed was treated as gone and dropped from the pane.
			failedDeletes = append(failedDeletes, tsk)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to remove task %q: %w", tsk.Name, err))
		} else {
			// Deletion committed cleanly: if this task was previously restored
			// into s.tasks after a failed attempt, remove it now so it does not
			// stay visible while SetTasks is gated on !failedEdit.
			sp.AcknowledgeDeletedRestored(tsk.ID)
		}
	}
	// Reload the sidebar unconditionally from disk. The TaskPane reload is
	// gated on !failedEdit so user edits survive for retry. This deliberately
	// narrows the #934 invariant ("the two panes can never diverge"): when a
	// save fails and the user's draft must be preserved, keeping the TaskPane
	// in its draft state is the right trade — the sidebar shows committed state
	// while the editor retains the pending edit. The divergence is bounded: it
	// ends when the edit is saved (SetTasks runs) or discarded (SetFocus(false)
	// drops the draft). Any restored-but-deleted row is removed synchronously by
	// AcknowledgeDeletedRestored above, so that class of divergence is closed.
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		// The authoritative answer to "does this record still belong to this
		// repo?" — the same set the sidebar is about to show. Present means the
		// removal genuinely did not land, so restore the row and keep the retry
		// queued; absent means it is gone (deleted elsewhere, or rebound out of
		// this repo), so drop any copy an earlier pass restored rather than
		// showing a row the sidebar does not have.
		// Build an index of the freshly loaded set so we can both decide
		// presence and pass the authoritative record to RestoreFailedDelete.
		// The stale tsk is used only as the retry expectation; the pane
		// displays and baselines against the freshly loaded record so a
		// subsequent user action (e.g. re-pressing D) submits the current
		// binding rather than a never-persisted one that the CAS would refuse.
		//
		// tasks.json permits duplicate IDs in hand-edited stores. When multiple
		// rows share an ID, we cannot identify which freshly-loaded row
		// corresponds to the deleted one; using any of them as the display
		// record would show the wrong row's content in the pane. Count
		// occurrences so RestoreFailedDeleteWithExpect is only given a fresh
		// record when the ID is unambiguous (exactly one match in the reload).
		loadedCount := make(map[string]int, len(tasks))
		loaded := make(map[string]task.Task, len(tasks))
		for _, t := range tasks {
			loadedCount[t.ID]++
			loaded[t.ID] = t
		}
		for _, tsk := range failedDeletes {
			if _, present := loaded[tsk.ID]; present {
				// Restore the authoritative record so the pane and originals
				// are up-to-date; pass the original tsk as the retry
				// expectation so the deletion CAS still pins the binding it
				// was authorised against.
				//
				// When there are duplicate IDs in the freshly loaded set, the
				// map holds only the last occurrence and we cannot tell which
				// one corresponds to the row the user was deleting.
				//
				// tsk itself (the CAS expectation from s.deleted) is also
				// already the LAST duplicate: deleteSelectedTask replaces the
				// selected record with originals[id], which SetTasks keyed by
				// ID and therefore also kept only the last. To recover the
				// exact selected row's content, use the display record captured
				// by deleteSelectedTask before the originals lookup.
				fresh := tsk
				if loadedCount[tsk.ID] == 1 {
					fresh = loaded[tsk.ID]
				} else if display, ok := sp.GetDeletedDisplay(tsk.ID); ok {
					// Duplicate IDs: the display record preserves the actual
					// selected row, independent of the ID-keyed originals map.
					fresh = display
				}
				sp.RestoreFailedDeleteWithExpect(fresh, tsk)
			} else {
				sp.AcknowledgeDeletedRestored(tsk.ID)
			}
		}
		m.store.SetTasks(tasks)
		if !failedEdit {
			sp.SetTasks(tasks)
		}
		// The task count feeds the rail's automations-section height (#1126);
		// reflow so an add/delete grows or shrinks the section immediately.
		m.relayout()
	} else {
		// No authoritative set to consult, so fall back to the conservative
		// answer: keep the rows visible and the retries queued. Dropping a row
		// on an unproven absence is the failure this restore exists to prevent.
		// Use the captured display record if available, so the pane shows the
		// exact selected row rather than the ID-keyed originals entry.
		for _, tsk := range failedDeletes {
			if display, ok := sp.GetDeletedDisplay(tsk.ID); ok {
				sp.RestoreFailedDeleteWithExpect(display, tsk)
			} else {
				sp.RestoreFailedDelete(tsk)
			}
		}
		saveErr = errors.Join(saveErr, fmt.Errorf("failed to reload tasks after save: %w", err))
	}
	if failedEdit {
		m.recovery = &recoveryNotice{"Cannot save task", "Your changes are retained. " + saveErr.Error(), "Press any key to continue."}
	}
	return saveErr
}

// saveInRepoPostWorktreeCommandsFn is indirected so TUI tests can force a
// hooks-save failure deterministically — without relying on filesystem
// permission tricks that a root test runner would bypass (#1001).
var saveInRepoPostWorktreeCommandsFn = config.SaveInRepoPostWorktreeCommands
