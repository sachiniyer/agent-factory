package ui

import (
	"errors"
	xansi "github.com/charmbracelet/x/ansi"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestHookEditAndAddUseRaisedField(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		roles := CurrentTheme()
		field := lipgloss.NewStyle().Bold(true).Foreground(roles.Ink).Background(roles.SurfaceRaised)
		for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune{'n'}}} {
			pane := NewHooksPane()
			pane.SetCommands([]string{"make test"})
			pane.SetSize(80, 20)
			pane.SetFocus(true)
			pane.HandleKeyPress(key)
			pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" updated")})
			require.Contains(t, pane.String(), SelectionMarker("▸ ")+field.Render(pane.editBuffer)+InputCaret())
			require.Equal(t, 1, strings.Count(xansi.Strip(pane.String()), "▸ "), "only the active edit/add field has the cursor")
		}
	}
}

func TestTaskHeaderMetadataRoles(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		roles := CurrentTheme()
		for _, enabled := range []bool{false, true} {
			for _, focused := range []bool{false, true} {
				tsk := task.Task{ID: "one", Name: "Review", Enabled: enabled, CronExpr: "0 9 * * *"}
				pane := NewTaskPane()
				pane.SetSize(120, 20)
				pane.SetTasks([]task.Task{tsk})
				pane.SetFocus(focused)
				meta := "  " + taskTriggerSummary(tsk) + "  " + taskDeliverySummary(tsk)
				if focused {
					status := "[✓]"
					if !enabled {
						status = "[✗]"
					}
					require.Contains(t, pane.String(), lipgloss.NewStyle().Bold(true).Foreground(roles.Ink).Background(roles.SurfaceRaised).Render(status+"  Review"+meta))
				} else {
					require.Contains(t, pane.String(), lipgloss.NewStyle().Foreground(roles.InkMuted).Render(meta))
				}
			}
		}
	}
}

func TestFailureAndNoticeRoles(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		box := NewErrBox()
		box.SetSize(80, 1)
		for _, failed := range []bool{false, true} {
			role := CurrentTheme().Ink
			if failed {
				box.SetError(errors.New("Result"))
				role = CurrentTheme().Dead
			} else {
				box.SetNotice(errors.New("Result"))
			}
			require.Contains(t, box.String(), lipgloss.NewStyle().Foreground(role).Background(CurrentTheme().Surface).Render("Result"))
		}
	}
}

func TestProjectSelectionIncludesAccentCursor(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		pane := NewProjectsPane()
		pane.rect.W = 80
		for _, active := range []bool{false, true} {
			marker := "▸ "
			if active {
				marker = "▹ "
			}
			row := pane.projectRow(SidebarProject{Name: "Project", Active: active}, true)
			require.Contains(t, row, SelectionMarker(marker))
			require.NotContains(t, row, "●")
		}
	}
}

func TestConfigSelectionStylesValueAndAccountState(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		pane := NewConfigPane()
		pane.SetSize(100, 20)
		pane.SetEntries([]config.ConfigEntry{{Key: "default_program", Value: "claude", Type: "string", Settable: true, Tier: 1, TierName: "Essentials"}}, "")
		require.Contains(t, pane.String(), configSelectedStyle.Render("claude"))
		for _, loggedIn := range []bool{false, true} {
			state := "not logged in"
			if loggedIn {
				state = "logged in"
			}
			require.Contains(t, pane.renderAccountRow(pane.selectedIdx, AccountRow{Agent: "claude", Name: "work", LoggedIn: loggedIn}), configSelectedStyle.Render("  "+state))
		}
	}
}
