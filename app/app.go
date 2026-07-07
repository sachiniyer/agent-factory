package app

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
)

var Version string

// Run is the main entrypoint into the application.
func Run(ctx context.Context, program string, autoYes bool, repo *config.RepoContext) error {
	p := tea.NewProgram(
		newHome(ctx, program, autoYes, repo),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(), // Mouse scroll
	)
	_, err := p.Run()
	return err
}
