package app

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type helpText interface {
	// toContent returns the help UI content.
	toContent() string
	// mask returns the bit mask for this help text. These are used to track which help screens
	// have been seen in the config and app state.
	mask() uint32
}

type helpTypeGeneral struct{}

type helpTypeInstanceStart struct {
	instance *session.Instance
}

type helpTypeInstanceAttach struct{}

func helpStart(instance *session.Instance) helpText {
	return helpTypeInstanceStart{instance: instance}
}

func (h helpTypeGeneral) toContent() string {
	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("Agent Factory"),
		"",
		"A terminal UI that manages multiple Claude Code (and other local agents) in separate workspaces.",
		"",
		headerStyle.Render("Managing:"),
		keyStyle.Render("n")+descStyle.Render("         - Create a new session"),
		keyStyle.Render("N")+descStyle.Render("         - Create a new session with a prompt"),
		keyStyle.Render("s")+descStyle.Render("         - Create a new task"),
		keyStyle.Render("S")+descStyle.Render("         - List tasks"),
		keyStyle.Render("r")+descStyle.Render("         - Run selected task now"),
		keyStyle.Render("D")+descStyle.Render("         - Kill (delete) the selected session"),
		keyStyle.Render("↑/j, ↓/k")+descStyle.Render("  - Navigate between sessions"),
		keyStyle.Render("↵/o")+descStyle.Render("       - Attach to the selected session"),
		keyStyle.Render(tmux.DetachKeyDisplay)+descStyle.Render("    - Detach from session"),
		"",
		headerStyle.Render("Sidebar:"),
		keyStyle.Render("↑/j, ↓/k")+descStyle.Render("  - Navigate sidebar items"),
		keyStyle.Render("h/←")+descStyle.Render("       - Collapse section or jump to header"),
		keyStyle.Render("l/→")+descStyle.Render("       - Expand section"),
		keyStyle.Render("[, ]")+descStyle.Render("      - Jump between section headers"),
		"",
		headerStyle.Render("Board (Kanban):"),
		keyStyle.Render("t")+descStyle.Render("         - Navigate to kanban board"),
		keyStyle.Render("j/k")+descStyle.Render("       - Navigate tasks (when focused)"),
		keyStyle.Render("h/l")+descStyle.Render("       - Jump between columns (when focused)"),
		keyStyle.Render("n")+descStyle.Render("         - Add new task (when focused)"),
		keyStyle.Render("m")+descStyle.Render("         - Grab/drop task to move between columns"),
		keyStyle.Render("d")+descStyle.Render("         - Delete task (when focused)"),
		keyStyle.Render("o")+descStyle.Render("         - Jump to linked instance"),
		keyStyle.Render("a")+descStyle.Render("         - Link session / attach to linked session"),
		"",
		headerStyle.Render("Configuration:"),
		keyStyle.Render("H")+descStyle.Render("         - Navigate to worktree hooks"),
		"",
		headerStyle.Render("GitHub PR:"),
		keyStyle.Render("p")+descStyle.Render("         - Open PR in browser"),
		keyStyle.Render("P")+descStyle.Render("         - Copy PR URL to clipboard"),
		"",
		headerStyle.Render("Other:"),
		keyStyle.Render("tab")+descStyle.Render("       - Switch between preview, diff, and terminal tabs"),
		keyStyle.Render("shift+tab")+descStyle.Render(" - Switch tabs in reverse"),
		keyStyle.Render("shift-↓/↑")+descStyle.Render(" - Scroll in preview/diff/terminal view"),
		keyStyle.Render("q")+descStyle.Render("         - Quit the application"),
	)
	return content
}

func (h helpTypeInstanceStart) toContent() string {
	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("Instance Created"),
		"",
		descStyle.Render("New session created:"),
		descStyle.Render(fmt.Sprintf("• Git branch: %s (isolated worktree)",
			lipgloss.NewStyle().Bold(true).Render(h.instance.Branch))),
		descStyle.Render(fmt.Sprintf("• %s running in background tmux session",
			lipgloss.NewStyle().Bold(true).Render(h.instance.Program))),
		"",
		headerStyle.Render("Managing:"),
		keyStyle.Render("↵/o")+descStyle.Render("   - Attach to the session to interact with it directly"),
		keyStyle.Render("tab")+descStyle.Render("   - Switch preview panes to view session diff"),
		keyStyle.Render("D")+descStyle.Render("     - Kill (delete) the selected session"),
	)
	return content
}

func (h helpTypeInstanceAttach) toContent() string {
	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("Attaching to Instance"),
		"",
		descStyle.Render("To detach from a session, press ")+keyStyle.Render(tmux.DetachKeyDisplay),
	)
	return content
}

func (h helpTypeGeneral) mask() uint32 {
	return 1
}

func (h helpTypeInstanceStart) mask() uint32 {
	return 1 << 1
}
func (h helpTypeInstanceAttach) mask() uint32 {
	return 1 << 2
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("#7D56F4"))
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#36CFC9"))
	keyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	descStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF"))
)

// showHelpScreen displays the help screen overlay if it hasn't been shown before
func (m *home) showHelpScreen(helpType helpText, onDismiss func()) (tea.Model, tea.Cmd) {
	// Get the flag for this help type
	var alwaysShow bool
	switch helpType.(type) {
	case helpTypeGeneral:
		alwaysShow = true
	}

	flag := helpType.mask()

	// Check if this help screen has been seen before
	// Only show if we're showing the general help screen or the corresponding flag is not set
	// in the seen bitmask.
	if alwaysShow || (m.appState.GetHelpScreensSeen()&flag) == 0 {
		// Mark this help screen as seen and save state
		if err := m.appState.SetHelpScreensSeen(m.appState.GetHelpScreensSeen() | flag); err != nil {
			log.WarningLog.Printf("failed to save help screen state: %v", err)
		}

		content := helpType.toContent()

		m.textOverlay = overlay.NewTextOverlay(content)
		m.textOverlay.OnDismiss = onDismiss
		m.state = stateHelp
		return m, nil
	}

	// Skip displaying the help screen
	if onDismiss != nil {
		onDismiss()
	}
	return m, nil
}

// handleHelpState handles key events when in help state
func (m *home) handleHelpState(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any key press will close the help overlay
	shouldClose := m.textOverlay.HandleKeyPress(msg)
	if shouldClose {
		m.state = stateDefault
		return m, tea.Sequence(
			tea.WindowSize(),
			func() tea.Msg {
				m.menu.SetState(ui.StateDefault)
				return nil
			},
		)
	}

	return m, nil
}
