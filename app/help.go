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
		titleStyle.Render(fmt.Sprintf("Agent Factory v%s", Version)),
		"",
		"A terminal UI that manages multiple Claude Code (and other local agents) in separate workspaces.",
		"",
		headerStyle.Render("Managing:"),
		keyStyle.Render("n")+descStyle.Render("         - Create a new session"),
		keyStyle.Render("N")+descStyle.Render("         - Create a new remote session (requires remote_hooks config)"),
		keyStyle.Render("S")+descStyle.Render("         - Manage tasks (n inside the manager creates one)"),
		keyStyle.Render("r")+descStyle.Render("         - Run selected task now"),
		keyStyle.Render("D")+descStyle.Render("         - Kill (delete) the selected session"),
		keyStyle.Render("↑/k, ↓/j")+descStyle.Render("  - Navigate between sessions"),
		keyStyle.Render("↵/o")+descStyle.Render("       - Attach to the selected session"),
		keyStyle.Render(tmux.DetachKeyDisplay)+descStyle.Render("    - Detach from session"),
		"",
		headerStyle.Render("Workspace:"),
		keyStyle.Render("tab")+descStyle.Render("       - Cycle focus: tree → pane A → pane B → automations"),
		keyStyle.Render("shift+tab")+descStyle.Render(" - Cycle focus backwards"),
		keyStyle.Render("s")+descStyle.Render("         - Split: open the selection in pane B / swap the panes"),
		keyStyle.Render("x")+descStyle.Render("         - Close the split (with pane B focused; w works too)"),
		keyStyle.Render("↑/k, ↓/j")+descStyle.Render("  - Navigate the tree (instances and their tabs)"),
		keyStyle.Render("h/←")+descStyle.Render("       - Collapse the selected instance's tabs"),
		keyStyle.Render("l/→")+descStyle.Render("       - Expand the selected instance's tabs"),
		"",
		headerStyle.Render("Configuration:"),
		keyStyle.Render("H")+descStyle.Render("         - Open the worktree hooks editor"),
		"",
		headerStyle.Render("GitHub PR:"),
		keyStyle.Render("p")+descStyle.Render("         - Open PR in browser"),
		keyStyle.Render("P")+descStyle.Render("         - Copy PR URL to clipboard"),
		"",
		headerStyle.Render("Tabs:"),
		keyStyle.Render("1-9")+descStyle.Render("       - Jump directly to a tab by number"),
		keyStyle.Render("t")+descStyle.Render("         - Open a new terminal tab"),
		keyStyle.Render("w")+descStyle.Render("         - Close the current tab (the agent tab can't be closed)"),
		keyStyle.Render("shift-↓/↑")+descStyle.Render(" - Scroll in the current tab"),
		"",
		headerStyle.Render("Other:"),
		keyStyle.Render("q")+descStyle.Render("         - Quit the application"),
	)
	return content
}

func (h helpTypeInstanceStart) toContent() string {
	// Remote instances block `t` (new tab) and `w` (close tab) — those actions
	// surface a "not available for remote" error (see handleNewTab /
	// handleCloseTab in app/handle_actions.go) — so only advertise the tab keys
	// that actually work for the instance type. 1-9 jump works for both (#988);
	// tabs also live in the left-rail tree since the layout cutover (#1024 PR 4).
	tabHelp := keyStyle.Render("1-9 jump") + descStyle.Render(" - Switch between tabs (t new tab, w close; tabs live in the tree)")
	if h.instance.IsRemote() {
		tabHelp = keyStyle.Render("1-9 jump") + descStyle.Render(" - Switch between tabs (tabs live in the tree)")
	}
	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("Instance Created"),
		"",
		descStyle.Render("New session created:"),
		descStyle.Render(fmt.Sprintf("• Git branch: %s (isolated worktree)",
			lipgloss.NewStyle().Bold(true).Render(h.instance.GetBranch()))),
		descStyle.Render(fmt.Sprintf("• %s running in background tmux session",
			lipgloss.NewStyle().Bold(true).Render(h.instance.Program))),
		"",
		headerStyle.Render("Managing:"),
		keyStyle.Render("↵/o")+descStyle.Render("   - Attach to the session to interact with it directly"),
		tabHelp,
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
	titleStyle  = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(ui.AccentColor)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#36CFC9"))
	keyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	descStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF"))
)

// showHelpScreen displays the help screen overlay if it hasn't been shown
// before. onDismiss may return a tea.Cmd; this function forwards that cmd
// back to the bubbletea event loop, which is how the attach path dispatches
// repaintAfterDetachMsg{} right after `<-ch` unblocks (#579).
func (m *home) showHelpScreen(helpType helpText, onDismiss func() tea.Cmd) (tea.Model, tea.Cmd) {
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
		return m, onDismiss()
	}
	return m, nil
}

// handleHelpState handles key events when in help state
func (m *home) handleHelpState(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any key press will close the help overlay
	dismissCmd, shouldClose := m.textOverlay.HandleKeyPress(msg)
	if shouldClose {
		m.state = stateDefault
		// Menu.SetState rebuilds the options slice; call it synchronously
		// on the event-loop goroutine rather than from a tea.Cmd closure
		// that runs off-loop and races with home.View -> Menu.String.
		m.menu.SetState(ui.StateDefault)
		// dismissCmd forwards repaintAfterDetachMsg{} from the attach
		// callback (#579) so the post-detach repaint doesn't have to wait
		// for the next previewTickMsg cycle.
		return m, tea.Batch(tea.WindowSize(), dismissCmd)
	}

	return m, nil
}
