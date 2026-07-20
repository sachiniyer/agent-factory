package app

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
)

var Version string

// Run is the main entrypoint into the application.
func Run(ctx context.Context, program string, autoYes bool, repo *config.RepoContext) error {
	h := newHome(ctx, program, autoYes, repo)
	p := tea.NewProgram(
		h,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(), // Mouse scroll
	)
	// Wire the terminal-handover seams a full-screen attach needs (#2157). The
	// attach is a raw stdin proxy, so for its duration the Program must stop
	// reading the terminal — otherwise its input reader and the attach's pump race
	// each other for every byte the user types or pastes. Bubble Tea's own
	// primitives; the same pair tea.Exec uses around a child that takes over the
	// terminal. Set here rather than in newHome because only Run has the Program.
	h.releaseTerminal = p.ReleaseTerminal
	h.restoreTerminal = p.RestoreTerminal
	_, err := p.Run()
	return err
}
