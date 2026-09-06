package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
		for _, scene := range []string{"config-edit", "account-register", "hooks-edit", "hooks-add", "rail-task-selection", "rail-project-selection", "notice", "failure-notice", "project-picker-existing", "archive-warning", "alarm", "pane", "keyboard", "preview", "hooks", "config", "accounts", "sessions", "tasks", "task-create", "task-schedule", "task-weekdays", "task-trigger", "task-program", "task-schedule-type", "help", "confirmation", "search", "project-picker", "selection", "prompt"} {
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

				case "tasks":
					h.state = stateTasks
					h.automations.TaskPane().SetTasks([]task.Task{{ID: "design", Name: "Daily design review", Enabled: true}, {ID: "nightly", Name: "Nightly checks", Enabled: true, CronExpr: "0 2 * * *"}})
					h.automations.TaskPane().SetFocus(true)
				case "task-trigger", "task-program", "task-schedule-type":
					h.state = stateTasks
					pane := h.automations.TaskPane()
					pane.EnterCreateMode(h.repoRoot)
					count := map[string]int{"task-trigger": 1, "task-program": 6, "task-schedule-type": 2}[scene]
					for n := 0; n < count; n++ {
						pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
					}
				case "task-create", "task-schedule", "task-weekdays":
					h.state = stateTasks
					h.automations.TaskPane().EnterCreateMode(h.repoRoot)
					if scene != "task-create" {
						pane := h.automations.TaskPane()
						pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
						pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
						if scene == "task-weekdays" {
							pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
							for n := 0; n < 4; n++ {
								pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
							}
						} else {
							pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
						}
					}
				case "help":
					h.showHelpScreen(helpTypeGeneral{}, nil)
				case "confirmation":
					h.confirmActionWithDetail("Kill Apply design roles? Its running process will stop.", "The worktree and conversation remain available.", nil)
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
