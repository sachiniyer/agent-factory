package tree

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

const readyIcon = "● "

// Theme is the subset of the TUI palette the tree renderer needs. It is
// supplied by ui.ApplyTheme so this subpackage does not import ui.
type Theme struct {
	Foreground          lipgloss.TerminalColor
	ForegroundStrong    lipgloss.TerminalColor
	ForegroundMuted     lipgloss.TerminalColor
	ForegroundDim       lipgloss.TerminalColor
	SelectionBackground lipgloss.TerminalColor
	SelectionForeground lipgloss.TerminalColor
	Success             lipgloss.TerminalColor
	Warning             lipgloss.TerminalColor
	Error               lipgloss.TerminalColor
}

// deadIcon is hollow (not the filled readyIcon) so a dead session differs from
// a healthy Ready one by shape as well as color — the distinction survives low
// contrast and color-blindness (#935).
const deadIcon = "○ "

// lostIcon marks a session whose tmux vanished with no kill on record (#1108)
// — recovery-eligible, unlike a corpse. Hollow like deadIcon (it cannot be
// attached right now, #935) but dotted + amber so "lost, coming back" reads
// differently from "dead" by shape as well as color.
const lostIcon = "◌ "

// archivedIcon marks an archived session (#1028): a filed-away box glyph, muted.
// Deliberately distinct in shape from the running/ready/lost dots so an archived
// row reads as "put away, restartable" rather than any live/vanished state.
const archivedIcon = "▧ "

// limitIcon marks a session blocked at a usage-limit wall (#1146): a filled
// diamond, distinct in shape from every dot/box glyph so "blocked on limit"
// never reads as a live Running/Ready session — the honest surface the whole
// feature exists for. Paired with the [limit] title prefix so it survives low
// contrast and color-blindness, the same discipline as the dead/lost dots.
// (Refines the provisional ◒ slot Phase 1e stubbed for #1204.)
const limitIcon = "◆ "

// expandedArrow/collapsedArrow mark an instance row whose tab children are
// shown/hidden; nonExpandableArrow keeps transient rows (never expandable, see
// Expandable) aligned with their siblings.
const (
	expandedArrow      = "▾"
	collapsedArrow     = "▸"
	nonExpandableArrow = " "
)

var readyStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#7F9F7F"))

// deadStyle paints the status dot of a session whose backing tmux/remote
// session has vanished (#935). A muted gray — the same recede treatment used
// for a deleting row — keeps a corpse from reading as a healthy green session.
var deadStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#989890"))

// lostStyle paints the status dot of a Lost session (#1108): amber, not the
// corpse gray — the session is expected to come back, but must not read as a
// healthy green either.
var lostStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#F0DFAF"))

// archivedStyle paints an archived session's dot + dims its title (#1028): the
// same muted gray as a deleting/dead recede, so a filed-away session never reads
// as live. Reused for the title/desc foreground below.
var archivedStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#989890"))

// limitStyle paints the status glyph of a usage-limit-blocked session (#1146): a
// warning red-orange, distinct from the ready-green, lost-amber, and dead/
// archived gray so the blocked state is unmistakable at a glance.
var limitStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#CC9393"))

// limitBadgePrefix returns the sidebar title prefix for a usage-limit-blocked
// session (#1146): "[limit] resets <t> " when a reset time is known, else a bare
// "[limit] ". Kept a helper (not inlined) so the tab pane / search overlay could
// reuse the exact same wording if they later surface it.
func limitBadgePrefix(i *session.Instance) string {
	resetAt, ok := i.LimitResetAt()
	if !ok {
		return "[limit] "
	}
	return fmt.Sprintf("[limit] resets %s ", formatLimitReset(resetAt, time.Now()))
}

// formatLimitReset renders a usage-limit reset time for the sidebar badge in the
// viewer's local zone: a bare hour like "3pm" on the hour, "3:04pm" otherwise,
// prefixed with the month/day ("Jul 6 3pm") when the reset is not today so a
// weekly-limit reset days out is unambiguous. now is passed in for testability.
func formatLimitReset(reset, now time.Time) string {
	reset = reset.Local()
	now = now.Local()
	clock := strings.ToLower(reset.Format("3:04pm"))
	if reset.Minute() == 0 {
		clock = strings.ToLower(reset.Format("3pm"))
	}
	if reset.Year() == now.Year() && reset.YearDay() == now.YearDay() {
		return clock
	}
	return reset.Format("Jan 2") + " " + clock
}

// InstanceTitleColor is the foreground of an unselected instance title — the
// adaptive near-black (light) / near-white (dark) that reads as primary text
// in the tree. It is exported as the single source of truth so surfaces
// stacked below the tree — the automations rail (#1126) — can paint their own
// titles in the exact same color and the two lists can never drift apart, the
// same single-definition discipline AccentColor uses for the accent.
var InstanceTitleColor lipgloss.TerminalColor = lipgloss.Color("#DCDCCC")

var titleStyle = lipgloss.NewStyle().
	Padding(1, 1, 0, 1).
	Foreground(InstanceTitleColor)

var listDescStyle = lipgloss.NewStyle().
	Padding(0, 1, 1, 1).
	Foreground(lipgloss.Color("#989890"))

var selectedTitleStyle = lipgloss.NewStyle().
	Padding(1, 1, 0, 1).
	Background(lipgloss.Color("#4F4F4F")).
	Foreground(lipgloss.Color("#FFFFEF"))

var selectedDescStyle = lipgloss.NewStyle().
	Padding(0, 1, 1, 1).
	Background(lipgloss.Color("#4F4F4F")).
	Foreground(lipgloss.Color("#FFFFEF"))

// tabRowStyle renders tab child rows in the same primary foreground as the
// Agent/active tab label (#1456). Selection still supplies the row highlight.
var tabRowStyle = lipgloss.NewStyle().
	Foreground(InstanceTitleColor)

// tabRowActiveStyle keeps tab rows in the same foreground; the tmux-style "*"
// marker carries the active cue.
var tabRowActiveStyle = lipgloss.NewStyle().
	Foreground(InstanceTitleColor)

// tabRowSelectedStyle highlights the tab row under the tree cursor with the
// same background the selected instance row uses.
var tabRowSelectedStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#4F4F4F")).
	Foreground(lipgloss.Color("#FFFFEF"))

// deletingTitleColor dims a mid-deletion row — title and branch line —
// to the description gray so it visually recedes while its teardown runs in
// the background (#844, #853).
var deletingTitleColor lipgloss.TerminalColor = lipgloss.Color("#989890")

// ApplyTheme rebuilds package-level tree styles after the TUI palette changes.
func ApplyTheme(t Theme) {
	readyStyle = lipgloss.NewStyle().Foreground(t.Success)
	deadStyle = lipgloss.NewStyle().Foreground(t.ForegroundMuted)
	lostStyle = lipgloss.NewStyle().Foreground(t.Warning)
	archivedStyle = lipgloss.NewStyle().Foreground(t.ForegroundMuted)
	limitStyle = lipgloss.NewStyle().Foreground(t.Error)

	InstanceTitleColor = t.Foreground
	titleStyle = lipgloss.NewStyle().
		Padding(1, 1, 0, 1).
		Foreground(InstanceTitleColor)
	listDescStyle = lipgloss.NewStyle().
		Padding(0, 1, 1, 1).
		Foreground(t.ForegroundMuted)
	selectedTitleStyle = lipgloss.NewStyle().
		Padding(1, 1, 0, 1).
		Background(t.SelectionBackground).
		Foreground(t.SelectionForeground)
	selectedDescStyle = lipgloss.NewStyle().
		Padding(0, 1, 1, 1).
		Background(t.SelectionBackground).
		Foreground(t.SelectionForeground)
	tabRowStyle = lipgloss.NewStyle().
		Foreground(InstanceTitleColor)
	tabRowActiveStyle = lipgloss.NewStyle().
		Foreground(InstanceTitleColor)
	tabRowSelectedStyle = lipgloss.NewStyle().
		Background(t.SelectionBackground).
		Foreground(t.SelectionForeground)
	deletingTitleColor = t.ForegroundMuted
}

// InstanceRenderer renders the tree's rows: session.Instance rows (absorbed
// from ui/list.go) and their tab child rows.
type InstanceRenderer struct {
	spinner *spinner.Model
	// width is the effective content width — the caller passes the sidebar's
	// usable column (its rect minus row padding), keeping the layout math in
	// one place outside this package.
	width int
}

// NewInstanceRenderer creates a renderer sharing the app-wide spinner.
func NewInstanceRenderer(spin *spinner.Model) *InstanceRenderer {
	return &InstanceRenderer{spinner: spin}
}

// SetWidth sets the effective content width rows render into.
func (r *InstanceRenderer) SetWidth(width int) {
	r.width = width
}

// ɹ and ɻ are other options.
const branchIcon = "Ꮧ"

// ArrowCell returns the (x, y) cell of the ▾/▸ expand/collapse arrow within
// an instance row block rendered at content width w, for mouse hit-testing
// (#1024 R4): block line 0 is the title style's top-padding line, so the
// arrow sits on line 1, one cell after the row's left padding + the prefix's
// leading space. ok is false at ultra-narrow widths, where Render drops the
// arrow from the prefix entirely (the #646 fallback) — the sidebar registers
// no arrow zone then. Kept next to Render so the prefix layout and the hit
// target can't drift apart; the render test pins them together against actual
// output.
func ArrowCell(w int) (x, y int, ok bool) {
	if w <= 9 {
		return 0, 0, false
	}
	return 2, 1, true
}

// instancePrefix renders the display-only tree prefix on instance rows. It
// intentionally does not include the instance's 1-based index (#1494).
func instancePrefix(arrow string, width int) string {
	if width <= 9 {
		return ""
	}
	return fmt.Sprintf(" %s ", arrow)
}

// Render renders an instance row. expanded selects the ▾/▸ tree arrow; a
// non-expandable instance (see Expandable) renders a blank arrow cell so its
// title stays aligned with its siblings.
func (r *InstanceRenderer) Render(i *session.Instance, _ int, selected bool, hasMultipleRepos bool, expanded bool) string {
	arrow := nonExpandableArrow
	if Expandable(i) {
		if expanded {
			arrow = expandedArrow
		} else {
			arrow = collapsedArrow
		}
	}
	prefix := instancePrefix(arrow, r.width)
	// The arrow is multibyte, so alignment math below must use the prefix's
	// CELL width, never len(prefix).
	prefixWidth := runewidth.StringWidth(prefix)
	titleS := selectedTitleStyle
	descS := selectedDescStyle
	if !selected {
		titleS = titleStyle
		descS = listDescStyle
	}

	// Status dot / spinner. Read the two axes directly (#1195): a row with any
	// in-flight op (create/kill/archive) keeps the spinner ("busy"); otherwise
	// the daemon-owned liveness picks the dot. The liveness switch is TOTAL — every
	// value is rendered explicitly, no silent default — so adding a Liveness value
	// (LimitReached landed this way, #1146) forces a deliberate choice here.
	liveness := i.GetLiveness()
	op := i.GetInFlightOp()
	var join string
	switch {
	case op != session.OpNone:
		join = fmt.Sprintf("%s ", r.spinner.View())
	default:
		switch liveness {
		case session.LiveRunning:
			join = fmt.Sprintf("%s ", r.spinner.View())
		case session.LiveReady:
			join = readyStyle.Render(readyIcon)
		case session.LiveDead:
			join = deadStyle.Render(deadIcon)
		case session.LiveLost:
			join = lostStyle.Render(lostIcon)
		case session.LiveArchived:
			join = archivedStyle.Render(archivedIcon)
		case session.LiveLimitReached:
			join = limitStyle.Render(limitIcon)
		case session.LivenessUnset:
			// Serialization sentinel, never a live in-memory value; render like
			// Running so a stray zero never blanks the dot.
			join = fmt.Sprintf("%s ", r.spinner.View())
		}
	}

	// Cut the title if it's too long
	titleText := i.Title
	if i.IsRemote() {
		titleText = "[remote] " + titleText
	}
	// A deleting row keeps spinning but is explicitly marked and dimmed so it
	// reads as "going away", not "busy working" (#844).
	// A lost row is explicitly marked so "tmux vanished under it, no kill on
	// record" (#1108) is readable without decoding the amber dot; the title
	// keeps full contrast — unlike deleting/dead treatments, the session is
	// expected back.
	if liveness == session.LiveLost {
		titleText = "[lost] " + titleText
	}
	if op == session.OpKilling || op == session.OpArchiving {
		titleText = "[deleting] " + titleText
		titleS = titleS.Foreground(deletingTitleColor)
		// Dim the branch line too: on a selected row descS is the
		// high-contrast selectedDescStyle, and leaving it bright makes the
		// secondary line stand out more than the dimmed title (#853).
		descS = descS.Foreground(deletingTitleColor)
	}
	// An archived row (#1028) is dimmed and carries the ▧ archived glyph so it
	// reads as "filed away, restartable" rather than a live session. It
	// deliberately carries NO "[archived] " text prefix: unlike the
	// transient [deleting]/[lost] states, the Archived list is a persistent list
	// the user browses BY NAME, and an 11-char word prefix eats the whole title
	// cell at ordinary sidebar widths (~13 cols), clipping every name to
	// "[archived]..." (#1225). The state is already conveyed three other ways on
	// the same row — the ▧ glyph, the dimming below, and the "▼ Archived (n)"
	// section header — so the name stays full-width like a live row's.
	if liveness == session.LiveArchived {
		titleS = titleS.Foreground(deletingTitleColor)
		descS = descS.Foreground(deletingTitleColor)
	}
	// A usage-limit-blocked row (#1146) is prefixed with a [limit] marker and its
	// reset time when known ("[limit] resets 3pm"), so the sidebar says WHY the
	// session is stalled and roughly when it frees up — retry now with c. The title
	// keeps full contrast (like [lost]): the session is blocked, not gone.
	if liveness == session.LiveLimitReached {
		titleText = limitBadgePrefix(i) + titleText
	}
	prefixSepWidth := 0
	if prefix != "" {
		prefixSepWidth = 1
	}
	widthAvail := r.width - 3 - prefixWidth - prefixSepWidth
	if widthAvail <= 0 {
		// No room for any title text at this width; render just the prefix.
		// lipgloss.Place doesn't clip oversize content, so leaving titleText
		// intact here would spill past sidebarW (#646).
		titleText = ""
	} else if runewidth.StringWidth(titleText) > widthAvail {
		// Drop the "..." tail when the container is too narrow to fit it,
		// otherwise runewidth.Truncate returns content wider than widthAvail
		// and lipgloss.Place won't clip the overflow.
		tail := "..."
		if widthAvail < runewidth.StringWidth(tail) {
			tail = ""
		}
		titleText = runewidth.Truncate(titleText, widthAvail, tail)
	}
	// At very narrow widths (sidebarW ≤ 11, r.width ≤ 9) the row would still
	// overflow sidebarW even with the bot's titleText="" fix above:
	// titleStyle.Padding(1,1,0,1) and descStyle's matching horizontal padding
	// each add 2 cells beyond r.width, exceeding the buffer the sidebar
	// carves out below sidebarW. JoinVertical then pads the
	// shorter title row up to the wider branchLine row, so the row spills past
	// the sidebar container. Drop horizontal padding on both styles at narrow
	// widths so the rendered row stays inside sidebarW (#646). Keep the top
	// padding line so the existing test's line indexing still works.
	if r.width <= 9 {
		titleS = titleS.PaddingLeft(0).PaddingRight(0)
		descS = descS.PaddingLeft(0).PaddingRight(0)
	}
	titleContent := titleText
	if prefix != "" {
		titleContent = fmt.Sprintf("%s %s", prefix, titleText)
	}
	title := titleS.Render(lipgloss.JoinHorizontal(
		lipgloss.Left,
		lipgloss.Place(r.width-3, 1, lipgloss.Left, lipgloss.Center, titleContent),
		" ",
		join,
	))

	remainingWidth := r.width
	remainingWidth -= prefixWidth
	remainingWidth -= runewidth.StringWidth(branchIcon)
	remainingWidth -= 2 // for the literal " " and "-" in the branchLine format string

	// Use the mutex-guarded accessor so this read (on the renderer
	// goroutine) doesn't race with LocalBackend.Start's write on the
	// instance-creation tea.Cmd goroutine.
	branch := i.GetBranch()
	if i.Started() && hasMultipleRepos {
		repoName, err := i.RepoName()
		if err != nil {
			log.ErrorLog.Printf("could not get repo name in instance renderer: %v", err)
		} else {
			branch += fmt.Sprintf(" (%s)", repoName)
		}
	}
	// Don't show branch if there's no space for it. Or show ellipsis if it's too long.
	branchWidth := runewidth.StringWidth(branch)
	if remainingWidth < 0 {
		branch = ""
	} else if remainingWidth < branchWidth {
		if remainingWidth < 3 {
			branch = ""
		} else {
			// We know the remainingWidth is at least 4 and branch is longer than that, so this is safe.
			branch = runewidth.Truncate(branch, remainingWidth-3, "...")
		}
	}
	remainingWidth -= runewidth.StringWidth(branch)

	// Add spaces to fill the remaining width.
	spaces := ""
	if remainingWidth > 0 {
		spaces = strings.Repeat(" ", remainingWidth)
	}

	branchLine := fmt.Sprintf("%s %s-%s%s", strings.Repeat(" ", prefixWidth), branchIcon, branch, spaces)

	// join title and subtitle
	text := lipgloss.JoinVertical(lipgloss.Left, title, descS.Render(branchLine))

	return text
}

// RenderTab renders one tab child row of an expanded instance: an indented
// ├/└ connector, the 1-based slot number (matching the 1-9 jump keys), the
// tab's label, and a tmux-style " *" marker on the tab the content pane is
// showing. selected highlights the row under the tree cursor.
func (r *InstanceRenderer) RenderTab(label string, oneBased int, isLast, selected, active bool) string {
	connector := "├"
	if isLast {
		connector = "└"
	}
	marker := ""
	if active {
		marker = " *"
	}
	text := fmt.Sprintf("%s%s %d %s%s",
		strings.Repeat(" ", runewidth.StringWidth(instancePrefix(expandedArrow, r.width))),
		connector, oneBased, label, marker)
	if r.width > 0 && runewidth.StringWidth(text) > r.width {
		// Same narrow-width handling as the instance rows: drop the "..." tail
		// when it would itself overflow, since lipgloss.Place won't clip
		// oversize content.
		tail := "..."
		if r.width < runewidth.StringWidth(tail) {
			tail = ""
		}
		text = runewidth.Truncate(text, r.width, tail)
	}
	style := tabRowStyle
	if active {
		style = tabRowActiveStyle
	}
	if selected {
		style = tabRowSelectedStyle
	}
	pad := 1
	if r.width <= 9 {
		// Match the instance rows' narrow-width padding drop (#646) so the row
		// stays inside the sidebar container.
		pad = 0
	}
	return style.Padding(0, pad).Render(
		lipgloss.Place(r.width, 1, lipgloss.Left, lipgloss.Center, text))
}
