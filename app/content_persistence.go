package app

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

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
// Recovery semantics on a task-save failure (#934): we reload BOTH the sidebar
// and the TaskPane from disk so the two panes always agree and always reflect
// the committed on-disk state — never a mix of stale in-memory edits in one
// pane and disk state in the other. Reloading clears the TaskPane's dirty flag,
// which means a failed edit is discarded rather than left dangling; we
// therefore return the error so callers surface it (via handleError) and the
// dropped edit is never silent. We deliberately do NOT keep dirty=true for an
// in-place retry: after the reload the in-memory edits are gone, so a lingering
// dirty flag would point at nothing. The user re-applies the edit from a
// known-consistent state instead.
func (m *home) saveContentPaneState() error {
	// Accumulate failures across both panes so a hooks error and a task error
	// can never clobber one another (#1001).
	var saveErr error

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
	// in-process, so there is no separate ReloadTasks poke here — the write and
	// its schedule refresh are one atomic daemon call (removing the old
	// double-reload).
	//
	// Persist ONLY the tasks the user actually edited (ConsumeDirty), and only
	// the FIELDS they changed: each edit carries a field-level patch (diffed
	// against the copy the pane loaded), so a save of one field never rewrites a
	// field another writer (CLI/daemon) changed out-of-band while the pane was
	// open — the #1700 clobber, of which #1213's whole-task guard was only a
	// partial fix. A patch that turns out empty (edited then reverted) is a
	// harmless no-op the daemon still validates.
	for _, edit := range sp.ConsumeDirty() {
		if err := updateTaskThroughDaemon(edit.ID, edit.Update); err != nil {
			log.ErrorLog.Printf("failed to update task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to save task %q: %w", edit.ID, err))
		}
	}
	for _, tsk := range sp.ConsumeDeleted() {
		if err := removeTaskThroughDaemon(tsk.ID); err != nil {
			log.ErrorLog.Printf("failed to remove task: %v", err)
			saveErr = errors.Join(saveErr, fmt.Errorf("failed to remove task %q: %w", tsk.Name, err))
		}
	}
	// Reload BOTH panes from disk so the TaskPane and sidebar can never diverge
	// (#934): whatever actually committed, both panes now show it.
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
		// The task count feeds the rail's automations-section height (#1126);
		// reflow so an add/delete grows or shrinks the section immediately.
		m.relayout()
	} else {
		saveErr = errors.Join(saveErr, fmt.Errorf("failed to reload tasks after save: %w", err))
	}
	return saveErr
}

// saveInRepoPostWorktreeCommandsFn is indirected so TUI tests can force a
// hooks-save failure deterministically — without relying on filesystem
// permission tricks that a root test runner would bypass (#1001).
var saveInRepoPostWorktreeCommandsFn = config.SaveInRepoPostWorktreeCommands
