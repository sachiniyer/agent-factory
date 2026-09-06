package app

import (
	"os"
	"path/filepath"
	"testing"

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
		for _, scene := range []string{"alarm", "pane", "keyboard", "preview", "hooks", "config", "accounts", "sessions", "tasks", "task-create", "help", "confirmation", "search", "project-picker", "selection", "prompt"} {
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
				case "config":
					h.state = stateConfigEditor
					h.configPane.SetEntries([]config.ConfigEntry{{Key: "default_program", Type: "string", Value: "claude", Purpose: "Agent for new sessions", Tier: 1, TierName: "Essentials", Settable: true, Enum: []string{"claude", "codex"}}}, "Local daemon · /home/operator/.agent-factory/config.toml")
					h.configPane.SetFocus(true)
				case "accounts":
					h.state = stateConfigEditor
					h.configPane.SetAccounts([]ui.AccountRow{{Agent: "claude", Name: "work", LoggedIn: true}, {Agent: "codex", Name: "personal"}}, []string{"claude", "codex"}, nil)
					h.configPane.SetFocus(true)
				case "hooks":
					h.state = stateHooks

				case "tasks":
					h.state = stateTasks
					h.automations.TaskPane().SetTasks([]task.Task{{ID: "design", Name: "Daily design review", Enabled: true}})
					h.automations.TaskPane().SetFocus(true)
				case "task-create":
					h.state = stateTasks
					h.automations.TaskPane().EnterCreateMode(h.repoRoot)
				case "help":
					h.showHelpScreen(helpTypeGeneral{}, nil)
				case "confirmation":
					h.confirmActionWithDetail("Kill Apply design roles? Its running process will stop.", "The worktree and conversation remain available.", nil)
				case "search":
					h.state = stateSearch
					results := []*session.Instance{inst}
					for title, status := range []session.Status{session.Lost, session.Dead, session.Archived} {
						result := newLoadingInstance(t, []string{"Lost session", "Dead session", "Archived session"}[title])
						result.SetStatusForTest(status)
						results = append(results, result)
					}
					h.searchOverlay = overlay.NewSearchOverlay(results)
					h.searchOverlay.SetMaxSize(100, 30)
				case "project-picker":
					h.state = stateSwitchProject
					h.projectPickerOverlay = overlay.NewProjectPickerOverlay(nil, h.repoRoot)
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
