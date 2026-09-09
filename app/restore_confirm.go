package app

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
)

// restoreIfResting turns an Enter/`o` on a resting (Lost/Dead/archived) row into a
// restore instead of interactiveGuard's "press <key> to restore" fence: the gesture
// the user already made expresses the intent, so acting on it is more intuitive than
// making them read the fence and press the key themselves (#2489/#2479). It reports
// whether it took the row. LifecycleActionRestore is true for EXACTLY the resting states the guard
// fences (LiveLost/LiveDead/LiveArchived, with a stable id and no teardown/replace
// op), so a live, tearing-down, id-less, or startup-unknown row returns false and
// falls through to the guard's own message unchanged.
//
// Taken rows are NOT subclassed by op: lifecycleActionFor returns Restore for
// LiveLost/LiveDead/LiveArchived regardless of an in-flight restore, so a row
// already mid-restore (OpRestoring) still reports LifecycleActionRestore and would
// otherwise share the destructive branch with an OpNone row. It can neither be
// re-restored nor re-confirmed, so it bails here and falls through to
// interactiveGuard's "is being restored" notice — the same surface `r`
// (handleRestore) and the safe local/archived path already produce.
//
// A restore is dispatched IMMEDIATELY (no confirmation) only where it is provably
// safe: a local session re-spawns its tmux in place, and an archived session
// restores from the branch the archive already pushed. But a Lost/Dead REMOTE
// session's restore may re-provision a fresh sandbox after session-specific dead
// evidence, discarding any unpushed work on the old one (#1794) — so that one is
// gated behind a confirmation that names the risk. Mere unreachability blocks the
// replacement. Enter, `o`, and a mouse double-click all funnel here, so none can
// silently discard work.
//
// Scope: the tree ROW verbs and the pane-preview commit reached from them. The
// focused-pane guards (activateInteractive, handleEnterPane) are a distinct, rarer
// interaction — a focused pane whose session went Lost out from under it — and keep
// the guard message; `o` and Enter agree there too (both error).
func (m *home) restoreIfResting(selected *session.Instance) (tea.Cmd, bool) {
	if selected == nil || selected.LifecycleAction() != session.LifecycleActionRestore {
		return nil, false
	}
	if selected.GetInFlightOp() == session.OpRestoring {
		// A restore is already in flight: interactiveGuard's "is being
		// restored" notice names the real state — the same surface `r`
		// (handleRestore) and the safe local/archived path already produce.
		// Do not re-open the reprovisioning confirm: it would show misleading
		// data-loss copy for a row whose restore is already running, and its
		// confirm-callback re-check would no-op silently.
		return nil, false
	}
	if selected.RestoreWouldDiscardUnpushedWork() {
		return m.confirmReprovisioningRestore(selected), true
	}
	// Safe (local re-spawn, or archived restore from the pushed branch): act at
	// once. handleRestore is the single owner of the restore transition and its
	// own already-restoring guard.
	_, cmd := m.handleRestore()
	return cmd, true
}

// confirmReprovisioningRestore opens the confirmation that fences a remote
// Lost/Dead restore behind an explicit yes, because it can re-provision a fresh
// sandbox and discard unpushed work (#2489 review / #1794). On confirm it raises
// the optimistic OpRestoring and dispatches the restore off the event loop via
// startRestoreMsg, re-checking identity and state at that boundary in case a
// snapshot healed or changed the row while the dialog was up.
func (m *home) confirmReprovisioningRestore(selected *session.Instance) tea.Cmd {
	title := selected.Title
	target := captureSessionActionTarget(selected, m.repoID)
	message := fmt.Sprintf("Restore sandbox session '%s'?\n\nReconnects if live. A dead agent's sandbox is pushed first.\nAn absent sandbox is restored from its last pushed state — never-pushed changes are lost.\nRestore refuses when reachability is uncertain.", title)
	return m.confirmAction(message, func() tea.Msg {
		inst := m.resolveSessionActionTarget(target)
		if inst == nil {
			return nil
		}
		if inst.LifecycleAction() != session.LifecycleActionRestore {
			return nil
		}
		if inst.GetInFlightOp() == session.OpRestoring {
			return nil
		}
		_ = inst.Transition(session.MarkRestoring())
		return startRestoreMsg{target: target}
	})
}
