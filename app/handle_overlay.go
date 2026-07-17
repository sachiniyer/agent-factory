package app

import (
	"fmt"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// handleStateSelectProgram handles key events during program selection.
func (m *home) handleStateSelectProgram(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.selectionOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.selectionOverlay.IsSubmitted() {
			idx := m.selectionOverlay.GetSelectedIndex()
			m.pendingProgram = tmux.SupportedPrograms[idx]
		}
		m.selectionOverlay = nil
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
		return m, nil
	}
	return m, nil
}

// handleStateConfirm handles key events during confirmation dialogs.
func (m *home) handleStateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.confirmationOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.state == stateConfirm {
			m.state = stateDefault
		}
		m.confirmationOverlay = nil
		// Forward any non-error tea.Msg returned by the confirmation
		// action (e.g. startKillMsg) back into the Bubble Tea event loop
		// so its handler can run.
		if pending := m.pendingConfirmMsg; pending != nil {
			m.pendingConfirmMsg = nil
			return m, func() tea.Msg { return pending }
		}
		return m, nil
	}
	return m, nil
}

// handleStateSearch handles key events during session search.
func (m *home) handleStateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.searchOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.searchOverlay.IsSubmitted() {
			if inst := m.searchOverlay.GetSelectedInstance(); inst != nil {
				m.sidebar.SelectInstance(inst)
			}
		}
		m.searchOverlay = nil
		m.state = stateDefault
		return m, tea.Sequence(tea.WindowSize(), m.selectionChanged())
	}
	return m, nil
}

// showSearchOverlay displays the session search overlay.
func (m *home) showSearchOverlay() (tea.Model, tea.Cmd) {
	// The overlay outlives this call and holds onto the slice, while a
	// background snapshotFetchedMsg reconcile can remove instances from the
	// projection in place (append-shift on the shared backing array). Hand the
	// overlay a stable copy so a later removal can't corrupt its list with
	// duplicate/ghost entries (#1008).
	instances := m.store.GetInstancesSnapshot()
	if len(instances) == 0 {
		return m, m.handleError(fmt.Errorf("no sessions to search"))
	}
	m.searchOverlay = overlay.NewSearchOverlay(instances)
	m.searchOverlay.SetWidth(60)
	m.layoutSearchOverlay()
	m.state = stateSearch
	return m, nil
}

// handleAutomationsFocus routes key events for the focused IN-RAIL automations
// section — which is only the compact summary since the #1096 play-test; the
// full manager lives in the tasks overlay. Cursor keys move the section's
// selection, Enter opens the manager overlay on it, Esc returns focus to the
// tree. Advertised global bindings (Tab/Shift-Tab focus ring, task manager,
// hooks, ? help, q quit) keep working, while pane-management verbs are
// consumed here so hidden `s`/`S`/`x`/pane-switch keys cannot mutate workspace
// panes from Automations focus (#1417).
func (m *home) handleAutomationsFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.ring.Active() != layout.RegionAutomations {
		return m, nil, false
	}
	if _, consumed := m.automations.HandleKey(msg); consumed {
		return m, nil, true
	}
	switch msg.String() {
	case "enter":
		mod, cmd := m.showTasksOverlay()
		return mod, cmd, true
	case "esc":
		m.focusRegion(layout.RegionTree)
		return m, nil, true
	}
	if automationsConsumesPaneVerb(msg) {
		return m, nil, true
	}
	return m, nil, false
}

// handleProjectsFocus routes key events for the focused bottom Projects section
// (#1588 follow-up) — a peer of the automations section that the Tab focus ring
// cycles into. It is a captive vim-style list (#1620): cursor keys (j/k/up/down)
// move the section's selection, Enter switches the rail to the cursor's project
// (reusing the #1547 switchProject path via switchToProjectRoot), and Esc returns
// focus to the tree. Search is entered ONLY on `/` (keys.KeySearch); every OTHER
// key is a no-op here (consumed, never fired) so nothing can START a search or
// filter from the section — notably the ctrl+p project-picker filter and the
// session-create/tasks/collapse verbs that used to leak through when this handler
// fell through to the global key map. Only the focus-ring and hard-exit chrome
// (Tab/Shift-Tab, ? help, q quit, ctrl+c) is allowed to fall through, so the user
// is never trapped in the section. Scoped to Projects focus; the Instances tree
// and Automations section are untouched.
func (m *home) handleProjectsFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.ring.Active() != layout.RegionProjects {
		return m, nil, false
	}
	// The section owns its vim/cursor nav (j/k/up/down).
	if _, consumed := m.projects.HandleKey(msg); consumed {
		return m, nil, true
	}
	switch msg.String() {
	case "enter":
		if proj, ok := m.projects.SelectedProject(); ok {
			mod, cmd := m.switchToProjectRoot(proj.Root)
			return mod, cmd, true
		}
		return m, nil, true
	case "esc":
		m.focusRegion(layout.RegionTree)
		return m, nil, true
	}
	// Delete the cursor's project (#1735): archive-then-remove, reversible. Routed
	// explicitly like `/` so the captive-section no-op below does not swallow it.
	if key.Matches(msg, keys.GlobalKeyBindings[keys.KeyDeleteProject]) {
		if proj, ok := m.projects.SelectedProject(); ok {
			mod, cmd := m.handleDeleteProject(proj)
			return mod, cmd, true
		}
		return m, nil, true
	}
	// `/` is the ONLY key that enters search from the Projects section (#1620),
	// vim-style. Route it explicitly rather than letting it fall through, so the
	// blanket no-op below can suppress every other key without also swallowing
	// the one search affordance.
	if key.Matches(msg, keys.GlobalKeyBindings[keys.KeySearch]) {
		mod, cmd := m.showSearchOverlay()
		return mod, cmd, true
	}
	// The focus ring and hard-exit chrome must keep working so the user can
	// always leave the section; let only those fall through to the root handler.
	if projectsChromeFallthroughKey(msg) {
		return m, nil, false
	}
	// Everything else is inert while Projects holds focus (#1620): no
	// session-create, no tasks overlay, no collapse/expand, and no ctrl+p
	// project-filter — so no key other than `/` can begin a search/filter from
	// the section.
	return m, nil, true
}

// projectsChromeFallthroughKey reports whether a key pressed with the Projects
// section focused must still reach the global handler — the focus ring
// (Tab/Shift-Tab), help (?), quit (q), and the always-on ctrl+c hard exit. Every
// other key is consumed as a no-op by handleProjectsFocus (#1620), so these are
// the only escape hatches out of the captive section.
func projectsChromeFallthroughKey(msg tea.KeyMsg) bool {
	if msg.String() == "ctrl+c" {
		return true
	}
	for _, name := range []keys.KeyName{
		keys.KeyTab,
		keys.KeyShiftTab,
		keys.KeyHelp,
		keys.KeyQuit,
	} {
		if key.Matches(msg, keys.GlobalKeyBindings[name]) {
			return true
		}
	}
	return false
}

func automationsConsumesPaneVerb(msg tea.KeyMsg) bool {
	for _, name := range []keys.KeyName{
		keys.KeyOpenPane,
		keys.KeySplitPane,
		keys.KeyHidePane,
		keys.KeyPanePrev,
		keys.KeyPaneNext,
	} {
		if key.Matches(msg, keys.GlobalKeyBindings[name]) {
			return true
		}
	}
	return false
}

// showTasksOverlay opens the task manager (list + create/edit form) as a
// centered modal overlay, preselecting the in-rail cursor's task and dropping
// straight into that task's editable config in a single action (#1249) — no
// second keypress to enter edit mode. The edit form still handles the selected
// task's list verbs (`r` run now, `x` toggle, `D` delete) and advertises Esc
// as the way back to the list, so the one-step edit flow does not hide the
// run-now path (#1288). When there are no tasks the overlay stays in list mode
// so `n` can create the first one.
func (m *home) showTasksOverlay() (tea.Model, tea.Cmd) {
	sp := m.automations.TaskPane()
	if idx := m.automations.SelectedTaskIndex(); idx >= 0 {
		sp.SelectTask(idx)
	}
	sp.SetFocus(true)
	sp.EnterEditSelected()
	m.layoutPaneOverlays()
	m.state = stateTasks
	return m, nil
}

// handleStateTasks routes key events to the task manager overlay and
// processes its pending actions. Esc in list mode drops the manager's own
// focus: the overlay closes and any dirty edits are saved — a failed save
// reloads both views to match disk and is surfaced inline so the dropped
// edit isn't silent. The configured quit key is root-routed before the task
// form can type it into an input, and ctrl+c still quits from normal mode.
func (m *home) handleStateTasks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sp := m.automations.TaskPane()

	// ctrl+c is unconditional: the form consumes it as "cancel", so routing it
	// here is the only thing that keeps a hard exit available from inside a
	// focused field (#1727).
	if msg.String() == "ctrl+c" {
		return m.handleQuit()
	}
	// The CONFIGURED quit key is gated on focus (#1961). It used to be routed
	// unconditionally alongside ctrl+c, which made "q" untypeable in a task name
	// or prompt — and a quit key is an ordinary character to a text field. The
	// user is not wedged: ctrl+c above still quits, and esc closes the form.
	if !sp.IsEditing() && !sp.IsCreating() && key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit]) {
		return m.handleQuit()
	}

	consumed := sp.HandleKeyPress(msg)

	if !sp.HasFocus() {
		m.state = stateDefault
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, nil
	}

	if sp.HasPendingCreate() {
		// Submitting the create form sets pendingCreate without releasing
		// focus, so the save-on-close branch above doesn't run.
		// handleTaskCreate writes the new task to disk and then reloads
		// every task via SetTasks, which clears the dirty flag and any
		// unsaved toggle/edit/delete. Flush those changes first so the
		// reload picks them up (#578). If that flush fails, surface it and
		// skip the create: the pending toggle/edit didn't persist, so we
		// don't want handleTaskCreate's reload to silently discard it (#934).
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, m.handleTaskCreate()
	}
	if sp.HasPendingTrigger() {
		// The trigger closes the overlay (handleTaskTrigger lands focus on the
		// spawned instance); flush dirty toggles/edits first so they aren't
		// stranded in a closed overlay until quit.
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, m.handleTaskTrigger()
	}

	if !consumed {
		if msg.String() == "ctrl+c" || key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit]) {
			return m.handleQuit()
		}
	}
	return m, nil
}

// showHooksOverlay opens the post-worktree hooks editor as a modal overlay
// (#1024 PR 4: hooks lost their persistent sidebar slot). The editor opens
// with input focus so its key loop (n add, enter edit, D delete, esc close)
// is live immediately.
func (m *home) showHooksOverlay() (tea.Model, tea.Cmd) {
	m.hooksPane.SetFocus(true)
	m.layoutPaneOverlays()
	m.state = stateHooks
	return m, nil
}

// showConfigEditor opens the global config editor overlay, reading config.toml
// fresh so the form shows the file as it is NOW — including a hand-edit or an
// `af config set` made since the TUI started. The pane must never render the
// TUI's own startup snapshot: config.toml is hand-editable by design, and an
// editor showing a stale copy would silently overwrite the newer file.
//
// A config that will not load is surfaced rather than swallowed: opening an
// editor onto a broken file and letting the user "fix" one key would write the
// rest of the broken state back.
func (m *home) showConfigEditor() (tea.Model, tea.Cmd) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return m, m.handleError(fmt.Errorf("cannot open the config editor: %w", err))
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return m, m.handleError(err)
	}
	m.configPane.SetEntries(config.ManifestWithValues(cfg), filepath.Join(configDir, config.TomlConfigFileName))
	m.configPane.SetFocus(true)
	m.layoutPaneOverlays()
	m.state = stateConfigEditor
	return m, nil
}

// handleStateConfigEditor routes key events to the config editor overlay. Esc
// closes it (the pane drops its own focus); each edit is written when it is
// committed, so there is nothing to flush on close.
//
// The quit-key handling deliberately differs from the hooks and tasks overlays
// above, which root-route the configured quit key unconditionally. Doing that
// here would make it impossible to TYPE the letter "q" into a value — and config
// values are arbitrary user strings: a vscode binary at /home/quentin/bin/code,
// a branch prefix, a detach-key spec. So the quit key is honored only in normal
// mode, while the value field is not taking runes.
//
// ctrl+c still quits unconditionally, which is the part #1727 is about: a text
// field must never swallow the hard exit, however it is being used.
func (m *home) handleStateConfigEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m.handleQuit()
	}
	if !m.configPane.IsEditing() && key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit]) {
		return m.handleQuit()
	}

	m.configPane.HandleKeyPress(msg)
	if !m.configPane.HasFocus() {
		m.state = stateDefault
		return m, nil
	}
	return m, nil
}

// handleStateHooks routes key events to the hooks editor overlay. Esc closes
// the overlay (the pane drops its own focus) and persists any edits; the
// configured quit key is root-routed before the pane can type it into an edit
// field, and ctrl+c still quits from normal mode.
func (m *home) handleStateHooks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// ctrl+c is unconditional (#1727); the configured quit key is gated on a
	// focused field (#1961). See handleStateTasks for the full argument.
	if msg.String() == "ctrl+c" {
		return m.handleQuit()
	}
	if !m.hooksPane.IsEditing() && key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit]) {
		return m.handleQuit()
	}

	consumed := m.hooksPane.HandleKeyPress(msg)
	if !m.hooksPane.HasFocus() {
		// The pane released focus (Esc): close the overlay and save. A failed
		// save keeps the pane dirty (#1001) and surfaces the error.
		m.state = stateDefault
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, nil
	}
	if !consumed {
		if msg.String() == "ctrl+c" || key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit]) {
			return m.handleQuit()
		}
	}
	return m, nil
}
