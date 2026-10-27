package app

import tea "github.com/charmbracelet/bubbletea"

func cleanQuitCmd() tea.Cmd {
	return tea.Sequence(
		// Clear the ALT screen, then leave it. A clear AFTER ExitAltScreen lands
		// on the host screen, which inside tmux is the shell pane af was
		// launched from — erasing it leaves a blank pane where typed input
		// lands invisibly in scrollback (#4785).
		tea.ClearScreen,
		tea.ExitAltScreen,
		tea.Quit,
	)
}
