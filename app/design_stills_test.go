package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/stretchr/testify/require"
)

// Run exclusively in the playtest container. These supplement P4 recovery
// scenes with each chrome family; RPCs and terminal output are deterministic.
func TestDesignDriverScenes(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	source, err := os.Getwd()
	require.NoError(t, err)
	for _, mode := range []string{"light", "dark"} {
		for _, scene := range []string{"sessions-dense", "projects-degraded", "account-picker", "task-actions", "task-delete", "single-project", "multiple-projects", "preview-help", "search-overflow", "selection-overflow", "project-picker-overflow", "config-edit", "account-register", "hooks-edit", "hooks-add", "rail-task-selection", "rail-project-selection", "notice", "failure-notice", "project-picker-existing", "archive-warning", "alarm", "pane", "keyboard", "preview", "hooks", "config", "accounts", "sessions", "tasks", "task-create", "task-schedule", "task-weekdays", "task-weekdays-unchecked", "task-trigger", "task-program", "task-schedule-type", "help", "confirmation", "search", "project-picker", "selection", "prompt"} {
			t.Run(scene+"-"+mode, func(t *testing.T) {
				lipgloss.SetHasDarkBackground(mode == "dark")
				h := newTestHome(t)
				h.termWidth, h.termHeight = 120, 36
				h.repoRoot = "/project"
				inst := newLoadingInstance(t, "Apply design roles")
				inst.SetStatusForTest(session.Ready)
				h.store.AddInstance(inst)
				h.sidebar.SelectInstance(inst)
				h.relayout()
				switch scene {
				case "sessions-dense":
					for i, status := range []session.Status{session.Running, session.Ready, session.Lost, session.Dead} {
						other := newLoadingInstance(t, fmt.Sprintf("Review change %d", i+1))
						other.SetStatusForTest(status)
						other.Branch = fmt.Sprintf("review-%d", i+1)
						h.store.AddInstance(other)
					}
					h.sidebar.SelectInstance(inst)
					h.relayout()
				case "archive-warning":
					inst.ReconcileArchiveWarning("Archive incomplete: complete original tree retained at /retained/source")
				case "alarm":
					h.alarmBanner.SetAlarms([]ui.AlarmInfo{{TaskName: "Review intake", Target: "Apply design roles", Pending: 3}})
					h.relayout()
				case "pane", "keyboard", "preview":
					setPreviewText(inst, "Design roles are applied.\nAgent-owned output stays intact.")
					pane := openTestPane(t, h, inst, 0)
					window := h.paneWindows[pane.ID()]
					require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(window, inst, 0, window.ContentSeq())())
					if scene == "keyboard" {
						window.SetInteractive(true)
					}
					if scene == "preview" {
						window.SetPreview(inst, 0, "Origin session")
					}
				case "config", "config-edit":
					h.state = stateConfigEditor
					h.configPane.SetEntries([]config.ConfigEntry{{Key: "default_program", Type: "string", Value: "claude", Purpose: "Agent for new sessions", Tier: 1, TierName: "Essentials", Settable: true, Enum: []string{"claude", "codex"}}}, "Local daemon · /home/operator/.agent-factory/config.toml")
					h.configPane.SetFocus(true)
					if scene == "config-edit" {
						h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
					}
				case "account-register":
					h.state = stateConfigEditor
					h.configPane.SetAccounts(nil, []string{"claude"}, nil)
					h.configPane.SetFocus(true)
					h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
					h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("work")})
				case "accounts":
					h.state = stateConfigEditor
					h.configPane.SetAccounts([]ui.AccountRow{{Agent: "claude", Name: "work", LoggedIn: true}, {Agent: "codex", Name: "personal"}}, []string{"claude", "codex"}, nil)
					h.configPane.SetFocus(true)
				case "hooks", "hooks-edit", "hooks-add":
					h.state = stateHooks
					h.hooksPane.SetCommands([]string{"make test", "make lint"})
					h.hooksPane.SetFocus(true)
					if scene == "hooks-edit" {
						h.hooksPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
					}
					if scene == "hooks-add" {
						h.hooksPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
						h.hooksPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("make vet")})
					}

				case "tasks", "task-actions", "task-delete":
					h.state = stateTasks
					next := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
					h.automations.TaskPane().SetTasks([]task.Task{{NextRunAt: &next, ID: "design", Name: "Daily design review", CronExpr: "0 9 * * *", Prompt: "Review the interface changes", Enabled: true}, {ID: "watch", Name: "Issue intake", WatchCmd: "gh-issue-watch", Enabled: true}, {ID: "paused", Name: "Release notes"}})
					h.automations.TaskPane().SetFocus(true)
					if scene == "task-actions" {
						h.automations.TaskPane().HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
					}
					if scene == "task-delete" {
						h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
					}
				case "single-project", "multiple-projects", "projects-degraded":
					rows := []ui.SidebarProject{{Name: "Interface", Root: "/project"}}
					if scene != "single-project" {
						rows = append(rows, ui.SidebarProject{Name: "Tools", Root: "/tools"})
					}
					h.projects.SetProjects(rows)
					h.projects.SetDegraded(scene == "projects-degraded")
					h.sidebar.SetProjectName("Interface")
					h.relayout()
				case "preview-help":
					pane := openTestPane(t, h, inst, 0)
					h.paneWindows[pane.ID()].SetPreview(inst, 0, "Origin session")
					h.showHelpScreen(helpTypeGeneral{}, nil)
				case "account-picker":
					h.state = stateSelectAccount
					h.selectionOverlay = overlay.NewSelectionOverlay("Select claude account", []string{"work", "personal"})
					h.selectionOverlay.SetMaxSize(100, 30)
				case "task-trigger", "task-program", "task-schedule-type":
					h.state = stateTasks
					pane := h.automations.TaskPane()
					pane.EnterCreateMode(h.repoRoot)
					count := map[string]int{"task-trigger": 1, "task-program": 7, "task-schedule-type": 2}[scene]
					for n := 0; n < count; n++ {
						pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
					}
				case "task-create", "task-schedule", "task-weekdays", "task-weekdays-unchecked":
					h.state = stateTasks
					h.automations.TaskPane().EnterCreateMode(h.repoRoot)
					if scene != "task-create" {
						pane := h.automations.TaskPane()
						pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
						pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
						if scene == "task-weekdays" || scene == "task-weekdays-unchecked" {
							pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
							for n := 0; n < 4; n++ {
								pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
							}
							if scene == "task-weekdays-unchecked" {
								pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeySpace})
							}
						} else {
							pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
						}
					}
				case "help":
					h.showHelpScreen(helpTypeGeneral{}, nil)
				case "confirmation":
					h.confirmActionWithDetail("Kill Apply design roles? Its running process will stop.", "The worktree and conversation remain available.", nil)
				case "search-overflow", "selection-overflow", "project-picker-overflow":
					var items []string
					var instances []*session.Instance
					var projects []overlay.Project
					for i := 0; i < 40; i++ {
						name := fmt.Sprintf("Review item %02d", i)
						items = append(items, name)
						instances = append(instances, &session.Instance{Title: name})
						projects = append(projects, overlay.Project{Name: name, Root: name})
					}
					switch scene {
					case "search-overflow":
						h.state = stateSearch
						h.searchOverlay = overlay.NewSearchOverlay(instances)
						h.searchOverlay.SetMaxSize(80, 20)
						h.searchOverlay.SetSelectedIndex(20)
					case "selection-overflow":
						h.state = stateSelectProgram
						h.selectionOverlay = overlay.NewSelectionOverlay("Select program", items)
						h.selectionOverlay.SetMaxSize(80, 20)
						h.selectionOverlay.SetSelectedIndex(20)
					case "project-picker-overflow":
						h.state = stateSwitchProject
						h.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, projects[20].Root)
						h.projectPickerOverlay.SetMaxSize(80, 20)
					}
				case "search":
					h.state = stateSearch
					results := []*session.Instance{inst}
					for title, status := range []session.Status{session.Running, session.Lost, session.Dead, session.Archived, session.Loading} {
						result := newLoadingInstance(t, []string{"Running session", "Lost session", "Dead session", "Archived session", "Creating session"}[title])
						result.SetStatusForTest(status)
						results = append(results, result)
					}
					h.searchOverlay = overlay.NewSearchOverlay(results)
					h.searchOverlay.SetMaxSize(100, 30)
				case "notice", "failure-notice":
					if scene == "failure-notice" {
						h.errBox.SetError(fmt.Errorf("Cannot save configuration: the file is read-only"))
					} else {
						h.errBox.SetNotice(fmt.Errorf("Configuration saved; restart the session to apply it"))
					}
				case "rail-task-selection", "rail-project-selection":
					h.store.SetTasks([]task.Task{{ID: "design", Name: "Daily review", Enabled: true, CronExpr: "0 9 * * *"}})
					h.projects.SetProjects([]ui.SidebarProject{{Name: "Agent Factory", Root: h.repoRoot, Active: true}, {Name: "Second project", Root: "/second"}})
					h.relayout()
					if scene == "rail-task-selection" {
						h.automations.Focus()
					} else {
						h.projects.Focus()
					}
				case "project-picker", "project-picker-existing":
					h.state = stateSwitchProject
					h.projectPickerOverlay = overlay.NewProjectPickerOverlay(nil, h.repoRoot)
					if scene == "project-picker-existing" {
						h.projectPickerOverlay = overlay.NewProjectPickerOverlay([]overlay.Project{{Name: "Agent Factory", Root: h.repoRoot, SessionCount: 4}}, h.repoRoot)
					}
					h.projectPickerOverlay.SetMaxSize(100, 30)
				case "selection":
					h.state = stateSelectProgram
					h.selectionOverlay = overlay.NewSelectionOverlay("Select program", []string{"claude", "codex", "aider"})
					h.selectionOverlay.SetMaxSize(100, 30)
				case "prompt":
					h.state = statePromptInput
					h.promptOverlay = overlay.NewPromptOverlay("Initial prompt", "Apply the fixed design roles")
					h.promptOverlay.SetMaxSize(100, 30)
				}
				frame := h.View()
				svg := recoverySVG(frame, mode, 120, 36)
				name := scene + "-" + mode
				if out := os.Getenv("AF_TUI_DESIGN_CAPTURE"); out != "" {
					require.NoError(t, os.MkdirAll(out, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(out, name+".svg"), []byte(svg), 0644))
					require.NoError(t, os.WriteFile(filepath.Join(out, name+".ansi"), []byte(frame), 0644))
					return
				}
				golden, err := os.ReadFile(filepath.Join(source, "testdata", "design", name+".svg"))
				require.NoError(t, err)
				require.Equal(t, string(golden), svg, "inspect and recapture design driver stills in the container")
			})
		}
	}
}
