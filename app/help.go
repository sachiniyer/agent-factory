package app

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// helpKey renders the effective key glyph(s) for an action from the generated
// binding table, so a [keys] rebind surfaces in the help overlay exactly as
// it does in the bottom menu and dispatch (#1026 — one source of truth). The
// help text is the single place these bindings are shown in full, so it must
// never fall back to a hardcoded literal.
func helpKey(name keys.KeyName) string {
	return keys.GlobalKeyBindings[name].Help().Key
}

// helpRow is one key→description line in the help overlay. key is a
// pre-rendered glyph string (usually from helpKey); literal is used for the
// handful of entries with no rebindable action (e.g. the detach key or the
// run-task shortcut).
type helpRow struct {
	key  string
	desc string
}

// helpSection is a titled group of help rows.
type helpSection struct {
	title string
	rows  []helpRow
}

// renderHelpSections lays the sections out with a single key column whose width
// fits the widest effective key across ALL sections. When contentWidth is
// known, the description is a separate block: lipgloss wraps it beneath its
// first word, never beneath the key (#2577).
func renderHelpSections(header string, sections []helpSection, contentWidth int) string {
	keyWidth := 0
	for _, s := range sections {
		for _, r := range s.rows {
			if w := lipgloss.Width(r.key); w > keyWidth {
				keyWidth = w
			}
		}
	}

	var lines []string
	lines = append(lines, header, "")
	for _, s := range sections {
		lines = append(lines, headerStyle.Render(s.title))
		for _, r := range s.rows {
			if contentWidth <= 0 {
				pad := strings.Repeat(" ", keyWidth-lipgloss.Width(r.key)+2)
				lines = append(lines, keyStyle.Render(r.key)+pad+descStyle.Render("- "+r.desc))
				continue
			}

			keyColumnWidth := keyWidth + 2
			if keyColumnWidth+3 > contentWidth {
				keyColumnWidth = contentWidth / 2
				if keyColumnWidth < 1 {
					keyColumnWidth = 1
				}
			}
			descWidth := contentWidth - keyColumnWidth - 2
			if descWidth < 1 {
				descWidth = 1
			}
			keyBlock := lipgloss.NewStyle().Width(keyColumnWidth).Render(keyStyle.Render(r.key))
			dashBlock := descStyle.Render("- ")
			descBlock := lipgloss.NewStyle().Width(descWidth).Render(descStyle.Render(r.desc))
			lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top, keyBlock, dashBlock, descBlock))
		}
		lines = append(lines, "")
	}
	// Drop the trailing blank so the overlay isn't bottom-padded.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

type helpText interface {
	// toContent returns the help UI content.
	toContent() string
	// mask returns the bit mask for this help text. These are used to track which help screens
	// have been seen in the config and app state.
	mask() uint32
}

type responsiveHelpText interface {
	toContentWidth(width int) string
}

type helpTypeGeneral struct{}

type helpTypeInstanceStart struct {
	instance *session.Instance
}

type helpTypeInstanceAttach struct {
	agent string
}

// helpTypeInteractive is shown once, the first time the user enters a pane
// (#1089 PR 2): the sharpest edge of the interaction change is that every
// key now types into the agent, so the escape hatch leads (RFC §5.7).
type helpTypeInteractive struct{}

func helpStart(instance *session.Instance) helpText {
	return helpTypeInstanceStart{instance: instance}
}

// helpAttach scopes agent-specific copy to the agent tab. Shell/process tabs
// deliberately get no scroll hint: their child program is arbitrary, and a
// confident guess there would repeat the exact honesty bug this notice fixes.
func helpAttach(instance *session.Instance, tabIdx int) helpText {
	agent := ""
	if instance != nil && tabIdx == 0 {
		agent = instance.ResolvedPaneAgent()
	}
	return helpTypeInstanceAttach{agent: agent}
}

const (
	claudeAttachedScrollControls = "pgup/pgdn · ctrl+home/end · mouse wheel"
	codexAttachedScrollControls  = "ctrl+t opens transcript · then pgup/pgdn or home/end"
)

func attachedScrollControls(agent string) (string, string) {
	switch agent {
	case tmux.ProgramClaude:
		return "Claude", claudeAttachedScrollControls
	case tmux.ProgramCodex:
		return "Codex", codexAttachedScrollControls
	default:
		return "", ""
	}
}

func firstRunActionLine(actions string) string {
	return descStyle.Render(actions)
}

type helpAlias struct {
	label string
	msg   tea.KeyMsg
}

func visibleHelpAliases(aliases []helpAlias) string {
	bindings := []keys.KeyName{
		keys.KeyHelp,
		keys.KeyUp,
		keys.KeyDown,
		keys.KeyShiftUp,
		keys.KeyShiftDown,
	}

	var visible []string
	for _, alias := range aliases {
		shadowed := false
		for _, name := range bindings {
			if key.Matches(alias.msg, keys.GlobalKeyBindings[name]) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			visible = append(visible, alias.label)
		}
	}
	return strings.Join(visible, "/")
}

func helpPagingAliases() string {
	return visibleHelpAliases([]helpAlias{
		{label: "pgup", msg: tea.KeyMsg{Type: tea.KeyPgUp}},
		{label: "pgdn", msg: tea.KeyMsg{Type: tea.KeyPgDown}},
	})
}

func helpJumpAliases() string {
	return visibleHelpAliases([]helpAlias{
		{label: "home", msg: tea.KeyMsg{Type: tea.KeyHome}},
		{label: "end", msg: tea.KeyMsg{Type: tea.KeyEnd}},
	})
}

func (h helpTypeGeneral) toContent() string {
	return h.toContentWidth(0)
}

func (h helpTypeGeneral) toContentWidth(contentWidth int) string {
	// Every key glyph below is pulled from the generated binding table via
	// helpKey, so [keys] rebinds appear here identically to the bottom menu
	// (#1026). The tmux detach key is the one entry with no rebindable action
	// and stays literal; the task-manager run shortcut is folded into the
	// tasks row (its `r` is overlay-local, not the global restore key #1605).
	navKeys := helpKey(keys.KeyUp) + ", " + helpKey(keys.KeyDown)
	pageLine := descStyle.Render("Page: ")
	if aliases := helpPagingAliases(); aliases != "" {
		pageLine += descStyle.Render(aliases + " · ")
	}
	pageLine += keyStyle.Render(helpKey(keys.KeyShiftUp) + "/" + helpKey(keys.KeyShiftDown))
	lineControls := descStyle.Render("Line: ") + keyStyle.Render(navKeys)
	if aliases := helpJumpAliases(); aliases != "" {
		lineControls += descStyle.Render(" · " + aliases + " jump")
	}
	header := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render(fmt.Sprintf("Agent Factory v%s", Version)),
		"",
		"A terminal UI that manages multiple Claude Code (and other local agents) in separate workspaces.",
		"",
		pageLine,
		lineControls,
		descStyle.Render("Close: esc · ")+keyStyle.Render(helpKey(keys.KeyHelp))+descStyle.Render(" toggles help"),
	)
	return renderHelpSections(header, []helpSection{
		{title: "Managing:", rows: []helpRow{
			{helpKey(keys.KeyNew), "Create a new session"},
			{helpKey(keys.KeyNewRemote), "Create a new remote session (requires remote_hooks config)"},
			// The naming form's three optional fields, named here because its own
			// status-bar hints shed by terminal width (ui/menu.go hintDropOrder): on a
			// narrow bar this is the only surface that still advertises them.
			{helpKey(keys.KeyChangeProgram) + " / " + helpKey(keys.KeySetPrompt) + " / " + helpKey(keys.KeySetBackend),
				"While naming a new session: pick its agent / initial prompt / backend"},
			{helpKey(keys.KeySwitchProject), "Switch to another project (repo) in place"},
			{helpKey(keys.KeyTaskList), "Manage tasks (n inside the manager creates one, r runs one)"},
			{helpKey(keys.KeyKill), "Kill (delete) the selected session"},
			{helpKey(keys.KeyArchive), "Archive the selected live session"},
			{helpKey(keys.KeyRestore), "Restore the selected archived / lost / dead session"},
			{helpKey(keys.KeyLimitRetry), "Retry a session blocked at a usage limit (re-spawn + resume)"},
			{navKeys, "Navigate between sessions"},
			{helpKey(keys.KeyEnter), "Interact with the session in its pane (all keys go to it)"},
			{helpKey(keys.KeyExitInteractive), "Leave interactive mode (back to navigation)"},
			{helpKey(keys.KeyAttach), "Attach to the selected session full-screen"},
			{tmux.DetachKeyDisplay, "Detach from a full-screen session"},
		}},
		{title: "Workspace:", rows: []helpRow{
			{helpKey(keys.KeyTab), "Cycle focus: tree → open panes → automations"},
			{helpKey(keys.KeyShiftTab), "Cycle focus backwards"},
			{helpKey(keys.KeyOpenPane), "Open the selected tab as a pane (or focus its pane)"},
			{helpKey(keys.KeySplitPane), "Commit the current preview as another pane"},
			{helpKey(keys.KeyHidePane), "Hide the focused pane (the tab keeps running)"},
			{helpKey(keys.KeyPanePrev) + "/" + helpKey(keys.KeyPaneNext), "Move focus between open panes"},
			{navKeys, "Navigate the tree (sessions and their tabs)"},
			{helpKey(keys.KeyLeft), "Collapse the selected session's tabs"},
			{helpKey(keys.KeyRight), "Expand the selected session's tabs"},
		}},
		{title: "Configuration:", rows: []helpRow{
			{helpKey(keys.KeyConfigAgent), "Open the config agent to change your settings"},
			{helpKey(keys.KeyHooks), "Open the worktree hooks editor"},
			{helpKey(keys.KeyConfigEditor), "Open the global config editor"},
		}},
		{title: "GitHub PR:", rows: []helpRow{
			{helpKey(keys.KeyOpenPR), "Open PR in browser"},
			{helpKey(keys.KeyCopyPR), "Copy PR URL to clipboard"},
		}},
		{title: "Tabs:", rows: []helpRow{
			{helpKey(keys.KeyJumpTab), "Select a tab by number (s opens it, enter attaches)"},
			{helpKey(keys.KeyNewTab), "Choose a terminal or VS Code tab"},
			{helpKey(keys.KeyCloseTab), "Close the current tab (the agent tab can't be closed)"},
			{helpKey(keys.KeyShiftUp) + "/" + helpKey(keys.KeyShiftDown), "Scroll the current tab preview (navigation mode only)"},
		}},
		{title: "Full-screen scrolling:", rows: []helpRow{
			{"Claude", claudeAttachedScrollControls},
			{"Codex", codexAttachedScrollControls},
		}},
		{title: "Other:", rows: []helpRow{
			{helpKey(keys.KeyQuit), "Quit the application"},
		}},
	}, contentWidth)
}

func (h helpTypeInstanceStart) toContent() string {
	// Remote instances block `t` (new tab) and `w` (close tab) — those actions
	// surface a "not available for remote" error (see handleNewTab /
	// handleCloseTab in app/handle_actions.go) — so only advertise the tab keys
	// that actually work for the instance type. 1-9 jump works for both (#988);
	// tabs also live in the left-rail tree since the layout cutover (#1024 PR 4).
	openPane, newTab, closeTab := helpKey(keys.KeyOpenPane), helpKey(keys.KeyNewTab), helpKey(keys.KeyCloseTab)
	tabHelp := keyStyle.Render("1-9 jump") + descStyle.Render(fmt.Sprintf(" - Select a tab (%s opens it; %s new tab, %s close; tabs live in the tree)", openPane, newTab, closeTab))
	if !h.instance.Capabilities().TabManagement {
		tabHelp = keyStyle.Render("1-9 jump") + descStyle.Render(fmt.Sprintf(" - Select a tab (%s opens it; tabs live in the tree)", openPane))
	}
	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("Session created"),
		"",
		descStyle.Render("New session created:"),
		descStyle.Render(fmt.Sprintf("• Git branch: %s (isolated worktree)",
			lipgloss.NewStyle().Bold(true).Render(h.instance.GetBranch()))),
		descStyle.Render("• Agent process running in background tmux session"),
		"",
		headerStyle.Render("Managing:"),
		keyStyle.Render(helpKey(keys.KeyEnter))+descStyle.Render(fmt.Sprintf("     - Interact with the session in its pane (%s returns to nav)", helpKey(keys.KeyExitInteractive))),
		keyStyle.Render(helpKey(keys.KeyAttach))+descStyle.Render("     - Attach to the session full-screen"),
		keyStyle.Render(tmux.DetachKeyDisplay)+descStyle.Render("     - Detach from a full-screen session"),
		tabHelp,
		keyStyle.Render(helpKey(keys.KeyKill))+descStyle.Render("     - Kill (delete) the selected session"),
		"",
		headerStyle.Render("Actions:"),
		firstRunActionLine("enter continue · esc close"),
	)
	return content
}

func (h helpTypeInstanceAttach) toContent() string {
	lines := []string{
		titleStyle.Render("Attaching to session"),
		"",
		descStyle.Render("The attached program owns input and scrolling."),
		descStyle.Render("AF's ") + keyStyle.Render(helpKey(keys.KeyShiftUp)+"/"+helpKey(keys.KeyShiftDown)) + descStyle.Render(" preview scrolling works only in navigation mode."),
	}
	if agent, controls := attachedScrollControls(h.agent); agent != "" {
		lines = append(lines, descStyle.Render(fmt.Sprintf("%s owns attached scrolling: %s.", agent, controls)))
	}
	lines = append(lines,
		"",
		firstRunActionLine("enter attach full-screen · esc cancel"),
		descStyle.Render("Detach later with ")+keyStyle.Render(tmux.DetachKeyDisplay),
	)
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

func (h helpTypeInteractive) toContent() string {
	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("Interactive pane"),
		"",
		descStyle.Render("You are typing into this pane's terminal: every key — including tab —"),
		descStyle.Render("goes to the agent/shell. The pane's frame turns green while it has the"),
		descStyle.Render("keyboard, and the sessions rail stays visible."),
		"",
		descStyle.Render("Press ")+keyStyle.Render(helpKey(keys.KeyExitInteractive))+descStyle.Render(" to return to navigation."),
		descStyle.Render("Full-screen attach is still available on ")+keyStyle.Render(helpKey(keys.KeyAttach))+descStyle.Render(" (from nav mode)."),
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

func (h helpTypeInteractive) mask() uint32 {
	return 1 << 3
}

// layoutTextOverlay sizes help/intro text overlays to fit the terminal. The
// overlay itself decides whether the content needs height-windowing, so short
// one-shot help screens stay compact while the general help becomes scrollable
// at 80x24 (#1290).
func (m *home) layoutTextOverlay() {
	if m.textOverlay == nil {
		return
	}
	m.textOverlay.SetWidth(int(float32(m.termWidth) * 0.6))
	overlayHeight := m.termHeight - 2
	if overlayHeight < 6 {
		overlayHeight = m.termHeight
	}
	m.textOverlay.SetHeight(overlayHeight)
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(ui.CurrentTheme().Accent)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(ui.CurrentTheme().Info)
	keyStyle    = lipgloss.NewStyle().Bold(true).Foreground(ui.CurrentTheme().Warning)
	descStyle   = lipgloss.NewStyle().Foreground(ui.CurrentTheme().Foreground)
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
	case helpTypeInstanceAttach:
		// A full-screen attach is about to start — immediately, or deferred
		// until this overlay is dismissed. Release the live termpane
		// attachment first: a second client on the same session would fight
		// over the session size, and our render client must never sit in an
		// interactive client's way (#598 class; #1089). The tick-driven sync
		// won't rebind while an overlay is open, and re-establishes the
		// attachment after the eventual detach. Interactive mode (if a stray
		// path ever got here in it) cannot survive its attachment.
		m.closeAllLiveTermPanes()
		m.enforceInteractiveInvariant()
	}

	flag := helpType.mask()

	// Check if this help screen has been seen before
	// Only show if we're showing the general help screen or the corresponding flag is not set
	// in the seen bitmask.
	m.replayHelpDismissKey = false
	m.textOverlayDismissPolicy = nil
	m.textOverlayScrollable = false
	if alwaysShow || (m.appState.GetHelpScreensSeen()&flag) == 0 {
		// Mark this help screen as seen and save state
		if err := m.appState.SetHelpScreensSeen(m.appState.GetHelpScreensSeen() | flag); err != nil {
			log.WarningLog.Printf("failed to save help screen state: %v", err)
		}

		if responsive, ok := helpType.(responsiveHelpText); ok {
			m.textOverlay = overlay.NewResponsiveTextOverlay(responsive.toContentWidth)
		} else {
			m.textOverlay = overlay.NewTextOverlay(helpType.toContent())
		}
		m.textOverlay.OnDismiss = onDismiss
		m.textOverlayDismissAnyKey = true
		if _, ok := helpType.(helpTypeGeneral); ok {
			m.textOverlayDismissAnyKey = false
			m.textOverlayScrollable = true
		}
		if _, ok := helpType.(helpTypeInstanceAttach); ok {
			m.textOverlayDismissAnyKey = false
			m.textOverlayDismissPolicy = attachHelpDismissPolicy
		}
		if _, ok := helpType.(helpTypeInteractive); ok {
			m.replayHelpDismissKey = true
		}
		m.layoutTextOverlay()
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
	if m.textOverlayScrollable && !isHelpDismissKey(msg) {
		// Effective bindings precede hardcoded aliases so an advertised rebind
		// such as up=pgdown keeps its configured meaning inside help.
		if isHelpLineUpKey(msg) {
			m.textOverlay.ScrollUp()
			return m, nil
		}
		if isHelpLineDownKey(msg) {
			m.textOverlay.ScrollDown()
			return m, nil
		}
		if isHelpPageUpBinding(msg) {
			m.textOverlay.PageUp()
			return m, nil
		}
		if isHelpPageDownBinding(msg) {
			m.textOverlay.PageDown()
			return m, nil
		}
		if isHelpJumpTopKey(msg) {
			m.textOverlay.ScrollToTop()
			return m, nil
		}
		if isHelpJumpBottomKey(msg) {
			m.textOverlay.ScrollToBottom()
			return m, nil
		}
		if isHelpPageUpAlias(msg) {
			m.textOverlay.PageUp()
			return m, nil
		}
		if isHelpPageDownAlias(msg) {
			m.textOverlay.PageDown()
			return m, nil
		}
	}

	runOnDismiss := true
	if m.textOverlayDismissPolicy != nil {
		dismiss, run := m.textOverlayDismissPolicy(msg)
		if !dismiss {
			return m, nil
		}
		runOnDismiss = run
	} else if !m.textOverlayDismissAnyKey && !isHelpDismissKey(msg) {
		return m, nil
	}

	var dismissCmd tea.Cmd
	var shouldClose bool
	if runOnDismiss {
		dismissCmd, shouldClose = m.textOverlay.HandleKeyPress(msg)
	} else {
		// The overlay was canceled (Esc/Ctrl+C on the attach help,
		// attachHelpDismissPolicy → runOnDismiss=false): its OnDismiss — the
		// attach flow — will NOT run. Clear the re-entrant-attach guard here so a
		// canceled attach can never leave attachTransitioning armed and turn every
		// later Enter into a no-op (#1530). Today the flag isn't set until
		// beginAttachTransition runs inside OnDismiss (which this path skips), so
		// this is defense-in-depth that keeps the now-load-bearing guard invariant
		// robust if arming ever moves earlier. Harmless for non-attach overlays,
		// whose guard is already clear.
		m.attachTransitioning = false
		m.textOverlay.Dismissed = true
		shouldClose = true
	}
	if shouldClose {
		replayDismissKey := m.replayHelpDismissKey
		m.replayHelpDismissKey = false
		m.textOverlayDismissAnyKey = false
		m.textOverlayScrollable = false
		m.textOverlayDismissPolicy = nil
		m.state = stateDefault
		// Menu.SetState rebuilds the options slice; call it synchronously
		// on the event-loop goroutine rather than from a tea.Cmd closure
		// that runs off-loop and races with home.View -> Menu.String.
		m.menu.SetState(ui.StateDefault)
		if replayDismissKey {
			dismissCmd = replayKeyAfterInteractiveHelpDismiss(dismissCmd, msg)
		}
		// dismissCmd forwards repaintAfterDetachMsg{} from the attach
		// callback (#579) so the post-detach repaint doesn't have to wait
		// for the next previewTickMsg cycle.
		return m, tea.Batch(tea.WindowSize(), dismissCmd)
	}

	return m, nil
}

func attachHelpDismissPolicy(msg tea.KeyMsg) (bool, bool) {
	switch msg.Type {
	case tea.KeyEnter:
		return true, true
	case tea.KeyEsc, tea.KeyCtrlC:
		return true, false
	default:
		return false, false
	}
}

func isHelpJumpTopKey(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyHome
}

func isHelpJumpBottomKey(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyEnd
}

func isHelpPageUpBinding(msg tea.KeyMsg) bool {
	return key.Matches(msg, keys.GlobalKeyBindings[keys.KeyShiftUp])
}

func isHelpPageDownBinding(msg tea.KeyMsg) bool {
	return key.Matches(msg, keys.GlobalKeyBindings[keys.KeyShiftDown])
}

func isHelpPageUpAlias(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyPgUp ||
		msg.Type == tea.KeyCtrlB ||
		msg.Type == tea.KeyShiftUp
}

func isHelpPageDownAlias(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyPgDown ||
		msg.Type == tea.KeyCtrlF ||
		msg.Type == tea.KeySpace ||
		msg.Type == tea.KeyShiftDown
}

func isHelpLineUpKey(msg tea.KeyMsg) bool {
	return key.Matches(msg, keys.GlobalKeyBindings[keys.KeyUp])
}

func isHelpLineDownKey(msg tea.KeyMsg) bool {
	return key.Matches(msg, keys.GlobalKeyBindings[keys.KeyDown])
}

func isHelpDismissKey(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyEsc ||
		msg.Type == tea.KeyCtrlC ||
		key.Matches(msg, keys.GlobalKeyBindings[keys.KeyHelp])
}

func replayKeyAfterInteractiveHelpDismiss(dismissCmd tea.Cmd, keyMsg tea.KeyMsg) tea.Cmd {
	if dismissCmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := dismissCmd()
		if enter, ok := msg.(enterInteractiveMsg); ok {
			enter.replayKey = keyMsg
			enter.replay = true
			return enter
		}
		return msg
	}
}
