package app

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
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

// helpKeyColumnFraction caps the key column as a share of the overlay's content
// width. The column is otherwise sized to the widest key across ALL sections, so
// a single unusually wide entry sets it for every row — and the help has exactly
// one: "tab / shift+tab / ctrl+r" at 24 cells, against ~30 rows of 1-6 cells.
//
// At 80x24 the overlay's content is 44 columns, so that one key took 26 of them
// (59%) and left 16 for every description. The screen a new user opens to learn
// the app became a column of 3-5-line ribbons with ~25 blank columns beside each
// of them: 8 page-downs to read, versus 1 at 200x50 (#3629).
//
// A key wider than the cap wraps inside its own column rather than widening
// everyone else's — it pays for its own length. The cap is inert at wide sizes:
// at 200x50 (contentWidth 116) 40% is 46 and the natural 26 is far below it, so
// the layout that already reads correctly there is untouched.
const helpKeyColumnFraction = 0.4

// helpKeyColumnWidth is the key column's width for a given content width: the
// widest key plus its 2-cell gutter, capped by helpKeyColumnFraction.
//
// When the cap bites, the column shrinks to the widest key that still FITS
// under it rather than to the cap itself. That is what keeps the cap from
// creating a second victim: at 80x24 the cap is 17, and snapping to the widest
// fitting key gives 15 — enough for "ctrl+u/ctrl+d", the widest ordinary key —
// so exactly one row wraps and it is the outlier that caused the problem.
func helpKeyColumnWidth(keyWidths []int, contentWidth int) int {
	widest := 0
	for _, w := range keyWidths {
		if w > widest {
			widest = w
		}
	}
	width := widest + 2
	if capped := int(float64(contentWidth) * helpKeyColumnFraction); width > capped {
		width = capped
		widestFitting := 0
		for _, w := range keyWidths {
			if fits := w + 2; fits <= capped && fits > widestFitting {
				widestFitting = fits
			}
		}
		// Zero means every key is over the cap; then the cap itself is the
		// column and they all wrap, which is the only fair answer.
		if widestFitting > 0 {
			width = widestFitting
		}
	}
	// Leave room for the "- " separator and at least one description cell.
	if room := contentWidth - 3; width > room {
		width = room
	}
	if width < 1 {
		width = 1
	}
	return width
}

// helpKeyColumnGutter is the blank space reserved on the right of the key
// column, so no key — wrapped or not — can touch the "- " that follows it. It
// collapses to zero only on a column too narrow to hold both.
func helpKeyColumnGutter(keyColumnWidth int) int {
	const gutter = 2
	if keyColumnWidth-gutter < 1 {
		return 0
	}
	return gutter
}

// renderHelpSections lays the sections out with a single key column sized to the
// widest effective key across ALL sections, capped at helpKeyColumnFraction of
// the content width. When contentWidth is known, the description is a separate
// block: lipgloss wraps it beneath its first word, never beneath the key (#2577).
//
// Keys are measured with layout.Cells, the ONE width answer (#3585/#3610). They
// are ASCII today, so nothing here renders differently — the point is that the
// invariant is literal rather than nearly true, and that a rebind to a glyph key
// cannot make this column disagree with the pane the overlay is drawn into
// (#3635).
func renderHelpSections(header string, sections []helpSection, contentWidth int) string {
	keyWidth := 0
	var keyWidths []int
	for _, s := range sections {
		for _, r := range s.rows {
			w := layout.Cells(r.key)
			keyWidths = append(keyWidths, w)
			if w > keyWidth {
				keyWidth = w
			}
		}
	}
	// One layout for the whole screen, computed once: every row shares the
	// column so the descriptions line up.
	keyColumnWidth, descWidth := 0, 0
	if contentWidth > 0 {
		keyColumnWidth = helpKeyColumnWidth(keyWidths, contentWidth)
		descWidth = contentWidth - keyColumnWidth - 2
		if descWidth < 1 {
			descWidth = 1
		}
	}

	var lines []string
	lines = append(lines, header, "")
	for _, s := range sections {
		lines = append(lines, headerStyle.Render(s.title))
		for _, r := range s.rows {
			if contentWidth <= 0 {
				pad := strings.Repeat(" ", keyWidth-layout.Cells(r.key)+2)
				lines = append(lines, keyStyle.Render(r.key)+pad+descStyle.Render("- "+r.desc))
				continue
			}

			// PaddingRight, not bare Width: the 2 cells are a GUTTER, and an
			// over-cap key must wrap inside its own text width rather than
			// spend the gutter on a wrapped line. Without it the row rendered
			// "tab / shift+tab- While naming…", the key colliding with its own
			// separator (#3629, same class as #3630's "Automation…·").
			keyBlock := lipgloss.NewStyle().
				Width(keyColumnWidth).
				PaddingRight(helpKeyColumnGutter(keyColumnWidth)).
				Render(keyStyle.Render(r.key))
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

// helpDismissBindings are the rebindable bindings that close a text overlay.
// isHelpDismissKey dispatches on this list and visibleHelpAliases hides any
// advertised paging/jump alias a rebind has turned into one of them, so the
// copy and the dispatch cannot disagree: with `[keys] quit = "pgdown"` the
// help must not print "pgdn" beside "Page:" when pgdn now closes the screen.
var helpDismissBindings = []keys.KeyName{
	keys.KeyHelp,
	keys.KeyEnter,
	keys.KeyQuit,
}

func visibleHelpAliases(aliases []helpAlias) string {
	// The dismiss keys, plus the scroll bindings an alias can be shadowed by.
	bindings := append([]keys.KeyName{
		keys.KeyUp,
		keys.KeyDown,
		keys.KeyShiftUp,
		keys.KeyShiftDown,
	}, helpDismissBindings...)

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
			{helpKey(keys.KeyJumpTab), "Select one of the first nine tabs by number (s opens it, enter attaches)"},
			{helpKey(keys.KeyJumpTabPrompt), "Jump to ANY tab by number or name — there is no tab limit (#3021)"},
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
	// surface a "not available for remote" error (see showNewTabPicker /
	// handleCloseTab in app/handle_actions.go) — so only advertise the tab keys
	// that actually work for the instance type. The tab jump works for both (#988);
	// tabs also live in the left-rail tree since the layout cutover (#1024 PR 4).
	//
	// Names the unbounded gesture beside the digits (#3021): the digits reach the
	// first nine tabs and nothing caps tab creation, so a help screen that mentions
	// only "1-9" describes a limit the product does not have.
	openPane, newTab, closeTab := helpKey(keys.KeyOpenPane), helpKey(keys.KeyNewTab), helpKey(keys.KeyCloseTab)
	jumpAny := helpKey(keys.KeyJumpTabPrompt)
	tabHelp := keyStyle.Render("1-9/"+jumpAny) + descStyle.Render(fmt.Sprintf(" - Select a tab: digits reach the first nine, %s reaches any by number or name (%s opens it; %s new tab, %s close; tabs live in the tree)", jumpAny, openPane, newTab, closeTab))
	if !h.instance.Capabilities().TabManagement {
		tabHelp = keyStyle.Render("1-9/"+jumpAny) + descStyle.Render(fmt.Sprintf(" - Select a tab: digits reach the first nine, %s reaches any by number or name (%s opens it; tabs live in the tree)", jumpAny, openPane))
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
	m.textOverlayPendingSeenMask = 0
	if alwaysShow || (m.appState.GetHelpScreensSeen()&flag) == 0 {
		// Record "seen" when the user DISMISSES the screen, not when it is
		// painted (#3628). A once-per-home screen that overflows 80x24 was
		// being burned on display, so a user who closed it before reaching the
		// bottom — which the old any-key dismiss made the default outcome —
		// could never get the rest back.
		m.textOverlayPendingSeenMask = flag

		if responsive, ok := helpType.(responsiveHelpText); ok {
			m.textOverlay = overlay.NewResponsiveTextOverlay(responsive.toContentWidth)
		} else {
			m.textOverlay = overlay.NewTextOverlay(helpType.toContent())
		}
		m.textOverlay.OnDismiss = onDismiss
		m.textOverlayDismissAnyKey = true
		switch helpType.(type) {
		case helpTypeGeneral:
			m.textOverlayDismissAnyKey = false
		case helpTypeInstanceStart:
			// This screen advertises its own close keys ("enter continue · esc
			// close"), so it is not a press-any-key gate — and it must behave the
			// same way whether or not it happens to overflow the terminal (#3628).
			m.textOverlayDismissAnyKey = false
		case helpTypeInstanceAttach:
			m.textOverlayDismissAnyKey = false
			m.textOverlayDismissPolicy = attachHelpDismissPolicy
		case helpTypeInteractive:
			// The one screen that stays a press-any-key gate: its dismiss
			// keystroke is deliberately the user's first pane input and gets
			// replayed into the pane (#1576/#2413). An overflowing overlay still
			// narrows that to the dismiss keys — see handleHelpState.
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
	// Scrolling is a property of the CONTENT, not of the caller: any overlay
	// that overflows paints "↓ more", so any overlay that overflows must honour
	// the keys that marker advertises (#3628).
	//
	// A key this overlay would treat as DISMISSAL never scrolls — and "would
	// treat as dismissal" has to be asked of the overlay's own policy, not of
	// the generic set. The attach overlay's policy accepts only enter/esc, so
	// gating on the generic set could swallow a key the policy then refuses,
	// leaving it dead in both branches (#3634 review).
	if m.textOverlay != nil && m.textOverlay.Scrollable() && !m.dismissesTextOverlay(msg) {
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
		m.recordHelpScreenSeen()
		replayDismissKey := m.replayHelpDismissKey
		m.replayHelpDismissKey = false
		m.textOverlayDismissAnyKey = false
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

// dismissesTextOverlay reports whether the open overlay would close on msg. It
// asks the overlay's own policy where it has one, so the scroll branch and the
// dismiss branch agree about every key and none can fall between them (#3634
// review). A press-any-key overlay is deliberately NOT treated as "dismisses
// everything" here: while its content overflows the scroll keys must scroll,
// and every other key still dismisses through the branch below — which is what
// keeps the first-run interactive help's ctrl+] alive at sizes where it
// overflows (#1576/#2413).
func (m *home) dismissesTextOverlay(msg tea.KeyMsg) bool {
	if m.textOverlayDismissPolicy != nil {
		dismiss, _ := m.textOverlayDismissPolicy(msg)
		return dismiss
	}
	return isHelpDismissKey(msg)
}

// recordHelpScreenSeen persists the one-shot mask of the overlay the user just
// dismissed. It is deliberately the LAST step of the screen's life rather than
// the first (#3628): a screen the user never finished reading has not been
// seen, and quitting with one open leaves it to be shown again.
func (m *home) recordHelpScreenSeen() {
	flag := m.textOverlayPendingSeenMask
	m.textOverlayPendingSeenMask = 0
	if flag == 0 {
		return
	}
	if err := m.appState.SetHelpScreensSeen(m.appState.GetHelpScreensSeen() | flag); err != nil {
		log.WarningLog.Printf("failed to save help screen state: %v", err)
	}
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

// isHelpDismissKey is the explicit close set for every text overlay that is not
// a press-any-key gate. enter and q join esc here because a scrolling overlay
// no longer closes on just anything (#3628), and the first-run screens advertise
// "enter continue · esc close" in their own copy. The rebindable half is read
// from helpDismissBindings — the same list visibleHelpAliases hides shadowed
// aliases from — off the generated table, so a [keys] rebind moves them (#1026).
func isHelpDismissKey(msg tea.KeyMsg) bool {
	if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
		return true
	}
	for _, name := range helpDismissBindings {
		if key.Matches(msg, keys.GlobalKeyBindings[name]) {
			return true
		}
	}
	return false
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
