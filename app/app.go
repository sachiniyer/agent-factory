package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sachiniyer/agent-factory/board"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/microclaw"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	// stateLinkInstance is the state when the user is selecting an instance to link to a task.
	stateLinkInstance
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
	// selectedWorktree stores the worktree info selected by the user for attach
	selectedWorktree *git.WorktreeInfo
	// availableWorktrees stores the worktrees shown in the selection overlay
	availableWorktrees []git.WorktreeInfo
	// linkingTaskID is the task ID being linked to an instance
	linkingTaskID string

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

	// Merge pending instances from task runs.
	pendingData, err := task.LoadAndClearPendingInstances()
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

	// Load tasks for sidebar display
	tasks, err := task.LoadTasksForCurrentRepo()
	if err != nil {
		log.WarningLog.Printf("Failed to load tasks: %v", err)
	} else {
		h.sidebar.SetTasks(tasks)
	}

	// Load board for sidebar display and kanban pane
	b, err := board.LoadBoard()
	if err != nil {
		log.WarningLog.Printf("Failed to load board: %v", err)
	} else {
		h.sidebar.SetTaskCount(b.TaskCount())
		h.contentPane.KanbanPane().SetBoard(b)
	}

	// Load tasks into task pane
	if len(tasks) > 0 {
		h.contentPane.TaskPane().SetTasks(tasks)
	}

	// Load hooks for sidebar display and hooks pane
	repoCfg, err := config.LoadRepoConfig(repoID)
	if err != nil {
		log.WarningLog.Printf("Failed to load repo config: %v", err)
	} else {
		h.sidebar.SetHookCount(len(repoCfg.PostWorktreeCommands))
		h.contentPane.HooksPane().SetCommands(repoCfg.PostWorktreeCommands)
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
	// Save any dirty board/task state
	m.saveContentPaneState()

	if err := m.storage.SaveInstances(m.sidebar.GetInstances()); err != nil {
		return m, m.handleError(err)
	}
	tw := m.contentPane.TabbedWindow()
	tw.CleanupTerminal()
	m.cleanupMicroClaw()
	return m, tea.Quit
}

// saveContentPaneState persists any changes from the board/task panes.
func (m *home) saveContentPaneState() {
	kp := m.contentPane.KanbanPane()
	if kp.IsDirty() {
		if b := kp.GetBoard(); b != nil {
			if err := board.SaveBoard(b); err != nil {
				log.ErrorLog.Printf("failed to save board: %v", err)
			}
			m.sidebar.SetTaskCount(b.TaskCount())
		}
	}

	hp := m.contentPane.HooksPane()
	if hp.IsDirty() {
		repoCfg, err := config.LoadRepoConfig(m.repoID)
		if err != nil {
			repoCfg = &config.RepoConfig{}
		}
		repoCfg.PostWorktreeCommands = hp.GetCommands()
		if err := config.SaveRepoConfig(m.repoID, repoCfg); err != nil {
			log.ErrorLog.Printf("failed to save hooks: %v", err)
		}
		m.sidebar.SetHookCount(len(hp.GetCommands()))
	}

	sp := m.contentPane.TaskPane()
	if sp.IsDirty() {
		for _, tsk := range sp.GetTasks() {
			if err := task.UpdateTask(tsk); err != nil {
				log.ErrorLog.Printf("failed to update task: %v", err)
			}
			if tsk.Enabled {
				if err := task.InstallSystemdTimer(tsk); err != nil {
					log.WarningLog.Printf("failed to install timer: %v", err)
				}
			} else {
				if err := task.RemoveSystemdTimer(tsk); err != nil {
					log.WarningLog.Printf("failed to remove timer: %v", err)
				}
			}
		}
		for _, tsk := range sp.GetDeleted() {
			if err := task.RemoveTask(tsk.ID); err != nil {
				log.ErrorLog.Printf("failed to remove task: %v", err)
			}
			if err := task.RemoveSystemdTimer(tsk); err != nil {
				log.WarningLog.Printf("failed to remove timer: %v", err)
			}
		}
		// Refresh sidebar
		tasks, err := task.LoadTasksForCurrentRepo()
		if err == nil {
			m.sidebar.SetTasks(tasks)
		}
	}
}

// handleTaskCreate processes a pending task creation from the inline form.
func (m *home) handleTaskCreate() tea.Cmd {
	sp := m.contentPane.TaskPane()
	name, prompt, cronExpr, projectPath := sp.ConsumePendingCreate()

	if name == "" {
		return m.handleError(fmt.Errorf("task name is required"))
	}
	if err := task.ValidateCronExpr(cronExpr); err != nil {
		return m.handleError(fmt.Errorf("invalid cron: %v", err))
	}
	absPath, err := filepath.Abs(projectPath)
	if err != nil {
		return m.handleError(fmt.Errorf("invalid path: %v", err))
	}
	t := task.Task{
		ID:          task.GenerateID(),
		Name:        name,
		Prompt:      prompt,
		CronExpr:    cronExpr,
		ProjectPath: absPath,
		Program:     m.program,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
	if err := task.AddTask(t); err != nil {
		return m.handleError(fmt.Errorf("failed to save task: %v", err))
	}
	if err := task.InstallSystemdTimer(t); err != nil {
		log.WarningLog.Printf("failed to install systemd timer: %v", err)
	}
	// Refresh sidebar and task pane
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		m.sidebar.SetTasks(tasks)
		sp.SetTasks(tasks)
	}
	return nil
}

// handleTaskTrigger immediately spawns an instance for the selected task.
func (m *home) handleTaskTrigger() tea.Cmd {
	sp := m.contentPane.TaskPane()
	tsk := sp.ConsumePendingTrigger()
	if tsk == nil {
		return m.handleError(fmt.Errorf("no task selected"))
	}

	if m.sidebar.NumInstances() >= GlobalInstanceLimit {
		return m.handleError(fmt.Errorf("you can't create more than %d instances", GlobalInstanceLimit))
	}

	title := tsk.Name
	if title == "" {
		title = fmt.Sprintf("task-%s", tsk.ID)
	}

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    tsk.ProjectPath,
		Program: tsk.Program,
	})
	if err != nil {
		return m.handleError(fmt.Errorf("failed to create instance: %w", err))
	}

	finalizer := m.sidebar.AddInstance(instance)
	m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
	instance.SetStatus(session.Loading)
	finalizer()
	m.menu.SetState(ui.StateDefault)

	// Create a board task linked to the new instance.
	kp := m.contentPane.KanbanPane()
	if b := kp.GetBoard(); b != nil {
		taskTitle := tsk.Name
		if taskTitle == "" {
			taskTitle = title
		}
		bt := b.AddTask(taskTitle, "in_progress")
		b.LinkTask(bt.ID, title)
		if err := board.SaveBoard(b); err != nil {
			log.ErrorLog.Printf("failed to save board task: %v", err)
		}
		m.sidebar.SetTaskCount(b.TaskCount())
	}

	prompt := tsk.Prompt
	taskID := tsk.ID
	startCmd := func() tea.Msg {
		if err := instance.Start(true); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		if err := task.WaitForReady(instance); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		if instance.CheckAndHandleTrustPrompt() {
			time.Sleep(1 * time.Second)
			if err := task.WaitForReady(instance); err != nil {
				return instanceStartedMsg{instance: instance, err: err}
			}
		}

		if err := instance.SendPromptCommand(prompt); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		// Update task last run status.
		if t, err := task.GetTask(taskID); err == nil {
			now := time.Now()
			t.LastRunAt = &now
			t.LastRunStatus = "triggered"
			if err := task.UpdateTask(*t); err != nil {
				log.ErrorLog.Printf("failed to update task status: %v", err)
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
	if m.state == statePrompt || m.state == stateHelp || m.state == stateConfirm || m.state == stateSelectWorktree || m.state == stateLinkInstance {
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

	// Dispatch to state-specific handlers
	switch m.state {
	case stateHelp:
		return m.handleHelpState(msg)
	case stateNew:
		return m.handleStateNew(msg)
	case statePrompt:
		return m.handleStatePrompt(msg)
	case stateSelectWorktree:
		return m.handleStateSelectWorktree(msg)
	case stateLinkInstance:
		return m.handleStateLinkInstance(msg)
	case stateConfirm:
		return m.handleStateConfirm(msg)
	case stateSearch:
		return m.handleStateSearch(msg)
	}

	// Route keys to content pane if it has focus (e.g., editing board/tasks)
	if mod, cmd, consumed := m.handleContentPaneFocus(msg); consumed {
		return mod, cmd
	}

	// Exit scrolling mode when ESC is pressed
	if msg.Type == tea.KeyEsc {
		if m.contentPane.GetMode() == ui.ContentModeInstance {
			tw := m.contentPane.TabbedWindow()
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

	// Handle content pane Enter/attach for focusing (board/tasks/hooks)
	if mod, cmd, consumed := m.handleContentPaneEnter(msg, name); consumed {
		return mod, cmd
	}

	return m.handleDefaultKeyPress(msg, name)
}

// jumpToInstance navigates the sidebar to the instance with the given title.
func (m *home) jumpToInstance(title string) tea.Cmd {
	for _, inst := range m.sidebar.GetInstances() {
		if inst.Title == title {
			// Expand instances section
			m.sidebar.ExpandInstancesSection()
			m.sidebar.SelectInstance(inst)
			m.contentPane.SetMode(ui.ContentModeInstance)
			return m.selectionChanged()
		}
	}
	return m.handleError(fmt.Errorf("instance %q not found", title))
}

// attachToInstance finds the instance by title and attaches to it.
func (m *home) attachToInstance(title string) (tea.Model, tea.Cmd) {
	for _, inst := range m.sidebar.GetInstances() {
		if inst.Title == title {
			if inst.Status == session.Loading || !inst.TmuxAlive() {
				return m, m.handleError(fmt.Errorf("instance %q is not ready", title))
			}
			m.sidebar.ExpandInstancesSection()
			m.sidebar.SelectInstance(inst)
			m.contentPane.SetMode(ui.ContentModeInstance)
			m.showHelpScreen(helpTypeInstanceAttach{}, func() {
				ch, err := inst.Attach()
				if err != nil {
					log.ErrorLog.Printf("failed to attach to %s: %v", title, err)
					return
				}
				<-ch
				m.state = stateDefault
			})
			return m, nil
		}
	}
	return m, m.handleError(fmt.Errorf("instance %q not found", title))
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
	case sel.Kind == ui.SectionBoard:
		m.contentPane.SetMode(ui.ContentModeBoard)
		m.menu.SetInstance(nil)
		m.menu.SetSidebarContext(sel.Kind, sel.IsHeader)
	case sel.Kind == ui.SectionTasks:
		m.contentPane.SetMode(ui.ContentModeTasks)
		m.menu.SetInstance(nil)
		m.menu.SetSidebarContext(sel.Kind, sel.IsHeader)
	case sel.Kind == ui.SectionHooks:
		m.contentPane.SetMode(ui.ContentModeHooks)
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
	} else if m.state == stateLinkInstance {
		if m.selectionOverlay == nil {
			log.ErrorLog.Printf("selection overlay is nil")
		}
		return overlay.PlaceOverlay(0, 0, m.selectionOverlay.Render(), mainView, true)
	}

	return mainView
}
