package app

import tea "github.com/charmbracelet/bubbletea"

func cleanQuitCmd() tea.Cmd {
	return tea.Sequence(
		tea.ClearScreen,
		tea.ExitAltScreen,
		tea.ClearScreen,
		tea.Quit,
	)
}
