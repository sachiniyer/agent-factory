package app

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
)

// handleDefaultKeyPress handles key events in stateDefault (main interaction state).
func (m *home) handleDefaultKeyPress(msg tea.KeyMsg, name keys.KeyName) (tea.Model, tea.Cmd) {
	switch name {
	case keys.KeyHelp:
		return m.showHelpScreen(helpTypeGeneral{}, nil)

	// Tree navigation
	case keys.KeyUp:
		m.sidebar.Up()
		return m, m.selectionChanged()
	case keys.KeyDown:
		m.sidebar.Down()
		return m, m.selectionChanged()
	case keys.KeyLeft:
		m.sidebar.CollapseSection()
		return m, m.selectionChanged()
	case keys.KeyRight:
		m.sidebar.ExpandSection()
		return m, m.selectionChanged()
	case keys.KeyNextSection:
		m.sidebar.JumpNextSection()
		return m, m.selectionChanged()
	case keys.KeyPrevSection:
		m.sidebar.JumpPrevSection()
		return m, m.selectionChanged()

	// Instance creation
	case keys.KeyNewRemote:
		return m.startNewInstance(true)

	case keys.KeyNew:
		return m.startNewInstance(false)

	case keys.KeyTaskList:
		// Open the task manager overlay (task creation lives on its `n` key —
		// `s` became the split verb in #1024 PR 5). The in-rail automations
		// section stays a compact summary; the manager gets a centered modal
		// so its form is never clamped into the narrow rail.
		return m.showTasksOverlay()

	// N-pane verbs (#1088): s opens the selected tab as a pane (or focuses
	// its pane); x hides the focused pane back to the background.
	case keys.KeyOpenPane:
		return m.handleOpenPane()
	case keys.KeyHidePane:
		return m.handleHidePane()

	case keys.KeySearch:
		return m.showSearchOverlay()

	// Hooks configuration (#1024 PR 4: an overlay, not a sidebar slot)
	case keys.KeyHooks:
		return m.showHooksOverlay()

	// PR actions
	case keys.KeyOpenPR:
		return m.handleOpenPR()
	case keys.KeyCopyPR:
		return m.handleCopyPR()

	// Scrolling (each pane scrolls its own view, #1088)
	case keys.KeyShiftUp:
		if pane, _ := m.focusedContentPane(); pane != nil {
			pane.ScrollUp()
		}
		return m, m.selectionChanged()
	case keys.KeyShiftDown:
		if pane, _ := m.focusedContentPane(); pane != nil {
			pane.ScrollDown()
		}
		return m, m.selectionChanged()

	// Focus ring (#1088): Tab cycles tree → open panes (in order) →
	// automations. Tabs themselves are reached via the tree and the 1-9
	// jump keys.
	case keys.KeyTab:
		return m, m.cycleFocus(false)
	case keys.KeyShiftTab:
		return m, m.cycleFocus(true)

	// Tab lifecycle (#930 PR 4): `t` creates, `w` kills the selection's
	// active tab. Hiding a PANE is `x` — `w` keeps its kill meaning
	// everywhere (§2.3).
	case keys.KeyNewTab:
		return m.handleNewTab()
	case keys.KeyCloseTab:
		return m.handleCloseTab()

	// Instance actions
	case keys.KeyKill:
		return m.handleKill()
	case keys.KeyArchive:
		return m.handleArchive()
	case keys.KeyEnter:
		return m.handleEnter()
	case keys.KeyAttach:
		return m.handleAttach()
	case keys.KeyQuit:
		return m.handleQuit()

	default:
		return m, nil
	}
}

// handleKill handles the kill/delete session action. The confirmation only
// flips the row to Deleting; the slow teardown (remote delete_cmd over ssh,
// tmux kill, worktree removal) runs in killInstanceCmd's background goroutine
// so the event loop never blocks on it (#844).
func (m *home) handleKill() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.GetStatus() == session.Loading {
		return m, nil
	}
	if selected.GetStatus() == session.Deleting {
		return m, m.handleError(fmt.Errorf("session '%s' is already being deleted", selected.Title))
	}

	// Capture the title at confirmation time so that background tick events
	// cannot change which instance we operate on.
	selectedTitle := selected.Title

	// Runs synchronously in the confirmation overlay's OnConfirm, on the
	// event loop — keep it fast. Marking Deleting here (not in the background
	// cmd) guarantees the row is visibly deleting and kill/attach are fenced
	// off before any other event can be processed.
	killAction := func() tea.Msg {
		for _, inst := range m.store.GetInstances() {
			if inst.Title == selectedTitle {
				inst.SetStatus(session.Deleting)
				return startKillMsg{title: selectedTitle}
			}
		}
		// The row was removed (e.g. by an external kill picked up by the
		// background refresh) while the dialog was open; nothing to do.
		return nil
	}

	// Check for uncommitted changes in the worktree (skip for remote sessions
	// which have no local worktree).
	warning := ""
	if !selected.IsRemote() {
		if wt := selected.GetWorktreePath(); wt != "" {
			warning = killConfirmationWarning(wt)
		}
	}

	message := fmt.Sprintf("[!] Kill session '%s'?", selectedTitle)
	if warning != "" {
		message += "\n\n" + warning
	}
	return m, m.confirmAction(message, killAction)
}

// killInstanceCmd returns a tea.Cmd that performs the actual session teardown
// off the event loop. The daemon RPC blocks for the whole teardown — for
// remote instances delete_cmd often runs over ssh and takes tens of seconds —
// which is exactly why this must not run on the Update goroutine (#844). The
// teardown itself (delete_cmd → tmux kill → worktree removal, #802 ordering)
// is unchanged: it still goes through daemon.KillSession, which also keeps
// the title blocked against reuse until the teardown completes.
func (m *home) killInstanceCmd(title string) tea.Cmd {
	repoID := m.repoID
	// Capture the kill seam on the event loop, before the goroutine: it is a
	// package var swapped by test seams, so reading it inside the cmd goroutine
	// would race a sibling parallel test's swap (#960 PR 4 race-fix class).
	kill := killSessionThroughDaemon
	return func() tea.Msg {
		// The shell tab's tmux session is owned by the instance and torn down by
		// LocalBackend.Kill (looping all tabs) inside the daemon teardown — there
		// is no longer a UI-side terminal cache to clean up (#930 PR 2).
		if err := kill(title, repoID); err != nil {
			log.ErrorLog.Printf("could not kill instance: %v", err)
			return instanceKilledMsg{title: title, err: err}
		}
		return instanceKilledMsg{title: title}
	}
}

// handleInstanceKilled finalizes an async kill on the event loop. On success
// the row is removed from the sidebar (the daemon already deleted the disk
// record). On failure the row flips back to Ready so the user can retry the
// kill, and the underlying error — the evidence, per #797 — lands in the
// error box.
func (m *home) handleInstanceKilled(msg instanceKilledMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		for _, inst := range m.store.GetInstances() {
			if inst.Title == msg.title && inst.GetStatus() == session.Deleting {
				inst.SetStatus(session.Ready)
				break
			}
		}
		return m, m.handleError(fmt.Errorf("failed to kill session '%s': %w", msg.title, msg.err))
	}

	// The TUI's in-memory instance is untouched by the daemon-side teardown,
	// so its repo name is still resolvable here for repo-section bookkeeping.
	// The row may already be gone when the background refresh noticed the
	// deleted disk record first — both removals are no-ops then.
	var repoName string
	var repoErr error = fmt.Errorf("instance not found")
	for _, inst := range m.store.GetInstances() {
		if inst.Title == msg.title {
			repoName, repoErr = inst.RepoName()
			break
		}
	}
	if repoErr == nil {
		m.store.RemoveInstanceByTitleWithRepo(msg.title, repoName)
	} else {
		m.store.RemoveInstanceByTitle(msg.title)
	}
	return m, m.selectionChanged()
}

// handleArchive is the archive/restore verb (#1028, `A`): on an archived row it
// restores the session (non-destructive — no confirm); on a live row it archives
// it behind a confirmation, since archive tears down tmux and relocates the
// worktree. Remote and in-place sessions can't be archived (no relocatable
// worktree) and are rejected up front with an immediate message; the daemon
// enforces the same rules authoritatively.
func (m *home) handleArchive() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.GetStatus() == session.Loading {
		return m, nil
	}
	if selected.GetStatus() == session.Deleting {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	title := selected.Title

	// Archived row → restore. No confirmation: restore only moves the worktree
	// back and re-spawns the agent.
	if selected.GetStatus() == session.Archived {
		return m, m.restoreInstanceCmd(title)
	}

	// Live row → archive. Fail fast on the unarchivable session kinds for a
	// snappy message (the daemon rejects them too).
	if selected.IsRemote() {
		return m, m.handleError(fmt.Errorf("cannot archive remote session '%s': it has no local worktree to relocate", title))
	}
	if selected.IsExternalWorktree() {
		return m, m.handleError(fmt.Errorf("cannot archive in-place session '%s': archive relocates the worktree, which it doesn't own", title))
	}

	message := fmt.Sprintf("[!] Archive session '%s'?\n\nIts tmux is torn down and its worktree is moved out to the archive directory (branch + uncommitted changes preserved). Restore later with A.", title)
	return m, m.confirmAction(message, func() tea.Msg { return startArchiveMsg{title: title} })
}

// archiveInstanceCmd runs the daemon archive (tmux teardown + worktree move) off
// the event loop (#1028), mirroring killInstanceCmd — the RPC blocks for the
// whole operation, so it must not run on the Update goroutine.
func (m *home) archiveInstanceCmd(title string) tea.Cmd {
	repoID := m.repoID
	archive := archiveSessionThroughDaemon
	return func() tea.Msg {
		if _, err := archive(title, repoID); err != nil {
			log.ErrorLog.Printf("could not archive instance %q: %v", title, err)
			return instanceArchivedMsg{title: title, err: err}
		}
		return instanceArchivedMsg{title: title}
	}
}

// restoreInstanceCmd runs the daemon restore (worktree move-back + agent
// re-spawn) off the event loop (#1028).
func (m *home) restoreInstanceCmd(title string) tea.Cmd {
	repoID := m.repoID
	restore := restoreArchivedThroughDaemon
	return func() tea.Msg {
		if _, err := restore(title, repoID); err != nil {
			log.ErrorLog.Printf("could not restore instance %q: %v", title, err)
			return instanceRestoredMsg{title: title, err: err}
		}
		return instanceRestoredMsg{title: title}
	}
}

// handleInstanceArchived finalizes an async archive. On success the row's new
// Archived status arrives via the next daemon Snapshot reconcile, which
// re-partitions it into the collapsed Archived folder — nothing to do here but
// refresh selection; on failure the error lands in the error box.
func (m *home) handleInstanceArchived(msg instanceArchivedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.handleError(fmt.Errorf("failed to archive session '%s': %w", msg.title, msg.err))
	}
	return m, m.selectionChanged()
}

// handleInstanceRestored finalizes an async restore. On success the row returns
// to the live Instances section via the next Snapshot reconcile; on failure the
// error (e.g. the origin repo is gone) lands in the error box.
func (m *home) handleInstanceRestored(msg instanceRestoredMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.handleError(fmt.Errorf("failed to restore session '%s': %w", msg.title, msg.err))
	}
	return m, m.selectionChanged()
}

// killConfirmationWarning returns the data-loss warning line for the kill
// confirmation dialog, or "" if the worktree at wt is verifiably clean. Kill
// tears the worktree down with `git worktree remove -f`, which bypasses git's
// own refusal to delete a dirty worktree, so this check is the only warning
// the user gets. If `git status` itself fails we cannot prove the worktree is
// clean — fail closed and warn that changes may be lost rather than silently
// skipping the warning (#815).
func killConfirmationWarning(wt string) string {
	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if err != nil {
		log.WarningLog.Printf("could not verify worktree status for %s before kill: %v", wt, err)
		return fmt.Sprintf("WARNING: Could not verify worktree status (%v); it may contain uncommitted changes that will be lost!", err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return "WARNING: This worktree has uncommitted changes that will be lost!"
	}
	return ""
}

// handleEnter is the Enter verb (#1089 PR 2, RFC §2.3): enter INTERACTIVE
// mode on the pane — every keystroke forwards to the agent/shell in place,
// no full-screen takeover, Ctrl-] returns to nav. With a workspace pane
// focused it enters that pane; on a tree instance/tab row it opens (or
// focuses) the selection's pane first, exactly like `s`, then enters it.
// Bindings that cannot embed — remote instances, whose only local terminal
// is the full-screen hook PTY — fall back to the full-screen attach Enter
// used to do; dead/transitional sessions keep their guard errors.
func (m *home) handleEnter() (tea.Model, tea.Cmd) {
	if p := m.focusedOpenPane(); p != nil {
		if instErr := interactiveGuard(p.Instance()); instErr != nil {
			return m, m.handleError(instErr)
		}
		if p.Instance() == nil || p.Instance().GetStatus() == session.Loading {
			return m, nil
		}
		if liveSessionName(p.Instance(), p.Tab()) == "" {
			// Not embeddable (remote): the old full-screen behavior.
			return m.handleEnterPane(p)
		}
		return m.requestInteractive(p)
	}
	sel := m.sidebar.GetSelection()

	// Toggle expandable section headers (Instances and the Archived folder).
	if sel.IsHeader && (sel.Kind == ui.SectionInstances || sel.Kind == ui.SectionArchived) {
		m.sidebar.ToggleSection()
		return m, m.selectionChanged()
	}
	// Instance selected — in either the Instances tree or the Archived folder
	// (#1028). An archived row is not embeddable: interactiveGuard surfaces the
	// "restore it first" error before any pane/attach path is reached.
	if sel.Kind == ui.SectionInstances || sel.Kind == ui.SectionArchived {
		selected := m.sidebar.GetSelectedInstance()
		if selected == nil || selected.GetStatus() == session.Loading {
			return m, nil
		}
		if err := interactiveGuard(selected); err != nil {
			return m, m.handleError(err)
		}
		if liveSessionName(selected, m.store.ActiveTab()) == "" {
			// Not embeddable (remote): the old full-screen attach flow.
			return m.attachSelected(selected)
		}
		// Open (or focus) the selection's pane — the `s` semantics — then
		// enter it. The pane pointer is captured here, at Enter-press time
		// (#716), for the deferred activation.
		_, cmd := m.openOrFocusPane(selected, m.store.ActiveTab())
		p := m.store.FindOpenPane(selected, m.store.ActiveTab())
		if p == nil {
			return m, cmd
		}
		mod, interactCmd := m.requestInteractive(p)
		return mod, tea.Batch(cmd, interactCmd)
	}
	return m, nil
}

// interactiveGuard returns the user-facing error that fences Enter off a
// session in a state it cannot be entered or attached in — shared by the
// interactive and full-screen paths (the #935 dead-tmux error, the Deleting
// fence, the #1108 Lost fence). A nil error does NOT mean embeddable (see
// liveSessionName); nil instance and Loading are the caller's silent no-op
// cases.
func interactiveGuard(inst *session.Instance) error {
	if inst == nil || inst.GetStatus() == session.Loading {
		return nil
	}
	if inst.GetStatus() == session.Deleting {
		return fmt.Errorf("session '%s' is being deleted", inst.Title)
	}
	if inst.GetStatus() == session.Lost {
		// Lost (#1108): the backing tmux session vanished with no kill on
		// record. Entering or attaching is impossible right now; say what
		// happened — same explicit-feedback contract as the Deleting path
		// (#935). Checked before TmuxAlive so the specific message wins.
		return fmt.Errorf("session '%s' was lost — its tmux session is gone", inst.Title)
	}
	if inst.GetStatus() == session.Archived {
		// Archived (#1028): the user tore the session down and its worktree was
		// moved to the global archive dir; there is no tmux to enter or attach.
		// Point at the off-ramp (restore) rather than a bare "not running" —
		// same explicit-feedback contract as Lost/Deleting. Checked before
		// TmuxAlive so the specific message wins.
		return fmt.Errorf("session '%s' is archived — restore it first (af sessions restore %s)", inst.Title, inst.Title)
	}
	if !inst.TmuxAlive() {
		return fmt.Errorf("session '%s' is no longer running", inst.Title)
	}
	return nil
}

// handleAttach is the full-screen attach verb (`o`; the pre-#1089-PR-2
// Enter): attach the FOCUSED pane's (instance, tab) full-screen (#1088), or
// the tree selection's. The attach path itself is untouched: the same
// attachOverlayCallbackFn seam, `attached` gate, and SIGKILL-bounded detach.
func (m *home) handleAttach() (tea.Model, tea.Cmd) {
	if p := m.focusedOpenPane(); p != nil {
		return m.handleEnterPane(p)
	}
	sel := m.sidebar.GetSelection()
	// Accept both the Instances tree and the Archived folder (#1028): an
	// archived row is non-attachable, and interactiveGuard surfaces the
	// "restore it first" error rather than a silent no-op.
	if sel.IsHeader || (sel.Kind != ui.SectionInstances && sel.Kind != ui.SectionArchived) {
		return m, nil
	}
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.GetStatus() == session.Loading {
		return m, nil
	}
	if err := interactiveGuard(selected); err != nil {
		return m, m.handleError(err)
	}
	return m.attachSelected(selected)
}

// attachSelected runs the tree-selection full-screen attach flow for a
// guarded (non-nil, non-Loading, non-Deleting, tmux-alive) instance.
//
// The instance is captured at keypress time — the synchronous moment the
// selection is provably current. For first-time attachers the attach is
// deferred until the help overlay is dismissed, and a background refresh can
// drift the selection onto a different instance in the meantime; the
// callbacks must attach to this captured instance, not re-read the live
// selection (#716).
func (m *home) attachSelected(selected *session.Instance) (tea.Model, tea.Cmd) {
	if activeTab := m.store.ActiveTab(); activeTab != 0 {
		// The terminal tab attaches a local tmux session for local
		// instances, but a remote instance's terminal_cmd PTY for remote
		// ones (#843) — and that remote PTY hands the terminal back via
		// session.hookAttachTerminalRestore (main screen, modes off), the
		// same neutral state the sidebar remote attach leaves. So the
		// post-detach handling must key off the instance's real
		// remote-ness, exactly like the sidebar path below: a remote
		// terminal_cmd detach needs the #845/#848 full reset + reassert,
		// or the TUI keeps rendering on the main screen (#889).
		// Capture the active tab index at keypress time alongside the
		// instance (#716): a background refresh could otherwise drift the
		// selection — or the user could change tabs while the help overlay is
		// open — before the deferred attach callback runs.
		return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
			return attachOverlayCallbackFn(m, selected.Title, "handleEnter-terminal", "", selected.IsRemote(), func() (chan struct{}, error) {
				return ui.AttachTerminalTab(selected, activeTab)
			})
		})
	}
	return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
		return attachOverlayCallbackFn(m, selected.Title, "handleEnter-sidebar", "", selected.IsRemote(), func() (chan struct{}, error) {
			return m.store.AttachInstance(selected)
		})
	})
}

// handleNewTab spawns a new shell tab in the selected instance and selects it
// (#930 PR 4). Single keypress, no prompt: the tab runs $SHELL in the instance's
// worktree. Remote instances have no local worktree and the hook protocol has no
// run-arbitrary-command verb, so new-tab is unsupported there: a remote session's
// only terminal tab is the one derived from remote_hooks.terminal_cmd (#930 PR 6).
//
// The spawn+persist is routed through the daemon's CreateTab RPC (#960): the
// daemon — the single writer — owns the new tab so its authoritative view holds
// it and the TUI no longer originates a tab write at all (#959). The TUI reflects
// the daemon-created tab locally via AttachShellTab for instant display (it
// reconnects to the session the daemon spawned, never a second colliding spawn);
// the snapshot reconcile (PR 3) keeps it mirrored thereafter. The daemon's soft
// cap (max tabs) error is surfaced verbatim.
func (m *home) handleNewTab() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if status := selected.GetStatus(); status == session.Loading || status == session.Deleting {
		return m, nil
	}
	if selected.IsRemote() {
		return m, m.handleError(fmt.Errorf("remote sessions don't support new process tabs; their terminal tab comes from remote_hooks.terminal_cmd and arbitrary remote processes aren't supported"))
	}

	name, err := createShellTabThroughDaemon(selected.Title, m.repoID)
	if err != nil {
		return m, m.handleError(err)
	}
	// The daemon spawned and persisted the tab; reflect it locally for instant
	// display without a second spawn. The daemon write is authoritative, so the
	// TUI never saves (#960 PR 4).
	if _, attachErr := selected.AttachShellTab(name); attachErr != nil {
		return m, m.handleError(attachErr)
	}

	// Select the fresh tab in the tree and open it as a pane (#1088): the
	// pre-N-pane behavior showed the new tab in the workspace immediately,
	// and the issue's canonical flow — agent pane + terminal pane side by
	// side for one instance — is exactly `s` then `t`.
	newIdx := len(tree.TabLabels(selected)) - 1
	m.store.SetActiveTab(newIdx)
	m.menu.SetActiveTab(newIdx)
	m.sidebar.SyncCursorToActiveTab()
	return m.openOrFocusPane(selected, newIdx)
}

// handleCloseTab closes the active tab of the selected instance and selects the
// previous (left) tab (#930 PR 4). The agent tab (index 0) is unclosable — w on
// it is a gentle no-op message pointing at D for killing the whole session. A
// remote instance's tabs (agent + optional terminal_cmd terminal) are fixed by
// its hook config, not user-managed, so closing any of them is refused.
//
// The kill+persist is routed through the daemon's CloseTab RPC (#960): the
// daemon — the single writer — kills the tab's tmux and persists the shrunk
// list, so the TUI no longer originates a tab write at all (#959). The agent-tab
// and remote rules are still enforced TUI-side so the friendly message shows
// without a round-trip (the RPC enforces them too). The TUI drops the now-dead
// tab locally via DropClosedTab — a no-kill removal, since the daemon already
// tore the tmux session down.
func (m *home) handleCloseTab() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	idx := m.store.ActiveTab()
	if idx <= 0 {
		return m, m.handleError(fmt.Errorf("the agent tab can't be closed; use D to kill the session"))
	}
	if selected.IsRemote() {
		return m, m.handleError(fmt.Errorf("remote session tabs can't be closed"))
	}
	tabs := selected.GetTabs()
	if idx >= len(tabs) {
		return m, m.handleError(fmt.Errorf("tab cannot be closed"))
	}
	tabName := tabs[idx].Name
	// Capture the slot→name list before the drop: reconcilePanesForTabs maps
	// the open panes' bindings across the change by tab name (#1088).
	oldNames := paneTabNames(selected)

	if err := closeTabThroughDaemon(selected.Title, m.repoID, tabName); err != nil {
		return m, m.handleError(err)
	}
	// The daemon killed the tmux and persisted the shrunk list; drop the
	// now-dead tab locally without re-killing. The daemon write is
	// authoritative, so the TUI never saves (#960 PR 4).
	if dropErr := selected.DropClosedTab(idx); dropErr != nil {
		return m, m.handleError(dropErr)
	}

	// The kill shifts every higher tab slot down by one, so the open panes
	// bound to this instance must follow (#1088): the killed tab's pane
	// leaves the workspace, higher-slot panes re-bind so they keep showing
	// the same tab. Shared with the daemon-snapshot reconcile, which applies
	// the identical semantics when a tab disappears out-of-band.
	if m.reconcilePanesForTabs(selected, oldNames) {
		m.relayout()
	}

	// Prefer the left/previous neighbor (idx >= 1, so idx-1 >= 0).
	m.store.SetActiveTab(idx - 1)
	m.clampSelectionTab()
	m.menu.SetActiveTab(m.store.ActiveTab())
	m.sidebar.SyncCursorToActiveTab()
	return m, m.selectionChanged()
}

// handleTabJump jumps the selection's active tab to a 1-based tab number (the
// 1-9 number keys). Out-of-range numbers are a no-op (#930 PR 4). When the
// sidebar cursor rests on one of the selected instance's tab rows, it follows
// the jump so the tree can't disagree; the jumped-to tab is what `s` opens
// and Enter attaches (#1088 — panes are explicit bindings, so the jump moves
// the selection, not any open pane).
func (m *home) handleTabJump(oneBased int) (tea.Model, tea.Cmd) {
	idx := oneBased - 1
	if idx < 0 || idx >= len(tree.TabLabels(m.store.GetSelectedInstance())) {
		return m, nil
	}
	m.store.SetActiveTab(idx)
	m.menu.SetActiveTab(idx)
	m.sidebar.SyncCursorToActiveTab()
	return m, m.selectionChanged()
}

// noPRForSessionErr is the actionable message surfaced when p/P is pressed on a
// session that has no PR yet, so the key press is never a silent no-op (#1170).
var noPRForSessionErr = fmt.Errorf("no PR for this session yet — push a branch / open a PR first")

// handleOpenPR opens the PR URL in the browser.
func (m *home) handleOpenPR() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.GetPRInfo() == nil {
		return m, m.handleError(noPRForSessionErr)
	}
	url := selected.GetPRInfo().URL
	var openCmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		openCmd = exec.Command("open", url)
	} else {
		openCmd = exec.Command("xdg-open", url)
	}
	if err := openCmd.Start(); err != nil {
		return m, m.handleError(fmt.Errorf("failed to open PR: %w", err))
	}
	// Reap the opener when it exits so it doesn't linger as a zombie for the
	// life of the TUI (#816).
	go func() {
		_ = openCmd.Wait()
	}()
	return m, nil
}

// handleCopyPR copies the PR URL to the clipboard.
func (m *home) handleCopyPR() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.GetPRInfo() == nil {
		return m, m.handleError(noPRForSessionErr)
	}
	url := selected.GetPRInfo().URL
	var copyCmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		copyCmd = exec.Command("pbcopy")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			copyCmd = exec.Command("wl-copy")
		} else {
			copyCmd = exec.Command("xclip", "-selection", "clipboard")
		}
	}
	copyCmd.Stdin = strings.NewReader(url)
	if err := copyCmd.Run(); err != nil {
		return m, m.handleError(fmt.Errorf("failed to copy PR URL: %w", err))
	}
	return m, nil
}
