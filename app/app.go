package app

import (
	"claude-squad/config"
	"claude-squad/keys"
	"claude-squad/log"
	"claude-squad/microclaw"
	"claude-squad/schedule"
	"claude-squad/session"
	"claude-squad/session/git"
	"claude-squad/task"
	"claude-squad/ui"
	"claude-squad/ui/overlay"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const GlobalInstanceLimit = 10

// Run is the main entrypoint into the application.
func Run(ctx context.Context, program string, autoYes bool, repoID string) error {
	p := tea.NewProgram(
		newHome(ctx, program, autoYes, repoID),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(), // Mouse scroll
	)
	_, err := p.Run()
	return err
}

type state int

const (
	stateDefault state = iota
	// stateNew is the state when the user is creating a new instance.
	stateNew
	// statePrompt is the state when the user is entering a prompt.
	statePrompt
	// stateHelp is the state when a help screen is displayed.
	stateHelp
	// stateConfirm is the state when a confirmation modal is displayed.
	stateConfirm
	// stateSelectWorktree is the state when the user is selecting an existing worktree.
	stateSelectWorktree
	// stateSearch is the state when the user is searching sessions.
	stateSearch
	// stateHooks is the state when the user is editing worktree hooks.
	stateHooks
)

type home struct {
	ctx context.Context

	// -- Storage and Configuration --

	program string
	autoYes bool
	repoID  string

	// storage is the interface for saving/loading data to/from the app's state
	storage *session.Storage
	// appConfig stores persistent application configuration
	appConfig *config.Config
	// appState stores persistent application state like seen help screens
	appState config.AppState

	// -- State --

	// state is the current discrete state of the application
	state state
	// newInstanceFinalizer is called when the state is stateNew and then you press enter.
	// It registers the new instance in the list after the instance has been started.
	newInstanceFinalizer func()

	// promptAfterName tracks if we should enter prompt mode after naming
	promptAfterName bool

	// keySent is used to manage underlining menu items
	keySent bool

	// -- UI Components --

	// sidebar is the unified left navigation pane with collapsible sections
	sidebar *ui.Sidebar
	// contentPane wraps the tabbed window and other contextual panes
	contentPane *ui.ContentPane
	// menu displays the bottom menu
	menu *ui.Menu
	// errBox displays error messages
	errBox *ui.ErrBox
	// global spinner instance. we plumb this down to where it's needed
	spinner spinner.Model
	// textInputOverlay handles text input with state
	textInputOverlay *overlay.TextInputOverlay
	// textOverlay displays text information
	textOverlay *overlay.TextOverlay
	// confirmationOverlay displays confirmation modals
	confirmationOverlay *overlay.ConfirmationOverlay
	// selectionOverlay handles worktree selection
	selectionOverlay *overlay.SelectionOverlay
	// searchOverlay handles session search
	searchOverlay *overlay.SearchOverlay
	// hooksOverlay handles worktree hooks editing
	hooksOverlay *overlay.HooksOverlay
	// selectedWorktree stores the worktree info selected by the user for attach
	selectedWorktree *git.WorktreeInfo
	// availableWorktrees stores the worktrees shown in the selection overlay
	availableWorktrees []git.WorktreeInfo

	// microclawBridge is the bridge to the microclaw instance
	microclawBridge *microclaw.Bridge
	// microclawChats caches the microclaw chats for the message overlay
	microclawChats []microclaw.Chat
}

func newHome(ctx context.Context, program string, autoYes bool, repoID string) *home {
	// Load application config
	appConfig := config.LoadConfig()

	// Load application state
	appState := config.LoadState()

	// Initialize storage (repo-scoped)
	storage, err := session.NewStorage(appState, repoID)
	if err != nil {
		fmt.Printf("Failed to initialize storage: %v\n", err)
		os.Exit(1)
	}

	// Initialize microclaw bridge
	mcDir := os.Getenv("MICROCLAW_DIR")
	mcBridge := microclaw.NewBridge(mcDir)
	var mcPane *ui.MicroClawPane
	if mcBridge.Available() {
		mcPane = ui.NewMicroClawPane(mcBridge)
	}

	tabbedWindow := ui.NewTabbedWindow(ui.NewPreviewPane(), ui.NewDiffPane(), ui.NewTerminalPane())

	h := &home{
		ctx:             ctx,
		spinner:         spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		menu:            ui.NewMenu(),
		contentPane:     ui.NewContentPane(tabbedWindow, mcPane),
		errBox:          ui.NewErrBox(),
		storage:         storage,
		appConfig:       appConfig,
		program:         program,
		autoYes:         autoYes,
		repoID:          repoID,
		state:           stateDefault,
		appState:        appState,
		microclawBridge: mcBridge,
	}
	h.sidebar = ui.NewSidebar(&h.spinner, autoYes)

	// Set microclaw availability on sidebar
	h.sidebar.SetMicroClawAvailable(mcBridge.Available())

	// Load saved instances (scoped to current repo)
	instances, err := storage.LoadInstances()
	if err != nil {
		fmt.Printf("Failed to load instances: %v\n", err)
		os.Exit(1)
	}

	// Merge pending instances from scheduled runs.
	pendingData, err := schedule.LoadAndClearPendingInstances()
	if err != nil {
		log.WarningLog.Printf("Failed to load pending instances: %v", err)
	}
	var otherRepoPending []session.InstanceData
	var mergedCount int
	for _, data := range pendingData {
		rid := config.RepoIDFromRoot(data.Worktree.RepoPath)
		if rid == repoID {
			pendingInstance, err := session.FromInstanceData(data)
			if err != nil {
				log.WarningLog.Printf("Failed to restore pending instance %s: %v", data.Title, err)
				continue
			}
			instances = append(instances, pendingInstance)
			mergedCount++
		} else {
			otherRepoPending = append(otherRepoPending, data)
		}
	}

	// Save other-repo pending instances directly to their per-repo files
	if len(otherRepoPending) > 0 {
		grouped := make(map[string][]session.InstanceData)
		for _, d := range otherRepoPending {
			rid := config.RepoIDFromRoot(d.Worktree.RepoPath)
			grouped[rid] = append(grouped[rid], d)
		}
		for rid, group := range grouped {
			existing, err := config.LoadRepoInstances(rid)
			if err != nil {
				log.WarningLog.Printf("Failed to load existing instances for repo %s: %v", rid, err)
			}
			var existingData []session.InstanceData
			if existing != nil && string(existing) != "[]" && string(existing) != "null" {
				if err := json.Unmarshal(existing, &existingData); err != nil {
					log.WarningLog.Printf("Failed to parse existing instances for repo %s: %v", rid, err)
				}
			}
			existingData = append(existingData, group...)
			jsonData, err := json.Marshal(existingData)
			if err != nil {
				log.WarningLog.Printf("Failed to marshal instances for repo %s: %v", rid, err)
				continue
			}
			if err := config.SaveRepoInstances(rid, jsonData); err != nil {
				log.WarningLog.Printf("Failed to save instances for repo %s: %v", rid, err)
			}
		}
	}

	// Add loaded instances to the sidebar
	for _, instance := range instances {
		h.sidebar.AddInstance(instance)()
		if autoYes {
			instance.AutoYes = true
		}
	}

	// Save instances so pending ones are persisted to the per-repo file
	if mergedCount > 0 {
		if err := storage.SaveInstances(h.sidebar.GetInstances()); err != nil {
			log.WarningLog.Printf("Failed to save merged instances: %v", err)
		}
	}

	// Load schedules for sidebar display
	schedules, err := schedule.LoadSchedulesForCurrentRepo()
	if err != nil {
		log.WarningLog.Printf("Failed to load schedules: %v", err)
	} else {
		h.sidebar.SetSchedules(schedules)
	}

	// Load task count for sidebar display
	tasks, err := task.LoadTasks()
	if err != nil {
		log.WarningLog.Printf("Failed to load tasks: %v", err)
	} else {
		h.sidebar.SetTaskCount(len(tasks))
		h.contentPane.TaskPane().SetTasks(tasks)
	}

	// Load schedules into schedule pane
	if len(schedules) > 0 {
		h.contentPane.SchedulePane().SetSchedules(schedules)
	}

	return h
}

// updateHandleWindowSizeEvent sets the sizes of the components.
func (m *home) updateHandleWindowSizeEvent(msg tea.WindowSizeMsg) {
	// Sidebar takes 30% of width, content takes 70%
	sidebarWidth := int(float32(msg.Width) * 0.3)
	contentWidth := msg.Width - sidebarWidth

	// Menu takes 10% of height, sidebar and content take 90%
	contentHeight := int(float32(msg.Height) * 0.9)
	menuHeight := msg.Height - contentHeight - 1
	m.errBox.SetSize(int(float32(msg.Width)*0.9), 1)

	m.contentPane.SetSize(contentWidth, contentHeight)
	m.sidebar.SetSize(sidebarWidth, contentHeight)

	if m.textInputOverlay != nil {
		m.textInputOverlay.SetSize(int(float32(msg.Width)*0.6), int(float32(msg.Height)*0.4))
	}
	if m.textOverlay != nil {
		m.textOverlay.SetWidth(int(float32(msg.Width) * 0.6))
	}
	if m.selectionOverlay != nil {
		m.selectionOverlay.SetWidth(int(float32(msg.Width) * 0.6))
	}

	tw := m.contentPane.TabbedWindow()
	previewWidth, previewHeight := tw.GetPreviewSize()
	if err := m.sidebar.SetSessionPreviewSize(previewWidth, previewHeight); err != nil {
		log.ErrorLog.Print(err)
	}
	m.menu.SetSize(msg.Width, menuHeight)
}

func (m *home) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			time.Sleep(100 * time.Millisecond)
			return previewTickMsg{}
		},
		tickUpdateMetadataCmd,
		tickUpdatePRInfoCmd,
	)
}

func (m *home) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case hideErrMsg:
		m.errBox.Clear()
	case previewTickMsg:
		m.contentPane.UpdateMicroClaw()
		cmd := m.selectionChanged()
		return m, tea.Batch(
			cmd,
			func() tea.Msg {
				time.Sleep(100 * time.Millisecond)
				return previewTickMsg{}
			},
		)
	case keyupMsg:
		m.menu.ClearKeydown()
		return m, nil
	case tickUpdatePRInfoMessage:
		for _, instance := range m.sidebar.GetInstances() {
			if !instance.Started() {
				continue
			}
			if err := instance.UpdatePRInfo(); err != nil {
				log.WarningLog.Printf("could not update PR info: %v", err)
			}
		}
		return m, tickUpdatePRInfoCmd
	case tickUpdateMetadataMessage:
		for _, instance := range m.sidebar.GetInstances() {
			if !instance.Started() {
				continue
			}
			instance.CheckAndHandleTrustPrompt()
			updated, prompt := instance.HasUpdated()
			if updated {
				instance.SetStatus(session.Running)
			} else {
				if prompt {
					instance.TapEnter()
				} else {
					instance.SetStatus(session.Ready)
				}
			}
			if err := instance.UpdateDiffStats(); err != nil {
				log.WarningLog.Printf("could not update diff stats: %v", err)
			}
		}
		return m, tickUpdateMetadataCmd
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress {
			if msg.Button == tea.MouseButtonWheelDown || msg.Button == tea.MouseButtonWheelUp {
				selected := m.sidebar.GetSelectedInstance()
				if selected == nil {
					return m, nil
				}
				switch msg.Button {
				case tea.MouseButtonWheelUp:
					m.contentPane.ScrollUp()
				case tea.MouseButtonWheelDown:
					m.contentPane.ScrollDown()
				}
			}
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKeyPress(msg)
	case tea.WindowSizeMsg:
		m.updateHandleWindowSizeEvent(msg)
		return m, nil
	case error:
		return m, m.handleError(msg)
	case instanceChangedMsg:
		return m, m.selectionChanged()
	case instanceStartedMsg:
		m.sidebar.SelectInstance(msg.instance)

		if msg.err != nil {
			m.sidebar.Kill()
			return m, tea.Batch(m.handleError(msg.err), m.selectionChanged())
		}

		if err := m.storage.SaveInstances(m.sidebar.GetInstances()); err != nil {
			return m, m.handleError(err)
		}
		if m.autoYes {
			msg.instance.AutoYes = true
		}

		if msg.promptAfterName {
			m.state = statePrompt
			m.menu.SetState(ui.StatePrompt)
			m.textInputOverlay = overlay.NewTextInputOverlay("Enter prompt", "")
		} else {
			m.menu.SetState(ui.StateDefault)
			m.showHelpScreen(helpStart(msg.instance), nil)
		}

		return m, tea.Batch(tea.WindowSize(), m.selectionChanged())
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *home) cleanupMicroClaw() {
	mc := m.contentPane.MicroClawPane()
	if mc != nil {
		mc.Close()
	}
}

func (m *home) handleQuit() (tea.Model, tea.Cmd) {
	// Save any dirty task/schedule state
	m.saveContentPaneState()

	if err := m.storage.SaveInstances(m.sidebar.GetInstances()); err != nil {
		return m, m.handleError(err)
	}
	tw := m.contentPane.TabbedWindow()
	tw.CleanupTerminal()
	m.cleanupMicroClaw()
	return m, tea.Quit
}

// saveContentPaneState persists any changes from the task/schedule panes.
func (m *home) saveContentPaneState() {
	tp := m.contentPane.TaskPane()
	if tp.IsDirty() {
		if err := task.SaveTasks(tp.GetTasks()); err != nil {
			log.ErrorLog.Printf("failed to save tasks: %v", err)
		}
		m.sidebar.SetTaskCount(len(tp.GetTasks()))
	}

	sp := m.contentPane.SchedulePane()
	if sp.IsDirty() {
		for _, sched := range sp.GetSchedules() {
			if err := schedule.UpdateSchedule(sched); err != nil {
				log.ErrorLog.Printf("failed to update schedule: %v", err)
			}
			if sched.Enabled {
				if err := schedule.InstallSystemdTimer(sched); err != nil {
					log.WarningLog.Printf("failed to install timer: %v", err)
				}
			} else {
				if err := schedule.RemoveSystemdTimer(sched); err != nil {
					log.WarningLog.Printf("failed to remove timer: %v", err)
				}
			}
		}
		for _, sched := range sp.GetDeleted() {
			if err := schedule.RemoveSchedule(sched.ID); err != nil {
				log.ErrorLog.Printf("failed to remove schedule: %v", err)
			}
			if err := schedule.RemoveSystemdTimer(sched); err != nil {
				log.WarningLog.Printf("failed to remove timer: %v", err)
			}
		}
		// Refresh sidebar
		schedules, err := schedule.LoadSchedulesForCurrentRepo()
		if err == nil {
			m.sidebar.SetSchedules(schedules)
		}
	}
}

// handleScheduleCreate processes a pending schedule creation from the inline form.
func (m *home) handleScheduleCreate() tea.Cmd {
	sp := m.contentPane.SchedulePane()
	prompt, cronExpr, projectPath := sp.ConsumePendingCreate()

	if err := schedule.ValidateCronExpr(cronExpr); err != nil {
		return m.handleError(fmt.Errorf("invalid cron: %v", err))
	}
	absPath, err := filepath.Abs(projectPath)
	if err != nil {
		return m.handleError(fmt.Errorf("invalid path: %v", err))
	}
	s := schedule.Schedule{
		ID:          schedule.GenerateID(),
		Prompt:      prompt,
		CronExpr:    cronExpr,
		ProjectPath: absPath,
		Program:     m.program,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
	if err := schedule.AddSchedule(s); err != nil {
		return m.handleError(fmt.Errorf("failed to save schedule: %v", err))
	}
	if err := schedule.InstallSystemdTimer(s); err != nil {
		log.WarningLog.Printf("failed to install systemd timer: %v", err)
	}
	// Refresh sidebar and schedule pane
	schedules, err := schedule.LoadSchedulesForCurrentRepo()
	if err == nil {
		m.sidebar.SetSchedules(schedules)
		sp.SetSchedules(schedules)
	}
	return nil
}

// handleScheduleTrigger immediately spawns an instance for the selected schedule.
func (m *home) handleScheduleTrigger() tea.Cmd {
	sp := m.contentPane.SchedulePane()
	sched := sp.ConsumePendingTrigger()
	if sched == nil {
		return m.handleError(fmt.Errorf("no schedule selected"))
	}

	if m.sidebar.NumInstances() >= GlobalInstanceLimit {
		return m.handleError(fmt.Errorf("you can't create more than %d instances", GlobalInstanceLimit))
	}

	title := fmt.Sprintf("sched-%s-%s", sched.ID, time.Now().Format("20060102-150405"))

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    sched.ProjectPath,
		Program: sched.Program,
	})
	if err != nil {
		return m.handleError(fmt.Errorf("failed to create instance: %w", err))
	}

	finalizer := m.sidebar.AddInstance(instance)
	m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
	instance.SetStatus(session.Loading)
	finalizer()
	m.menu.SetState(ui.StateDefault)

	prompt := sched.Prompt
	schedID := sched.ID
	startCmd := func() tea.Msg {
		if err := instance.Start(true); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		if err := schedule.WaitForReady(instance); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		if instance.CheckAndHandleTrustPrompt() {
			time.Sleep(1 * time.Second)
			if err := schedule.WaitForReady(instance); err != nil {
				return instanceStartedMsg{instance: instance, err: err}
			}
		}

		if err := instance.SendPromptCommand(prompt); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		// Update schedule last run status.
		if s, err := schedule.GetSchedule(schedID); err == nil {
			now := time.Now()
			s.LastRunAt = &now
			s.LastRunStatus = "triggered"
			if err := schedule.UpdateSchedule(*s); err != nil {
				log.ErrorLog.Printf("failed to update schedule status: %v", err)
			}
		}

		return instanceStartedMsg{instance: instance, err: nil}
	}

	return tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
}

func (m *home) handleMenuHighlighting(msg tea.KeyMsg) (cmd tea.Cmd, returnEarly bool) {
	if m.keySent {
		m.keySent = false
		return nil, false
	}
	if m.state == statePrompt || m.state == stateHelp || m.state == stateConfirm || m.state == stateSelectWorktree || m.state == stateHooks {
		return nil, false
	}
	// Don't highlight when content pane has focus
	if m.contentPane.HasFocus() {
		return nil, false
	}
	name, ok := keys.GlobalKeyStringsMap[msg.String()]
	if !ok {
		return nil, false
	}

	if name == keys.KeyShiftDown || name == keys.KeyShiftUp {
		return nil, false
	}
	// Skip sidebar nav keys from menu highlighting
	if name == keys.KeyLeft || name == keys.KeyRight || name == keys.KeyNextSection || name == keys.KeyPrevSection {
		return nil, false
	}

	if name == keys.KeyEnter && m.state == stateNew {
		name = keys.KeySubmitName
	}
	m.keySent = true
	return tea.Batch(
		func() tea.Msg { return msg },
		m.keydownCallback(name)), true
}

func (m *home) handleKeyPress(msg tea.KeyMsg) (mod tea.Model, cmd tea.Cmd) {
	cmd, returnEarly := m.handleMenuHighlighting(msg)
	if returnEarly {
		return m, cmd
	}

	if m.state == stateHelp {
		return m.handleHelpState(msg)
	}

	if m.state == stateNew {
		if msg.String() == "ctrl+c" {
			m.state = stateDefault
			m.promptAfterName = false
			m.selectedWorktree = nil
			m.availableWorktrees = nil
			m.sidebar.Kill()
			return m, tea.Sequence(
				tea.WindowSize(),
				func() tea.Msg {
					m.menu.SetState(ui.StateDefault)
					return nil
				},
			)
		}

		instance := m.sidebar.GetInstances()[m.sidebar.NumInstances()-1]
		switch msg.Type {
		case tea.KeyEnter:
			if len(instance.Title) == 0 {
				return m, m.handleError(fmt.Errorf("title cannot be empty"))
			}

			instance.SetStatus(session.Loading)
			m.newInstanceFinalizer()
			promptAfterName := m.promptAfterName
			m.promptAfterName = false
			m.state = stateDefault
			m.menu.SetState(ui.StateDefault)

			selectedWt := m.selectedWorktree
			m.selectedWorktree = nil
			m.availableWorktrees = nil
			startCmd := func() tea.Msg {
				var err error
				if selectedWt != nil {
					err = instance.StartWithExistingWorktree(selectedWt.Path, selectedWt.Branch)
				} else {
					err = instance.Start(true)
				}
				return instanceStartedMsg{
					instance:        instance,
					err:             err,
					promptAfterName: promptAfterName,
				}
			}

			return m, tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
		case tea.KeyRunes:
			if runewidth.StringWidth(instance.Title) >= 32 {
				return m, m.handleError(fmt.Errorf("title cannot be longer than 32 characters"))
			}
			if err := instance.SetTitle(instance.Title + string(msg.Runes)); err != nil {
				return m, m.handleError(err)
			}
		case tea.KeyBackspace:
			runes := []rune(instance.Title)
			if len(runes) == 0 {
				return m, nil
			}
			if err := instance.SetTitle(string(runes[:len(runes)-1])); err != nil {
				return m, m.handleError(err)
			}
		case tea.KeySpace:
			if err := instance.SetTitle(instance.Title + " "); err != nil {
				return m, m.handleError(err)
			}
		case tea.KeyEsc:
			m.sidebar.Kill()
			m.state = stateDefault
			m.selectedWorktree = nil
			m.availableWorktrees = nil
			cmd := m.selectionChanged()

			return m, tea.Batch(cmd, tea.Sequence(
				tea.WindowSize(),
				func() tea.Msg {
					m.menu.SetState(ui.StateDefault)
					return nil
				},
			))
		default:
		}
		return m, nil
	} else if m.state == statePrompt {
		shouldClose := m.textInputOverlay.HandleKeyPress(msg)
		if shouldClose {
			selected := m.sidebar.GetSelectedInstance()
			if selected == nil {
				return m, nil
			}
			if m.textInputOverlay.IsSubmitted() {
				if err := selected.SendPrompt(m.textInputOverlay.GetValue()); err != nil {
					return m, m.handleError(err)
				}
			}
			m.textInputOverlay = nil
			m.state = stateDefault
			return m, tea.Sequence(
				tea.WindowSize(),
				func() tea.Msg {
					m.menu.SetState(ui.StateDefault)
					m.showHelpScreen(helpStart(selected), nil)
					return nil
				},
			)
		}
		return m, nil
	}

	// Handle worktree selection state
	if m.state == stateSelectWorktree {
		shouldClose := m.selectionOverlay.HandleKeyPress(msg)
		if shouldClose {
			if m.selectionOverlay.IsSubmitted() {
				idx := m.selectionOverlay.GetSelectedIndex()
				wt := m.availableWorktrees[idx]
				m.selectedWorktree = &wt
				m.selectionOverlay = nil

				instance, err := session.NewInstance(session.InstanceOptions{
					Title:   "",
					Path:    ".",
					Program: m.program,
				})
				if err != nil {
					m.selectedWorktree = nil
					m.state = stateDefault
					m.menu.SetState(ui.StateDefault)
					return m, m.handleError(err)
				}

				m.newInstanceFinalizer = m.sidebar.AddInstance(instance)
				m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
				m.state = stateNew
				m.menu.SetState(ui.StateNewInstance)
				return m, nil
			}
			m.selectionOverlay = nil
			m.selectedWorktree = nil
			m.availableWorktrees = nil
			m.state = stateDefault
			m.menu.SetState(ui.StateDefault)
			return m, nil
		}
		return m, nil
	}

	// Handle confirmation state
	if m.state == stateConfirm {
		shouldClose := m.confirmationOverlay.HandleKeyPress(msg)
		if shouldClose {
			if m.state == stateConfirm {
				m.state = stateDefault
			}
			m.confirmationOverlay = nil
			return m, nil
		}
		return m, nil
	}

	// Handle search state
	if m.state == stateSearch {
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

	// Handle hooks editing state
	if m.state == stateHooks {
		shouldClose := m.hooksOverlay.HandleKeyPress(msg)
		if shouldClose {
			if m.hooksOverlay.IsDirty() {
				repoCfg, err := config.LoadRepoConfig(m.repoID)
				if err != nil {
					repoCfg = &config.RepoConfig{}
				}
				repoCfg.PostWorktreeCommands = m.hooksOverlay.GetCommands()
				if err := config.SaveRepoConfig(m.repoID, repoCfg); err != nil {
					log.ErrorLog.Printf("failed to save hooks: %v", err)
				}
			}
			m.hooksOverlay = nil
			m.state = stateDefault
			return m, nil
		}
		return m, nil
	}

	// Route keys to content pane if it has focus (e.g., editing todos/schedules)
	if m.contentPane.HasFocus() {
		consumed := m.contentPane.HandleKeyPress(msg)
		if consumed {
			// If focus was released (Esc), save state
			if !m.contentPane.HasFocus() {
				m.saveContentPaneState()
			}
			// Check if a new schedule was submitted via the inline form
			sp := m.contentPane.SchedulePane()
			if sp.HasPendingCreate() {
				return m, m.handleScheduleCreate()
			}
			if sp.HasPendingTrigger() {
				return m, m.handleScheduleTrigger()
			}
			return m, nil
		}
	}

	// Exit scrolling mode when ESC is pressed
	tw := m.contentPane.TabbedWindow()
	if msg.Type == tea.KeyEsc {
		if m.contentPane.GetMode() == ui.ContentModeInstance {
			if tw.IsInPreviewTab() && tw.IsPreviewInScrollMode() {
				selected := m.sidebar.GetSelectedInstance()
				err := tw.ResetPreviewToNormalMode(selected)
				if err != nil {
					return m, m.handleError(err)
				}
				return m, m.selectionChanged()
			}
			if tw.IsInTerminalTab() && tw.IsTerminalInScrollMode() {
				tw.ResetTerminalToNormalMode()
				return m, m.selectionChanged()
			}
		}
		if m.contentPane.GetMode() == ui.ContentModeMicroClaw {
			mc := m.contentPane.MicroClawPane()
			if mc != nil && mc.IsScrolling() {
				mc.ResetToNormalMode()
				return m, m.selectionChanged()
			}
		}
	}

	// Handle quit commands
	if msg.String() == "ctrl+c" || msg.String() == "q" {
		return m.handleQuit()
	}

	name, ok := keys.GlobalKeyStringsMap[msg.String()]
	if !ok {
		return m, nil
	}

	// Handle content pane Enter for focusing (todos/schedules)
	if name == keys.KeyEnter {
		mode := m.contentPane.GetMode()
		if mode == ui.ContentModeTodos || mode == ui.ContentModeSchedules {
			consumed := m.contentPane.HandleKeyPress(msg)
			if consumed {
				return m, nil
			}
		}
	}

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
	case keys.KeyPrompt:
		if m.sidebar.NumInstances() >= GlobalInstanceLimit {
			return m, m.handleError(
				fmt.Errorf("you can't create more than %d instances", GlobalInstanceLimit))
		}
		instance, err := session.NewInstance(session.InstanceOptions{
			Title:   "",
			Path:    ".",
			Program: m.program,
		})
		if err != nil {
			return m, m.handleError(err)
		}
		m.newInstanceFinalizer = m.sidebar.AddInstance(instance)
		m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
		m.promptAfterName = true
		return m, nil

	case keys.KeyNew:
		// Context-aware: if on Schedules section, create a schedule instead
		if m.sidebar.GetSelection().Kind == ui.SectionSchedules {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			m.contentPane.SchedulePane().EnterCreateMode(cwd)
			m.contentPane.SetMode(ui.ContentModeSchedules)
			return m, m.selectionChanged()
		}

		if m.sidebar.NumInstances() >= GlobalInstanceLimit {
			return m, m.handleError(
				fmt.Errorf("you can't create more than %d instances", GlobalInstanceLimit))
		}
		instance, err := session.NewInstance(session.InstanceOptions{
			Title:   "",
			Path:    ".",
			Program: m.program,
		})
		if err != nil {
			return m, m.handleError(err)
		}
		m.newInstanceFinalizer = m.sidebar.AddInstance(instance)
		m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
		return m, nil

	case keys.KeySchedule:
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		m.contentPane.SchedulePane().EnterCreateMode(cwd)
		m.navigateToSection(ui.SectionSchedules)
		m.contentPane.SetMode(ui.ContentModeSchedules)
		return m, m.selectionChanged()

	case keys.KeyScheduleList:
		// Navigate to schedules section in sidebar
		m.navigateToSection(ui.SectionSchedules)
		return m, m.selectionChanged()

	case keys.KeyTriggerSchedule:
		// Only trigger when viewing schedules section
		if m.sidebar.GetSelection().Kind != ui.SectionSchedules {
			return m, nil
		}
		sp := m.contentPane.SchedulePane()
		if len(sp.GetSchedules()) == 0 {
			return m, m.handleError(fmt.Errorf("no schedules to trigger"))
		}
		m.contentPane.SetMode(ui.ContentModeSchedules)
		sp.SetFocus(true)
		sp.SetPendingTrigger()
		return m, m.handleScheduleTrigger()

	case keys.KeyTasks:
		// Navigate to todos section in sidebar
		m.navigateToSection(ui.SectionTodos)
		return m, m.selectionChanged()

	case keys.KeyMicroClaw:
		if m.microclawBridge == nil || !m.microclawBridge.Available() {
			return m, m.handleError(fmt.Errorf("MicroClaw not available — set MICROCLAW_DIR or install microclaw"))
		}
		// Navigate to MicroClaw section in sidebar
		m.navigateToSection(ui.SectionMicroClaw)
		return m, m.selectionChanged()

	case keys.KeySearch:
		instances := m.sidebar.GetInstances()
		if len(instances) == 0 {
			return m, m.handleError(fmt.Errorf("no sessions to search"))
		}
		m.searchOverlay = overlay.NewSearchOverlay(instances)
		m.searchOverlay.SetWidth(60)
		m.state = stateSearch
		return m, nil

	case keys.KeyAttach:
		if m.sidebar.NumInstances() >= GlobalInstanceLimit {
			return m, m.handleError(
				fmt.Errorf("you can't create more than %d instances", GlobalInstanceLimit))
		}

		worktrees, err := git.ListWorktrees(".")
		if err != nil {
			return m, m.handleError(fmt.Errorf("failed to list worktrees: %v", err))
		}
		if len(worktrees) == 0 {
			return m, m.handleError(fmt.Errorf("no worktrees found"))
		}

		trackedPaths := make(map[string]bool)
		for _, inst := range m.sidebar.GetInstances() {
			if p := inst.GetWorktreePath(); p != "" {
				trackedPaths[p] = true
			}
		}

		items := make([]string, len(worktrees))
		for i, wt := range worktrees {
			label := wt.Path
			if wt.Branch != "" {
				label = fmt.Sprintf("%s (%s)", wt.Branch, wt.Path)
			}
			if wt.IsMainWorktree {
				label += " [root]"
			}
			if trackedPaths[wt.Path] {
				label += " [has session]"
			}
			items[i] = label
		}

		m.availableWorktrees = worktrees
		m.selectionOverlay = overlay.NewSelectionOverlay("Attach to existing worktree", items)
		m.selectionOverlay.SetWidth(60)
		m.state = stateSelectWorktree
		return m, nil

	// Hooks configuration
	case keys.KeyHooks:
		repoCfg, err := config.LoadRepoConfig(m.repoID)
		if err != nil {
			return m, m.handleError(fmt.Errorf("failed to load repo config: %w", err))
		}
		m.hooksOverlay = overlay.NewHooksOverlay(repoCfg.PostWorktreeCommands)
		m.hooksOverlay.SetWidth(60)
		m.state = stateHooks
		return m, nil

	// PR actions
	case keys.KeyOpenPR:
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
		return m, nil

	case keys.KeyCopyPR:
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

	// Instance actions
	case keys.KeyKill:
		selected := m.sidebar.GetSelectedInstance()
		if selected == nil || selected.Status == session.Loading {
			return m, nil
		}

		killAction := func() tea.Msg {
			tw.CleanupTerminalForInstance(selected.Title)
			m.sidebar.Kill()
			if err := m.storage.DeleteInstance(selected.Title); err != nil {
				log.ErrorLog.Printf("failed to delete instance from storage: %v", err)
			}
			return instanceChangedMsg{}
		}

		message := fmt.Sprintf("[!] Kill session '%s'?", selected.Title)
		return m, m.confirmAction(message, killAction)

	case keys.KeyEnter:
		sel := m.sidebar.GetSelection()
		// Toggle expandable section headers (only Instances has children)
		if sel.IsHeader && sel.Kind == ui.SectionInstances {
			m.sidebar.ToggleSection()
			return m, m.selectionChanged()
		}
		// Instance selected
		if sel.Kind == ui.SectionInstances {
			selected := m.sidebar.GetSelectedInstance()
			if selected == nil || selected.Status == session.Loading || !selected.TmuxAlive() {
				return m, nil
			}
			if tw.IsInTerminalTab() {
				m.showHelpScreen(helpTypeInstanceAttach{}, func() {
					ch, err := tw.AttachTerminal()
					if err != nil {
						log.ErrorLog.Printf("failed to attach terminal: %v", err)
						return
					}
					<-ch
					m.state = stateDefault
				})
				return m, nil
			}
			m.showHelpScreen(helpTypeInstanceAttach{}, func() {
				ch, err := m.sidebar.Attach()
				if err != nil {
					log.ErrorLog.Printf("failed to attach: %v", err)
					return
				}
				<-ch
				m.state = stateDefault
			})
			return m, nil
		}
		// MicroClaw selected — attach
		if sel.Kind == ui.SectionMicroClaw {
			if m.microclawBridge == nil || !m.microclawBridge.Available() {
				return m, m.handleError(fmt.Errorf("MicroClaw not available"))
			}
			mc := m.contentPane.MicroClawPane()
			if mc == nil {
				return m, nil
			}
			m.showHelpScreen(helpTypeInstanceAttach{}, func() {
				ch, err := mc.Attach()
				if err != nil {
					log.ErrorLog.Printf("failed to attach microclaw: %v", err)
					return
				}
				<-ch
				m.state = stateDefault
			})
			return m, nil
		}
		return m, nil

	default:
		return m, nil
	}
}

// navigateToSection moves the sidebar selection to the header of the given section.
func (m *home) navigateToSection(kind ui.SidebarSectionKind) {
	sel := m.sidebar.GetSelection()
	// Already on the right section? Do nothing extra.
	if sel.Kind == kind && sel.IsHeader {
		return
	}
	// Jump through sections until we land on the right header
	for i := 0; i < 10; i++ { // safety limit
		m.sidebar.JumpNextSection()
		sel = m.sidebar.GetSelection()
		if sel.Kind == kind && sel.IsHeader {
			return
		}
	}
	// If we didn't find it going forward, try backward
	for i := 0; i < 10; i++ {
		m.sidebar.JumpPrevSection()
		sel = m.sidebar.GetSelection()
		if sel.Kind == kind && sel.IsHeader {
			return
		}
	}
}

// selectionChanged updates the content pane and menu based on the sidebar selection.
func (m *home) selectionChanged() tea.Cmd {
	sel := m.sidebar.GetSelection()
	tw := m.contentPane.TabbedWindow()

	switch {
	case sel.Kind == ui.SectionInstances && !sel.IsHeader:
		m.contentPane.SetMode(ui.ContentModeInstance)
		selected := m.sidebar.GetSelectedInstance()
		tw.UpdateDiff(selected)
		tw.SetInstance(selected)
		m.menu.SetInstance(selected)
		m.menu.SetSidebarContext(sel.Kind, sel.IsHeader)
		if err := tw.UpdatePreview(selected); err != nil {
			return m.handleError(err)
		}
		if err := tw.UpdateTerminal(selected); err != nil {
			return m.handleError(err)
		}
	case sel.Kind == ui.SectionTodos:
		m.contentPane.SetMode(ui.ContentModeTodos)
		m.menu.SetInstance(nil)
		m.menu.SetSidebarContext(sel.Kind, sel.IsHeader)
	case sel.Kind == ui.SectionSchedules:
		m.contentPane.SetMode(ui.ContentModeSchedules)
		m.menu.SetInstance(nil)
		m.menu.SetSidebarContext(sel.Kind, sel.IsHeader)
	case sel.Kind == ui.SectionMicroClaw:
		m.contentPane.SetMode(ui.ContentModeMicroClaw)
		m.menu.SetInstance(nil)
		m.menu.SetSidebarContext(sel.Kind, sel.IsHeader)
	default:
		// On section headers, show the instance preview if available
		if sel.Kind == ui.SectionInstances {
			if m.sidebar.NumInstances() > 0 {
				m.contentPane.SetMode(ui.ContentModeInstance)
			} else {
				m.contentPane.SetMode(ui.ContentModeEmpty)
			}
		} else {
			m.contentPane.SetMode(ui.ContentModeEmpty)
		}
		m.menu.SetInstance(nil)
		m.menu.SetSidebarContext(sel.Kind, sel.IsHeader)
	}

	return nil
}

type keyupMsg struct{}

func (m *home) keydownCallback(name keys.KeyName) tea.Cmd {
	m.menu.Keydown(name)
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
		return keyupMsg{}
	}
}

type hideErrMsg struct{}
type previewTickMsg struct{}
type tickUpdateMetadataMessage struct{}
type tickUpdatePRInfoMessage struct{}
type instanceChangedMsg struct{}

type instanceStartedMsg struct {
	instance        *session.Instance
	err             error
	promptAfterName bool
}

var tickUpdateMetadataCmd = func() tea.Msg {
	time.Sleep(500 * time.Millisecond)
	return tickUpdateMetadataMessage{}
}

var tickUpdatePRInfoCmd = func() tea.Msg {
	time.Sleep(60 * time.Second)
	return tickUpdatePRInfoMessage{}
}

func (m *home) handleError(err error) tea.Cmd {
	log.ErrorLog.Printf("%v", err)
	m.errBox.SetError(err)
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
		case <-time.After(3 * time.Second):
		}
		return hideErrMsg{}
	}
}

func (m *home) confirmAction(message string, action tea.Cmd) tea.Cmd {
	m.state = stateConfirm
	m.confirmationOverlay = overlay.NewConfirmationOverlay(message)
	m.confirmationOverlay.SetWidth(50)

	m.confirmationOverlay.OnConfirm = func() {
		m.state = stateDefault
		if action != nil {
			if msg := action(); msg != nil {
				if err, ok := msg.(error); ok {
					log.ErrorLog.Printf("confirmation action failed: %v", err)
					m.errBox.SetError(err)
				}
			}
		}
	}

	m.confirmationOverlay.OnCancel = func() {
		m.state = stateDefault
	}

	return nil
}

func (m *home) View() string {
	sidebarWithPadding := lipgloss.NewStyle().PaddingTop(1).Render(m.sidebar.String())
	contentWithPadding := lipgloss.NewStyle().PaddingTop(1).Render(m.contentPane.String())
	sidebarAndContent := lipgloss.JoinHorizontal(lipgloss.Top, sidebarWithPadding, contentWithPadding)

	mainView := lipgloss.JoinVertical(
		lipgloss.Center,
		sidebarAndContent,
		m.menu.String(),
		m.errBox.String(),
	)

	if m.state == statePrompt {
		if m.textInputOverlay == nil {
			log.ErrorLog.Printf("text input overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.textInputOverlay.Render(), mainView, true)
	} else if m.state == stateHelp {
		if m.textOverlay == nil {
			log.ErrorLog.Printf("text overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.textOverlay.Render(), mainView, true)
	} else if m.state == stateConfirm {
		if m.confirmationOverlay == nil {
			log.ErrorLog.Printf("confirmation overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.confirmationOverlay.Render(), mainView, true)
	} else if m.state == stateSelectWorktree {
		if m.selectionOverlay == nil {
			log.ErrorLog.Printf("selection overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.selectionOverlay.Render(), mainView, true)
	} else if m.state == stateSearch {
		if m.searchOverlay == nil {
			log.ErrorLog.Printf("search overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.searchOverlay.Render(), mainView, true)
	} else if m.state == stateHooks {
		if m.hooksOverlay == nil {
			log.ErrorLog.Printf("hooks overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.hooksOverlay.Render(), mainView, true)
	}

	return mainView
}
