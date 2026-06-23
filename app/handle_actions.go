package app

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// handleDefaultKeyPress handles key events in stateDefault (main interaction state).
func (m *home) handleDefaultKeyPress(msg tea.KeyMsg, name keys.KeyName) (tea.Model, tea.Cmd) {
	tw := m.contentPane.TabbedWindow()

	switch name {
	case keys.KeyHelp:
		return m.showHelpScreen(helpTypeGeneral{}, nil)

	// Sidebar navigation
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
		// Context-aware: if on Tasks section, create a task instead
		if m.sidebar.GetSelection().Kind == ui.SectionTasks {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			m.contentPane.TaskPane().EnterCreateMode(cwd)
			m.contentPane.SetMode(ui.ContentModeTasks)
			return m, m.selectionChanged()
		}
		return m.startNewInstance(false)

	case keys.KeyTask:
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		m.contentPane.TaskPane().EnterCreateMode(cwd)
		m.navigateToSection(ui.SectionTasks)
		m.contentPane.SetMode(ui.ContentModeTasks)
		return m, m.selectionChanged()

	case keys.KeyTaskList:
		m.navigateToSection(ui.SectionTasks)
		return m, m.selectionChanged()

	case keys.KeyTriggerTask:
		if m.sidebar.GetSelection().Kind != ui.SectionTasks {
			return m, nil
		}
		sp := m.contentPane.TaskPane()
		if len(sp.GetTasks()) == 0 {
			return m, m.handleError(fmt.Errorf("no tasks to trigger"))
		}
		m.contentPane.SetMode(ui.ContentModeTasks)
		sp.SetFocus(true)
		sp.SetPendingTrigger()
		return m, m.handleTaskTrigger()

	case keys.KeySearch:
		return m.showSearchOverlay()

	// Hooks configuration
	case keys.KeyHooks:
		m.navigateToSection(ui.SectionHooks)
		return m, m.selectionChanged()

	// PR actions
	case keys.KeyOpenPR:
		return m.handleOpenPR()
	case keys.KeyCopyPR:
		return m.handleCopyPR()

	// Scrolling
	case keys.KeyShiftUp:
		m.contentPane.ScrollUp()
		return m, m.selectionChanged()
	case keys.KeyShiftDown:
		m.contentPane.ScrollDown()
		return m, m.selectionChanged()

	// Tab cycling (instance mode only)
	case keys.KeyTab:
		if m.contentPane.GetMode() == ui.ContentModeInstance {
			tw.Toggle()
			m.menu.SetActiveTab(tw.GetActiveTab())
			return m, m.selectionChanged()
		}
		return m, nil
	case keys.KeyShiftTab:
		if m.contentPane.GetMode() == ui.ContentModeInstance {
			tw.ToggleBack()
			m.menu.SetActiveTab(tw.GetActiveTab())
			return m, m.selectionChanged()
		}
		return m, nil

	// Tab lifecycle (instance mode only)
	case keys.KeyNewTab:
		return m.handleNewTab()
	case keys.KeyCloseTab:
		return m.handleCloseTab()

	// Instance actions
	case keys.KeyKill:
		return m.handleKill()
	case keys.KeyEnter:
		return m.handleEnter()

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
		for _, inst := range m.sidebar.GetInstances() {
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
	return func() tea.Msg {
		// The shell tab's tmux session is owned by the instance and torn down by
		// LocalBackend.Kill (looping all tabs) inside the daemon teardown — there
		// is no longer a UI-side terminal cache to clean up (#930 PR 2).
		if err := killSessionThroughDaemon(title, repoID); err != nil {
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
		for _, inst := range m.sidebar.GetInstances() {
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
	for _, inst := range m.sidebar.GetInstances() {
		if inst.Title == msg.title {
			repoName, repoErr = inst.RepoName()
			break
		}
	}
	if repoErr == nil {
		m.sidebar.RemoveInstanceByTitleWithRepo(msg.title, repoName)
	} else {
		m.sidebar.RemoveInstanceByTitle(msg.title)
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

// handleEnter handles the enter/open key action.
func (m *home) handleEnter() (tea.Model, tea.Cmd) {
	sel := m.sidebar.GetSelection()
	tw := m.contentPane.TabbedWindow()

	// Toggle expandable section headers (only Instances has children)
	if sel.IsHeader && sel.Kind == ui.SectionInstances {
		m.sidebar.ToggleSection()
		return m, m.selectionChanged()
	}
	// Instance selected
	if sel.Kind == ui.SectionInstances {
		selected := m.sidebar.GetSelectedInstance()
		if selected == nil || selected.GetStatus() == session.Loading {
			return m, nil
		}
		if selected.GetStatus() == session.Deleting {
			return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
		}
		if !selected.TmuxAlive() {
			// The backing session has vanished, so attaching is impossible.
			// Surface an actionable error instead of swallowing Enter, mirroring
			// the Deleting path above — a silent return left the user unsure
			// whether the keypress registered while the sidebar still showed a
			// green Ready dot (#935).
			return m, m.handleError(fmt.Errorf("session '%s' is no longer running", selected.Title))
		}
		// Capture the instance at Enter-press time — the synchronous moment the
		// selection is provably current. For first-time attachers the attach is
		// deferred until the help overlay is dismissed, and a background refresh
		// can drift the selection onto a different instance in the meantime; the
		// callbacks must attach to this captured instance, not re-read the live
		// selection (#716).
		if tw.IsInTerminalTab() {
			// The terminal tab attaches a local tmux session for local
			// instances, but a remote instance's terminal_cmd PTY for remote
			// ones (#843) — and that remote PTY hands the terminal back via
			// session.hookAttachTerminalRestore (main screen, modes off), the
			// same neutral state the sidebar remote attach leaves. So the
			// post-detach handling must key off the instance's real
			// remote-ness, exactly like the sidebar path below: a remote
			// terminal_cmd detach needs the #845/#848 full reset + reassert,
			// or the TUI keeps rendering on the main screen (#889).
			// Capture the active tab index at Enter-press time alongside the
			// instance (#716): a background refresh could otherwise drift the
			// selection — or the user could change tabs while the help overlay is
			// open — before the deferred attach callback runs.
			activeTab := tw.GetActiveTab()
			return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
				return attachOverlayCallbackFn(m, "handleEnter-terminal", "", selected.IsRemote(), func() (chan struct{}, error) {
					return tw.AttachTerminalForInstance(selected, activeTab)
				})
			})
		}
		return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
			return attachOverlayCallbackFn(m, "handleEnter-sidebar", "", selected.IsRemote(), func() (chan struct{}, error) {
				return m.sidebar.AttachInstance(selected)
			})
		})
	}
	return m, nil
}

// handleNewTab spawns a new shell tab in the selected instance and selects it
// (#930 PR 4). Single keypress, no prompt: the tab runs $SHELL in the instance's
// worktree. Remote instances have no local worktree, so new-tab is unsupported
// there and surfaces the same "not available for remote" guidance as the remote
// terminal tab. The soft cap (max 9 tabs) is enforced by Instance.AddShellTab,
// whose error is surfaced verbatim. The grown tab list is persisted so the new
// tab survives a restart (Sachin's #930 requirement).
func (m *home) handleNewTab() (tea.Model, tea.Cmd) {
	if m.contentPane.GetMode() != ui.ContentModeInstance {
		return m, nil
	}
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if status := selected.GetStatus(); status == session.Loading || status == session.Deleting {
		return m, nil
	}
	if selected.IsRemote() {
		return m, m.handleError(fmt.Errorf("new tabs are not available for remote sessions"))
	}
	if _, err := selected.AddShellTab(); err != nil {
		return m, m.handleError(err)
	}
	tw := m.contentPane.TabbedWindow()
	tw.SelectLastTab()
	m.menu.SetActiveTab(tw.GetActiveTab())
	if err := m.storage.SaveInstances(m.sidebar.GetInstances()); err != nil {
		log.ErrorLog.Printf("failed to persist new tab: %v", err)
	}
	return m, m.selectionChanged()
}

// handleCloseTab closes the active tab of the selected instance and selects the
// previous (left) tab (#930 PR 4). The agent tab (index 0) is unclosable — w on
// it is a gentle no-op message pointing at D for killing the whole session.
// Remote instances carry only the synthetic agent/terminal slots, neither of
// which is a real closable Tab. The shrunk tab list is persisted.
func (m *home) handleCloseTab() (tea.Model, tea.Cmd) {
	if m.contentPane.GetMode() != ui.ContentModeInstance {
		return m, nil
	}
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	tw := m.contentPane.TabbedWindow()
	idx := tw.GetActiveTab()
	if idx <= 0 {
		return m, m.handleError(fmt.Errorf("the agent tab can't be closed; use D to kill the session"))
	}
	if selected.IsRemote() {
		return m, m.handleError(fmt.Errorf("remote session tabs can't be closed"))
	}
	if err := selected.CloseTab(idx); err != nil {
		return m, m.handleError(err)
	}
	// Prefer the left/previous neighbor; SelectTab clamps so this is always in
	// range (idx >= 1, so idx-1 >= 0).
	tw.SelectTab(idx - 1)
	m.menu.SetActiveTab(tw.GetActiveTab())
	if err := m.storage.SaveInstances(m.sidebar.GetInstances()); err != nil {
		log.ErrorLog.Printf("failed to persist tab close: %v", err)
	}
	return m, m.selectionChanged()
}

// handleTabJump jumps the tabbed window to a 1-based tab number (the 1-9 number
// keys). Out-of-range numbers are a no-op (#930 PR 4).
func (m *home) handleTabJump(oneBased int) (tea.Model, tea.Cmd) {
	tw := m.contentPane.TabbedWindow()
	if !tw.JumpToTab(oneBased - 1) {
		return m, nil
	}
	m.menu.SetActiveTab(tw.GetActiveTab())
	return m, m.selectionChanged()
}

// handleOpenPR opens the PR URL in the browser.
func (m *home) handleOpenPR() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.GetPRInfo() == nil {
		return m, nil
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
	if selected == nil || selected.GetPRInfo() == nil {
		return m, nil
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
