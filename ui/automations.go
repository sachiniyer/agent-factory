package ui

import (
	"fmt"
	"strings"
	"time"

	cron "github.com/robfig/cron/v3"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

var automationsTitleStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(AccentColor)

var automationsTitleDimStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(activeTheme.ForegroundMuted)

var automationsEnabledStyle = lipgloss.NewStyle().
	Foreground(activeTheme.Info)

var automationsDisabledStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundMuted)

// automationsOverdueStyle paints the overdue marker and its detail text in the
// theme's warning color. It is a color and a static glyph and nothing else — no
// spinner, no blink (#1766): state reads from a glyph in this app.
var automationsOverdueStyle = lipgloss.NewStyle().
	Foreground(activeTheme.Warning)

// automationItemTitleStyle paints an automation's title in the SAME adaptive
// color the instances tree uses for instance titles (tree.InstanceTitleColor),
// so the automations rail and the instance list above it read as one stacked
// list rather than two differently-colored ones (#1126).
var automationItemTitleStyle = lipgloss.NewStyle().
	Foreground(tree.InstanceTitleColor)

// automationDetailStyle renders an expanded row's cron/watch/status detail
// line — the recede gray the tree uses for its branch/description lines, so the
// detail reads as secondary to the title it hangs under (#1126).
var automationDetailStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundMuted)

var automationsHintStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundDim)

// AutomationsPane is the bottom section of the left rail (#1087 revised RFC
// §2.1's bottom strip): one row per task — the enabled glyph and the task
// title in the instances-list title color — pinned under the instances tree
// behind a horizontal rule. Rows are collapsed by default (title only, #1126
// dropped the always-on trailing cron/next/last text); the focused cursor's
// row expands to reveal its trigger and next/last-run detail on a dim indented
// line beneath the title. The full TaskPane manager (list + edit/create form)
// is NOT hosted in the rail: it opens as a centered modal overlay (like the
// hooks editor), so its form is never clamped into the narrow rail. The pane
// OWNS the TaskPane the overlay hosts; the rows render straight off the store
// projection so the section and the manager can never show different task sets
// after a reload.
//
// It implements layout.Pane. While focused the rows carry a cursor
// (up/down/j/k via HandleKey); the root opens the manager overlay on Enter or
// the global task-manager key and moves the focus ring on Esc.
type AutomationsPane struct {
	proj     *store.Projection
	taskPane *TaskPane

	rect    layout.Rect
	compact bool
	focused bool

	// selected is the focused section's cursor over the projection's task
	// list (clamped on every read); offset is the scroll window start so the
	// cursor stays visible in the few in-rail rows.
	selected int
	offset   int

	// now returns the current time for next-run derivation; a fixed value in
	// tests so rendered "next" columns are deterministic.
	now func() time.Time

	// zones is the shared mouse hit-test registry (#1024 R4); String()
	// registers the section plus its task rows every frame. Nil skips.
	zones *zones.Registry
}

// NewAutomationsPane creates the section over the given projection.
func NewAutomationsPane(proj *store.Projection) *AutomationsPane {
	return &AutomationsPane{
		proj:     proj,
		taskPane: NewTaskPane(),
		now:      time.Now,
	}
}

// TaskPane returns the task manager this section owns (hosted by the root's
// tasks overlay).
func (a *AutomationsPane) TaskPane() *TaskPane {
	return a.taskPane
}

// SetRect implements layout.Pane.
func (a *AutomationsPane) SetRect(r layout.Rect) {
	a.rect = r
}

// SetCompact selects the 1-line summary rendering (degradation ladder <80
// cols / <20 rows, RFC §2.6).
func (a *AutomationsPane) SetCompact(compact bool) {
	a.compact = compact
}

// Focused implements layout.Pane.
func (a *AutomationsPane) Focused() bool { return a.focused }

// Focus implements layout.Pane: the section shows a cursor over its compact
// rows. The task manager itself opens as an overlay (root-driven), never
// in-rail.
func (a *AutomationsPane) Focus() {
	a.focused = true
}

// Blur implements layout.Pane.
func (a *AutomationsPane) Blur() {
	a.focused = false
}

// SelectedTaskIndex returns the cursor's task index (clamped; -1 when there
// are no tasks) — the task the manager overlay preselects on open.
func (a *AutomationsPane) SelectedTaskIndex() int {
	n := len(a.proj.GetTasks())
	if n == 0 {
		return -1
	}
	return clampInt(a.selected, 0, n-1)
}

// HandleKey implements layout.Pane: the focused section owns only its cursor
// movement; everything else (Enter → manager overlay, Esc → focus ring) is
// root-routed so it stays in one place.
func (a *AutomationsPane) HandleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if !a.focused {
		return nil, false
	}
	switch msg.String() {
	case "up", "k":
		a.ScrollUp()
		return nil, true
	case "down", "j":
		a.ScrollDown()
		return nil, true
	}
	return nil, false
}

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 R4).
func (a *AutomationsPane) SetZoneRegistry(reg *zones.Registry) {
	a.zones = reg
}

// SelectTaskByID moves the section cursor onto the task with the given id —
// the click action for a task row. Reports whether the task was found.
func (a *AutomationsPane) SelectTaskByID(id string) bool {
	for i, tsk := range a.proj.GetTasks() {
		if tsk.ID == id {
			a.selected = i
			return true
		}
	}
	return false
}

// ScrollUp moves the section cursor up (wheel/key routing).
func (a *AutomationsPane) ScrollUp() {
	if a.selected > 0 {
		a.selected--
	}
}

// ScrollDown moves the section cursor down.
func (a *AutomationsPane) ScrollDown() {
	if a.selected < len(a.proj.GetTasks())-1 {
		a.selected++
	}
}

// nextRunSummary derives the "next/last" column of a compact row: a cron
// task's next fire time (from its schedule), a watch task's supervision
// state, plus the last run when one is recorded.
//
// A cron can be syntactically legal and still match no date at all —
// "0 0 31 2 *" is February 31st. ParseCron succeeds on it, but Next gives up
// after five years and returns the ZERO time.Time, which formats as a
// thoroughly plausible "next Jan 01 00:00"; name the absence instead of
// promising a fire time the task will never reach. The schedule picker's
// Custom preview guards the same trap (see renderPreviewLine).
func (a *AutomationsPane) nextRunSummary(tsk task.Task) string {
	var parts []string
	if tsk.IsWatch() {
		parts = append(parts, watchTaskStatus(tsk))
	} else if tsk.Enabled && tsk.CronExpr != "" {
		switch {
		case tsk.NextRunAt != nil:
			// The LIVE armed entry when the record carries one (#3623): a number
			// read off the scheduler cannot promise a fire the scheduler is not
			// holding.
			parts = append(parts, "next "+tsk.NextRunAt.Format("Jan 02 15:04"))
		case tsk.Arming == task.ArmingNotArmed:
			parts = append(parts, "not armed")
		default:
			// The rail reads tasks.json directly rather than through the daemon, so
			// it usually has no live entry to read and falls back to evaluating the
			// expression. That fallback is what used to render a confident "next"
			// for a task that had been dark for 18 days; the warning fragment below
			// is what stops it being read as health.
			sched, err := task.ParseCron(tsk.CronExpr)
			switch next := nextFireOrZero(sched, err, a.now()); {
			case err != nil:
				// The one unschedulable shape with no other explanation on this
				// line. "No upcoming run" below is emitted only after a SUCCESSFUL
				// parse, so without this a hand-edited or legacy row showed its raw
				// expression and a [!] with nothing saying why (#3623 review).
				parts = append(parts, "Invalid cron expression")
			case next.IsZero():
				parts = append(parts, "No upcoming run")
			default:
				parts = append(parts, "next "+next.Format("Jan 02 15:04"))
			}
		}
	}
	if tsk.LastRunAt != nil {
		parts = append(parts, "last "+tsk.LastRunAt.Format("Jan 02 15:04"))
	}
	// An errored task says so even with no LastRunAt. A task refused at arming
	// has never run, so gating this on a timestamp would hide the one thing it
	// has to report — and an unarmed task is exactly the one that looks healthy
	// otherwise (#2929).
	//
	// Not for a watch task: the watch branch above already reported it through
	// watchTaskStatus, which reads the same "errored:" prefix, and appending here
	// too renders "watch: … · errored · errored".
	if !tsk.IsWatch() && strings.HasPrefix(tsk.LastRunStatus, "errored:") {
		parts = append(parts, "errored")
	}
	return strings.Join(parts, " · ")
}

// itemPrefixWidth is the fixed lead of a collapsed row — marker (1) + the
// enabled glyph "[✓]" (3) + a 2-cell gap — that the title starts after and the
// expanded detail line indents to, so a row's detail aligns under its title.
const itemPrefixWidth = 6

// titleRow renders one collapsed automation row: the enabled/disabled glyph and
// the task title in the instance-title color — and nothing else (#1126: no
// always-on trailing cron/next/last text). The focused, expanded row is marked
// "▾" and its title bolded; every other row leads with a blank marker.
func (a *AutomationsPane) titleRow(tsk task.Task, expanded bool) string {
	glyph, glyphStyle := "[✓]", automationsEnabledStyle
	if !tsk.Enabled {
		glyph, glyphStyle = "[✗]", automationsDisabledStyle
	}
	// An overdue task takes over the enabled glyph rather than adding a column
	// (#3623). It can only ever BE enabled — the derivation says nothing about a
	// disabled task — so the two can never collide, and replacing the mark keeps
	// the row exactly as wide as it was at the 22-column rail minimum. Collapsed
	// rows are the point: a warning you have to focus a row to see is one nobody
	// sees.
	if needsAttention(tsk) {
		glyph, glyphStyle = "[!]", automationsOverdueStyle
	}
	marker := " "
	nameStyle := automationItemTitleStyle
	if expanded {
		marker = "▾"
		nameStyle = nameStyle.Bold(true)
	}
	name := tsk.Name
	if name == "" {
		name = "(unnamed)"
	}
	w := a.rect.W
	if w <= itemPrefixWidth {
		// Too narrow to split the styled segments cleanly: fall back to one
		// fitted plain line so the row never overflows the rail.
		return automationItemTitleStyle.Render(fitLine(marker+glyph+"  "+name, w))
	}
	return marker + glyphStyle.Render(glyph) + "  " +
		nameStyle.Render(fitLine(name, w-itemPrefixWidth))
}

// rowDetail is the text an expanded row reveals: the trigger (cron expression
// or watch command) and the next/last-run or supervision summary — the details
// that used to trail every collapsed row (#1126). Empty when a task has neither.
func (a *AutomationsPane) rowDetail(tsk task.Task) string {
	var parts []string
	// An overdue task leads with the problem, ahead of its own configuration
	// (#3623). The rail is narrow and this line is ellipsized to fit, so whatever
	// sits last is what gets cut: at the 22-column minimum the cron expression
	// would survive and the warning would not, which is backwards.
	if fragment := attentionFragment(tsk); fragment != "" {
		parts = append(parts, fragment)
	}
	trigger := tsk.CronExpr
	if tsk.IsWatch() {
		trigger = "watch: " + tsk.WatchCmd
	}
	if trigger != "" {
		parts = append(parts, trigger)
	}
	if next := a.nextRunSummary(tsk); next != "" {
		parts = append(parts, next)
	}
	return strings.Join(parts, " · ")
}

// nextFireOrZero evaluates a parsed schedule once, so the branch that decides
// what to say and the branch that formats it cannot disagree about the answer.
func nextFireOrZero(sched cron.Schedule, err error, now time.Time) time.Time {
	if err != nil {
		return time.Time{}
	}
	return sched.Next(now)
}

// needsAttention reports whether the row carries a warning: the task has stopped
// firing on its schedule, or its expression can never fire at all. Both are read
// off the record, so the rail's disk-backed poll sees them without a daemon.
func needsAttention(tsk task.Task) bool {
	return tsk.Overdue || tsk.Unschedulable
}

// attentionFragment is the detail line's warning text, or "" when the row is
// healthy. The missed count is omitted rather than printed as zero when the walk
// found none — an uncounted "missed 0" beside "overdue" reads as a
// contradiction — and a count that hit the derivation's cap is marked with a
// trailing "+" so a floor never renders as an exact number.
func attentionFragment(tsk task.Task) string {
	// Deliberately silent for an unschedulable task: the next/last summary already
	// diagnoses it — "No upcoming run" for an expression that parses (#2596),
	// "Invalid cron expression" for one that does not — and a second fragment
	// saying the same thing would both repeat itself and push the line past the
	// 36-column rail, clipping the half that names the expression. The glyph is
	// what that case was missing — a collapsed row read as healthy — not more
	// words.
	if !tsk.Overdue {
		return ""
	}
	if tsk.MissedOccurrences <= 0 {
		return "overdue"
	}
	if tsk.MissedOccurrencesCapped {
		return fmt.Sprintf("overdue · missed %d+", tsk.MissedOccurrences)
	}
	return fmt.Sprintf("overdue · missed %d", tsk.MissedOccurrences)
}

// detailRow renders the expanded row's detail as a dim line indented under the
// title, ellipsized to the rail width. Returns "" when the task has no detail.
func (a *AutomationsPane) detailRow(tsk task.Task) string {
	detail := a.rowDetail(tsk)
	if detail == "" {
		return ""
	}
	indent := strings.Repeat(" ", itemPrefixWidth)
	return automationDetailStyle.Render(fitLine(indent+detail, a.rect.W))
}

// overdueCount returns how many of the projection's tasks have missed their
// schedule. Only enabled cron tasks can be overdue (see task.DeriveScheduleHealth).
func (a *AutomationsPane) overdueCount() int {
	n := 0
	for _, tsk := range a.proj.GetTasks() {
		if needsAttention(tsk) {
			n++
		}
	}
	return n
}

// enabledCount returns how many of the projection's tasks are enabled.
func (a *AutomationsPane) enabledCount() int {
	n := 0
	for _, tsk := range a.proj.GetTasks() {
		if tsk.Enabled {
			n++
		}
	}
	return n
}

func automationHelpKey(name keys.KeyName) string {
	return keys.GlobalKeyBindings[name].Help().Key
}

func automationsActionHint(name keys.KeyName, desc string) string {
	return automationHelpKey(name) + " " + desc
}

// automationsHeader is the section header's text, split at the seam the width
// ladder needs: the noun is decoration and may be shortened or dropped, the
// counts are the only information the header carries and never may be (#3630).
type automationsHeader struct {
	noun   string // "Automations" — or "Automations:" in the compact summary
	counts string // "(2)", or "2 (1 on)" in the compact summary
	// primary is counts reduced to the one number that must survive, and it is a
	// rung of its own before the affordance is touched. The compact summary
	// carries two numbers ("100 (100 on)" is 12 cells), which at the 22-column
	// rail minimum left nothing for the hint and truncated it — regressing the
	// contract that the manage affordance is cut last, at a SUPPORTED width
	// rather than below one (#3641 review). Empty means counts is already
	// minimal, as it is in full mode.
	primary string
}

// text is the header at full width: " Automations (2)".
func (h automationsHeader) text() string { return " " + h.noun + " " + h.counts }

// countsOnly sheds the noun but keeps the numbers: " (2)".
func (h automationsHeader) countsOnly() string { return " " + h.counts }

// primaryOnly sheds the secondary number too: " 100" from " 100 (100 on)". It
// is the last rung that still says anything true before clipping.
func (h automationsHeader) primaryOnly() string {
	if h.primary == "" {
		return ""
	}
	return " " + h.primary
}

// shrunk ellipsizes the NOUN inside w cells while keeping the counts whole, or
// returns "" when there is no room for a noun worth rendering.
func (h automationsHeader) shrunk(w int) string {
	room := w - runewidth.StringWidth(h.countsOnly()) - 1 // 1 for the leading pad
	// Below three cells a "noun" is an ellipsis and a letter or two; drop it and
	// let countsOnly have the width instead.
	if room < 3 {
		return ""
	}
	return " " + fitLine(h.noun, room) + " " + h.counts
}

// automationsHintSeparator is the repo's fragment separator (CLAUDE.md), and it
// belongs to the HINT rather than to the title's trailing space. That is the
// whole of #3630: while the leading space lived on the end of the title, every
// shrink of the title ate it, and the header rendered "Automation…· m manage" —
// an ellipsis welded to a separator, reading as one mangled token.
const automationsHintSeparator = " · "

// titleLine renders the section header width-aware. Segments shed in order of
// what they cost the reader:
//
//	Automations (2) · m manage · e hooks    full
//	Automations (2) · m manage              hooks drops first
//	Automatio… (2) · m manage               then the noun shrinks, counts intact
//	(2) · m manage                          then the noun goes, counts intact
//	100 · m manage                          then the secondary count goes
//
// Three rules hold at every step, and #3630 was the first two failing at once.
//
// The " · " separator is never ellipsized into. Its leading space used to live
// on the END of the title, so every shrink of the title ate it and the header
// rendered "Automation…· m manage" — an ellipsis welded to a separator, reading
// as one mangled token rather than a truncated word beside a hint. The
// separator now belongs to the hint, where it cannot be shortened away.
//
// The counts survive every form. The old fallback replaced the whole title with
// an ellipsized constant, so below 110 columns a section with two tasks was
// byte-identical to one with none — the header stopped carrying the only
// information it has.
//
// And the manage affordance is the last thing cut, which is the shipped contract
// (TestAutomationsTitleWidthAware: "22-col rail still shows the manage
// affordance", "the shrunk name marks its cut with an ellipsis"): the key to the
// manager stays reachable at the 22-column rail minimum (#1090 width).
func (a *AutomationsPane) titleLine(header automationsHeader, nameStyle lipgloss.Style) string {
	w := a.rect.W
	manage := automationsHintSeparator + automationsActionHint(keys.KeyTaskList, "manage")
	hooks := automationsHintSeparator + automationsActionHint(keys.KeyHooks, "hooks")

	render := func(title, hint string) string {
		return nameStyle.Render(title) + automationsHintStyle.Render(hint)
	}
	fits := func(title, hint string) bool {
		return runewidth.StringWidth(title+hint) <= w
	}

	full := header.text()
	if fits(full, manage+hooks) {
		return render(full, manage+hooks)
	}
	if fits(full, manage) {
		return render(full, manage)
	}
	if shrunk := header.shrunk(w - runewidth.StringWidth(manage)); shrunk != "" && fits(shrunk, manage) {
		return render(shrunk, manage)
	}
	counts := header.countsOnly()
	if fits(counts, manage) {
		return render(counts, manage)
	}
	if primary := header.primaryOnly(); primary != "" && fits(primary, manage) {
		return render(primary, manage)
	}
	// Narrower than the rail minimum: nothing composes, so clip the whole line
	// rather than pretend one of the pieces still fits.
	return nameStyle.Render(fitLine(counts+manage, w))
}

// View implements layout.Pane: exactly rect-sized.
func (a *AutomationsPane) View() string { return a.String() }

func (a *AutomationsPane) String() string {
	if a.rect.Empty() {
		return ""
	}
	// The section's base zone: any click inside it that lands on no task row
	// focuses the automations region. Task rows register on top (later wins).
	if a.zones != nil {
		a.zones.Register(zones.AutoBG, a.rect)
	}

	tasks := a.proj.GetTasks()
	if a.selected >= len(tasks) {
		a.selected = len(tasks) - 1
	}
	if a.selected < 0 {
		a.selected = 0
	}

	// 1-line degraded summary (RFC §2.6, <80 cols).
	if a.compact || a.rect.H <= 1 {
		header := automationsHeader{
			noun:    "Automations:",
			counts:  fmt.Sprintf("%d (%d on)", len(tasks), a.enabledCount()),
			primary: fmt.Sprintf("%d", len(tasks)),
		}
		if overdue := a.overdueCount(); overdue > 0 {
			// The degraded one-line mode has no rows, so this line IS the section —
			// and it is the narrowest thing the rail draws. A task that has stopped
			// firing is easiest to miss exactly here, so the count rides the header
			// (#3623).
			//
			// It becomes the PRIMARY as well as riding the counts, which is what
			// #3641's ladder is for: when the width sheds everything but one number,
			// the number that must survive is the one saying something is wrong, not
			// the total.
			//
			// The primary carries the ROW GLYPH rather than the word, and that is a
			// width decision rather than a style one. #3641's last rung exists to
			// keep the manage affordance from being clipped at the 22-column rail
			// minimum, and " 100 overdue" needs 12 of the 11 cells that rung has —
			// measured, it fell straight through to the clip and truncated the
			// affordance, which is the contract that rung was added to protect. A
			// spelling picked by digit count would be no better: rebinding the
			// manager key changes the budget. "[!] 100" fits at any count, and it is
			// the mark the rows above carry at wider widths.
			header.counts = fmt.Sprintf("%d (%d on · %d overdue)", len(tasks), a.enabledCount(), overdue)
			header.primary = fmt.Sprintf("[!] %d", overdue)
		}
		style := automationsTitleDimStyle
		if a.focused {
			style = automationsTitleStyle
		}
		return layout.ClampToRect(a.titleLine(header, style), a.rect)
	}

	nameStyle := automationsTitleDimStyle
	if a.focused {
		nameStyle = automationsTitleStyle
	}
	title := a.titleLine(automationsHeader{
		noun:   "Automations",
		counts: fmt.Sprintf("(%d)", len(tasks)),
	}, nameStyle)
	lines := []string{title}
	if len(tasks) == 0 {
		lines = append(lines, automationsDisabledStyle.Render(
			fitLine(fmt.Sprintf("  No tasks — press %s, then n to create one", automationHelpKey(keys.KeyTaskList)), a.rect.W)))
	}

	// Reserve the last rail row as a blank bottom margin so the workspace
	// frame's bottom border never abuts the section's last row (#1560), the way
	// the sidebar's leading blank row keeps the frame's TOP border off the
	// rail. The grid sizes every full-mode section at least layout.AutomationsRows
	// tall (floor / grow-to-content / half-cap all include this margin), so
	// reserving one row costs no visible capacity in the app. Guard the
	// reservation on the section being at least that floor tall: a direct caller
	// that hands the pane a tighter, content-exact rect (below any size the grid
	// ever produces) keeps every content row rather than silently losing one —
	// the margin only matters where a workspace frame is actually drawn beside
	// the section, which is always at the grid's real (>= floor) sizes.
	contentH := a.rect.H
	if a.rect.H >= layout.AutomationsRows {
		contentH = a.rect.H - 1
	}

	// Window the rows around the cursor so a focused selection below the fold
	// scrolls into view instead of moving invisibly. The focused selection
	// expands to a 2-line row (title + detail), so reserve a line for the
	// detail when scrolling the selection to the bottom keeps it fully visible
	// rather than clipping its detail off the fold.
	visible := contentH - 1
	if visible < 0 {
		visible = 0
	}
	effVisible := visible
	if a.focused && effVisible > 1 {
		effVisible--
	}
	if a.offset > a.selected {
		a.offset = a.selected
	}
	if effVisible > 0 && a.selected >= a.offset+effVisible {
		a.offset = a.selected - effVisible + 1
	}
	if max := len(tasks) - visible; a.offset > max {
		a.offset = max
	}
	if a.offset < 0 {
		a.offset = 0
	}
	for i := a.offset; i < len(tasks); i++ {
		if len(lines) >= contentH {
			break
		}
		expanded := a.focused && i == a.selected
		rowStart := len(lines)
		lines = append(lines, a.titleRow(tasks[i], expanded))
		if expanded && len(lines) < contentH {
			if detail := a.detailRow(tasks[i]); detail != "" {
				lines = append(lines, detail)
			}
		}
		if a.zones != nil {
			a.zones.Register(zones.AutoTask(tasks[i].ID), layout.Rect{
				X: a.rect.X, Y: a.rect.Y + rowStart, W: a.rect.W, H: len(lines) - rowStart,
			})
		}
	}
	return layout.ClampToRect(strings.Join(lines, "\n"), a.rect)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
