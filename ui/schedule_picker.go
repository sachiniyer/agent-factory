package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/schedule"
	"github.com/sachiniyer/agent-factory/task"
)

// schedulePicker is the friendly, cron-free editor for a time-triggered task's
// schedule (#2057). It occupies the cron trigger's value focus stop in the task
// form: a schedule-type selector plus only the inputs that type needs, a live
// plain-English preview, and the generated cron shown read-only so users trust
// what gets saved. The underlying model and cron generation live in the
// canonical, UI-agnostic schedule package; this type is only the terminal
// input surface over it (the phase-2 web modal is the other surface).
//
// Navigation is self-contained so the outer form's tab model is untouched: the
// picker owns a single form stop and handles internal movement itself —
// up/down moves between the visible cells, left/right adjusts the focused cell
// (cycle the type, ±1 a number, flip AM/PM, walk the weekday row), space
// toggles (AM/PM, a weekday), and digits type into the numeric cells. Custom
// hands its raw-cron cell straight to a text input, restoring today's behavior.
type schedulePicker struct {
	typ           int    // index into scheduleTypes
	interval      string // shared by every-N-minutes / every-N-hours
	hourStr       string // 12-hour hour (1-12) for daily/weekly/monthly
	minuteStr     string // minute (0-59); also minute-of-hour for hourly
	domStr        string // day-of-month (1-31) for monthly
	meridiemPM    bool   // false=AM, true=PM
	weekdays      [7]bool
	weekdayCursor int
	raw           textinput.Model // custom cron escape hatch
	cursor        int             // index into cells() — the focused cell
	focused       bool
	width         int
}

// scheduleType pairs a schedule.Type with its selector label, in selector
// order. Custom sits last as the advanced escape hatch.
type scheduleTypeOption struct {
	kind  schedule.Type
	label string
}

var scheduleTypes = []scheduleTypeOption{
	{schedule.EveryNMinutes, "Every N minutes"},
	{schedule.EveryNHours, "Every N hours"},
	{schedule.Hourly, "Hourly"},
	{schedule.Daily, "Daily"},
	{schedule.Weekly, "Weekly"},
	{schedule.Monthly, "Monthly"},
	{schedule.Custom, "Custom (cron)"},
}

// Weekday toggles render Monday-first ("M T W T F S S", #2057) but map to the
// Sunday-first time.Weekday numbering the schedule package normalizes on.
var (
	weekdayDisplayOrder = [7]int{1, 2, 3, 4, 5, 6, 0} // display index → time.Weekday
	weekdayLetters      = [7]string{"M", "T", "W", "T", "F", "S", "S"}
)

// scheduleCell identifies one editable element within the picker. Which cells
// are present depends on the selected type (see cells).
type scheduleCell int

const (
	cellType scheduleCell = iota
	cellInterval
	cellHour
	cellMinute
	cellMeridiem
	cellWeekdays
	cellDayOfMonth
	cellRaw
)

func newSchedulePicker() *schedulePicker {
	raw := textinput.New()
	raw.Placeholder = "e.g. 0 9 * * 1-5"
	raw.PlaceholderStyle = taskPlaceholderStyle
	raw.CharLimit = 64
	raw.Blur()

	p := &schedulePicker{raw: raw}
	p.reset()
	return p
}

// reset seeds a sensible default for a brand-new task: daily at 9:00 AM. Every
// other type's fields also get valid defaults so switching types after this
// never lands on an empty/invalid input.
func (p *schedulePicker) reset() {
	p.setType(schedule.Daily)
	p.interval = "15"
	p.hourStr = "9"
	p.minuteStr = "00"
	p.domStr = "1"
	p.meridiemPM = false
	p.weekdays = [7]bool{true} // Monday
	p.weekdayCursor = 0
	p.raw.SetValue("")
	p.cursor = 0
}

// seed populates the picker from a Schedule parsed out of an existing task's
// cron. It starts from reset defaults so unrelated types keep valid values,
// then overlays the seeded type's fields. A custom schedule drops into the raw
// editor with the original expression.
func (p *schedulePicker) seed(sc schedule.Schedule) {
	p.reset()
	p.setType(sc.Type)
	switch sc.Type {
	case schedule.EveryNMinutes, schedule.EveryNHours:
		if sc.Interval > 0 {
			p.interval = strconv.Itoa(sc.Interval)
		}
	case schedule.Hourly:
		p.minuteStr = fmt.Sprintf("%02d", sc.Minute)
	case schedule.Daily:
		p.setClockFields(sc.Hour, sc.Minute)
	case schedule.Weekly:
		p.setClockFields(sc.Hour, sc.Minute)
		p.setWeekdays(sc.Weekdays)
	case schedule.Monthly:
		p.setClockFields(sc.Hour, sc.Minute)
		if sc.DayOfMonth > 0 {
			p.domStr = strconv.Itoa(sc.DayOfMonth)
		}
	case schedule.Custom:
		p.raw.SetValue(sc.Raw)
		p.raw.CursorEnd()
	}
	p.cursor = 0
	p.syncRawFocus()
}

func (p *schedulePicker) setClockFields(hour24, minute int) {
	h12, pm := to12Hour(hour24)
	p.hourStr = strconv.Itoa(h12)
	p.minuteStr = fmt.Sprintf("%02d", minute)
	p.meridiemPM = pm
}

// setWeekdays lights up the toggle row for the given weekdays, mapping the
// Sunday-first time.Weekday numbering onto the Monday-first display order.
func (p *schedulePicker) setWeekdays(days []time.Weekday) {
	p.weekdays = [7]bool{}
	for _, d := range days {
		n := int(d) % 7
		if n < 0 {
			n += 7
		}
		for i, wd := range weekdayDisplayOrder {
			if wd == n {
				p.weekdays[i] = true
			}
		}
	}
}

// setType points the selector at t (falling back to Custom for an unknown
// type) and keeps the cursor within the new cell set.
func (p *schedulePicker) setType(t schedule.Type) {
	p.typ = len(scheduleTypes) - 1 // Custom
	for i, opt := range scheduleTypes {
		if opt.kind == t {
			p.typ = i
			break
		}
	}
	p.clampCursor()
	p.syncRawFocus()
}

func (p *schedulePicker) kind() schedule.Type { return scheduleTypes[p.typ].kind }

// cells returns the focusable cells for the current type, in visual order.
func (p *schedulePicker) cells() []scheduleCell {
	switch p.kind() {
	case schedule.EveryNMinutes, schedule.EveryNHours:
		return []scheduleCell{cellType, cellInterval}
	case schedule.Hourly:
		return []scheduleCell{cellType, cellMinute}
	case schedule.Daily:
		return []scheduleCell{cellType, cellHour, cellMinute, cellMeridiem}
	case schedule.Weekly:
		return []scheduleCell{cellType, cellHour, cellMinute, cellMeridiem, cellWeekdays}
	case schedule.Monthly:
		return []scheduleCell{cellType, cellHour, cellMinute, cellMeridiem, cellDayOfMonth}
	default: // Custom
		return []scheduleCell{cellType, cellRaw}
	}
}

func (p *schedulePicker) clampCursor() {
	if n := len(p.cells()); p.cursor >= n {
		p.cursor = n - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

func (p *schedulePicker) activeCell() scheduleCell { return p.cells()[p.cursor] }

// setFocused toggles whether the picker is the active form stop; it only
// affects rendering (the active-cell highlight) and the raw input's cursor.
func (p *schedulePicker) setFocused(focused bool) {
	p.focused = focused
	p.syncRawFocus()
}

func (p *schedulePicker) setWidth(width int) {
	p.width = width
	rawWidth := width - 8
	if rawWidth < 1 {
		rawWidth = 1
	}
	p.raw.Width = rawWidth
}

// syncRawFocus keeps the raw text input's cursor visible only while it is the
// focused cell of a focused picker.
func (p *schedulePicker) syncRawFocus() {
	if p.focused && p.activeCell() == cellRaw {
		p.raw.Focus()
	} else {
		p.raw.Blur()
	}
}

// handleKey processes one key while the picker owns focus. Tab/enter/esc are
// intercepted by the outer form before reaching here, so those still move
// between form stops and submit as usual.
func (p *schedulePicker) handleKey(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyUp:
		p.moveCursor(-1)
		return
	case tea.KeyDown:
		p.moveCursor(1)
		return
	}

	// The raw cron cell is a plain text field: give it every remaining key
	// (including left/right for the text cursor) except the up/down handled
	// above.
	if p.activeCell() == cellRaw {
		p.raw, _ = p.raw.Update(msg)
		return
	}

	switch {
	case msg.Type == tea.KeyLeft:
		p.adjust(-1)
	case msg.Type == tea.KeyRight:
		p.adjust(1)
	case msg.Type == tea.KeySpace || msg.String() == " ":
		p.toggle()
	case msg.Type == tea.KeyBackspace:
		p.editNumeric(msg)
	case msg.Type == tea.KeyRunes:
		p.editNumeric(msg)
	}
}

func (p *schedulePicker) moveCursor(dir int) {
	n := len(p.cells())
	p.cursor = (p.cursor + dir + n) % n
	p.syncRawFocus()
}

// adjust applies left/right to the focused cell: cycle the type, step a number,
// flip AM/PM, or walk the weekday-row cursor.
func (p *schedulePicker) adjust(dir int) {
	switch p.activeCell() {
	case cellType:
		prev := p.toSchedule()
		n := len(scheduleTypes)
		p.typ = (p.typ + dir + n) % n
		p.clampCursor()
		p.normalizeInterval()
		// Switching into Custom prefills the raw field with the cron the
		// previous preset generated, so the escape hatch starts from a working
		// expression rather than blank.
		if p.kind() == schedule.Custom && strings.TrimSpace(p.raw.Value()) == "" {
			p.raw.SetValue(prev.Cron())
			p.raw.CursorEnd()
		}
		p.syncRawFocus()
	case cellInterval:
		p.interval = stepNumber(p.interval, dir, 1, p.intervalMax())
	case cellHour:
		p.hourStr = stepNumber(p.hourStr, dir, 1, 12)
	case cellMinute:
		p.minuteStr = stepNumber(p.minuteStr, dir, 0, 59)
	case cellDayOfMonth:
		p.domStr = stepNumber(p.domStr, dir, 1, 31)
	case cellMeridiem:
		p.meridiemPM = !p.meridiemPM
	case cellWeekdays:
		p.weekdayCursor = clampInt(p.weekdayCursor+dir, 0, 6)
	}
}

// toggle applies space to the focused cell: flip AM/PM, or toggle the weekday
// under the row cursor.
func (p *schedulePicker) toggle() {
	switch p.activeCell() {
	case cellMeridiem:
		p.meridiemPM = !p.meridiemPM
	case cellWeekdays:
		p.weekdays[p.weekdayCursor] = !p.weekdays[p.weekdayCursor]
	}
}

// editNumeric appends a typed digit to (or backspaces) the focused numeric
// cell. Non-digit runes are ignored; values are clamped to a valid range only
// when the schedule is materialized, so a mid-edit "7" on the way to "17" is
// never fought.
func (p *schedulePicker) editNumeric(msg tea.KeyMsg) {
	field, maxLen := p.numericField()
	if field == nil {
		return
	}
	cur := *field
	if msg.Type == tea.KeyBackspace {
		if r := []rune(cur); len(r) > 0 {
			cur = string(r[:len(r)-1])
		}
		*field = cur
		return
	}
	for _, r := range msg.Runes {
		if unicode.IsDigit(r) && len([]rune(cur)) < maxLen {
			cur += string(r)
		}
	}
	*field = cur
}

// numericField returns a pointer to the string backing the focused numeric
// cell and its max digit length, or (nil, 0) if the focused cell is not a
// numeric input.
func (p *schedulePicker) numericField() (*string, int) {
	switch p.activeCell() {
	case cellInterval:
		return &p.interval, 3
	case cellHour:
		return &p.hourStr, 2
	case cellMinute:
		return &p.minuteStr, 2
	case cellDayOfMonth:
		return &p.domStr, 2
	default:
		return nil, 0
	}
}

func (p *schedulePicker) intervalMax() int {
	if p.kind() == schedule.EveryNHours {
		return 23
	}
	return 59
}

// intervalValue is the one canonical projection of the shared interval field.
// Both save-time materialization and a type transition use it, so the editable
// chip cannot retain a value that the selected schedule type will save
// differently.
func (p *schedulePicker) intervalValue() int {
	if p.kind() == schedule.EveryNHours {
		return atoiClamp(p.interval, 1, p.intervalMax(), 1)
	}
	return atoiClamp(p.interval, 1, p.intervalMax(), 15)
}

func (p *schedulePicker) normalizeInterval() {
	switch p.kind() {
	case schedule.EveryNMinutes, schedule.EveryNHours:
		p.interval = strconv.Itoa(p.intervalValue())
	}
}

// toSchedule materializes the current picker state into a canonical Schedule,
// clamping numeric fields into valid ranges so the generated cron is always
// well-formed (custom raw text excepted — it is validated separately).
func (p *schedulePicker) toSchedule() schedule.Schedule {
	switch p.kind() {
	case schedule.EveryNMinutes:
		return schedule.Schedule{Type: schedule.EveryNMinutes, Interval: p.intervalValue()}
	case schedule.EveryNHours:
		return schedule.Schedule{Type: schedule.EveryNHours, Interval: p.intervalValue()}
	case schedule.Hourly:
		return schedule.Schedule{Type: schedule.Hourly, Minute: atoiClamp(p.minuteStr, 0, 59, 0)}
	case schedule.Daily:
		return schedule.Schedule{Type: schedule.Daily, Hour: p.hour24(), Minute: atoiClamp(p.minuteStr, 0, 59, 0)}
	case schedule.Weekly:
		return schedule.Schedule{Type: schedule.Weekly, Hour: p.hour24(), Minute: atoiClamp(p.minuteStr, 0, 59, 0), Weekdays: p.selectedWeekdays()}
	case schedule.Monthly:
		return schedule.Schedule{Type: schedule.Monthly, Hour: p.hour24(), Minute: atoiClamp(p.minuteStr, 0, 59, 0), DayOfMonth: atoiClamp(p.domStr, 1, 31, 1)}
	default: // Custom
		return schedule.Schedule{Type: schedule.Custom, Raw: strings.TrimSpace(p.raw.Value())}
	}
}

// Cron and Describe are the values the form saves and previews; both are always
// live off the current picker state.
func (p *schedulePicker) Cron() string     { return p.toSchedule().Cron() }
func (p *schedulePicker) Describe() string { return p.toSchedule().Describe() }

// validate returns a user-facing error message for an unsavable schedule, or ""
// when the schedule is valid. It enforces the picker-specific constraints
// (a weekly needs at least one day; custom needs a non-empty, valid cron) then
// confirms the generated expression passes the daemon's own validator — the
// same gate the form applied to the old raw-cron field.
func (p *schedulePicker) validate() string {
	switch p.kind() {
	case schedule.Custom:
		raw := strings.TrimSpace(p.raw.Value())
		if raw == "" {
			return "cron expression is required"
		}
		if err := task.ValidateCronExpr(raw); err != nil {
			return fmt.Sprintf("invalid cron: %v", err)
		}
	case schedule.Weekly:
		if len(p.selectedWeekdays()) == 0 {
			return "select at least one day of the week"
		}
	}
	if err := task.ValidateCronExpr(p.Cron()); err != nil {
		return fmt.Sprintf("invalid cron: %v", err)
	}
	return ""
}

func (p *schedulePicker) hour24() int {
	h := atoiClamp(p.hourStr, 1, 12, 12)
	switch {
	case p.meridiemPM && h == 12:
		return 12
	case p.meridiemPM:
		return h + 12
	case h == 12: // 12 AM = midnight
		return 0
	default:
		return h
	}
}

func (p *schedulePicker) selectedWeekdays() []time.Weekday {
	var days []time.Weekday
	for i, on := range p.weekdays {
		if on {
			days = append(days, time.Weekday(weekdayDisplayOrder[i]))
		}
	}
	return days
}

// render draws the picker block: the type selector, only the inputs the current
// type needs, the plain-English preview, and the generated cron shown
// read-only. Each line is clipped to the pane width; the returned block carries
// no trailing newline (the form adds it). Sub-lines are indented so the block
// reads as one unit under the "Schedule:" label even on a narrow terminal.
func (p *schedulePicker) render() string {
	t := CurrentTheme()
	labelStyle := lipgloss.NewStyle().Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	previewStyle := lipgloss.NewStyle().Foreground(t.Foreground)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Warning)
	dimSelectedStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)

	lines := []string{labelStyle.Render("Schedule:") + " " + p.renderTypeSelector(selectedStyle, dimSelectedStyle)}
	lines = append(lines, p.renderContextLines()...)
	// Preview (plain-English) and the generated cron share one read-only line to
	// keep the picker compact enough to fit an 80x24 pane; the preview leads so
	// it survives on a narrow terminal even if the trailing cron clips.
	lines = append(lines, indentSub+previewStyle.Render(p.Describe())+dimStyle.Render("  ·  "+p.Cron()))
	if p.focused {
		lines = append(lines, indentSub+dimStyle.Render(p.hint()))
	}

	for i := range lines {
		lines[i] = fitLine(lines[i], p.width)
	}
	return strings.Join(lines, "\n")
}

const indentSub = "  "

func (p *schedulePicker) renderTypeSelector(selected, dim lipgloss.Style) string {
	label := scheduleTypes[p.typ].label
	if p.focused && p.activeCell() == cellType {
		return selected.Render("◂ " + label + " ▸")
	}
	return dim.Render(label)
}

// renderContextLines renders the inputs specific to the selected type.
func (p *schedulePicker) renderContextLines() []string {
	t := CurrentTheme()
	plain := lipgloss.NewStyle().Foreground(t.ForegroundMuted)
	switch p.kind() {
	case schedule.EveryNMinutes:
		return []string{indentSub + plain.Render("Every") + p.chip(cellInterval, p.interval) + plain.Render("minutes")}
	case schedule.EveryNHours:
		return []string{indentSub + plain.Render("Every") + p.chip(cellInterval, p.interval) + plain.Render("hours")}
	case schedule.Hourly:
		return []string{indentSub + plain.Render("At minute") + p.chip(cellMinute, p.minuteStr)}
	case schedule.Daily:
		return []string{indentSub + p.renderTimeLine(plain)}
	case schedule.Weekly:
		return []string{
			indentSub + p.renderTimeLine(plain),
			indentSub + plain.Render("Days") + p.renderWeekdayRow(),
		}
	case schedule.Monthly:
		return []string{
			indentSub + p.renderTimeLine(plain),
			indentSub + plain.Render("Day") + p.chip(cellDayOfMonth, p.domStr),
		}
	default: // Custom
		return []string{indentSub + plain.Render("Cron ") + p.raw.View()}
	}
}

func (p *schedulePicker) renderTimeLine(plain lipgloss.Style) string {
	meridiem := "AM"
	if p.meridiemPM {
		meridiem = "PM"
	}
	return plain.Render("At") + p.chip(cellHour, p.hourStr) + plain.Render(":") +
		p.chip(cellMinute, p.minuteStr) + p.chip(cellMeridiem, meridiem)
}

func (p *schedulePicker) renderWeekdayRow() string {
	t := CurrentTheme()
	active := p.focused && p.activeCell() == cellWeekdays
	var b strings.Builder
	for i, letter := range weekdayLetters {
		style := lipgloss.NewStyle().Foreground(t.ForegroundMuted)
		if p.weekdays[i] {
			style = lipgloss.NewStyle().Foreground(t.Warning).Bold(true)
		}
		if active && i == p.weekdayCursor {
			style = style.Reverse(true)
		}
		b.WriteString(style.Render(" " + letter + " "))
	}
	return b.String()
}

// chip renders one value cell, highlighting it when it is the focused cell of a
// focused picker (matching the form's focused-button treatment).
func (p *schedulePicker) chip(cell scheduleCell, text string) string {
	t := CurrentTheme()
	if strings.TrimSpace(text) == "" {
		text = " "
	}
	style := lipgloss.NewStyle().Foreground(t.Foreground)
	if p.focused && p.activeCell() == cell {
		style = lipgloss.NewStyle().Bold(true).Background(t.Accent).Foreground(t.Background)
	}
	return style.Render(" " + text + " ")
}

// hint is the picker's one-line internal-navigation help, tailored to the
// focused cell. The form's own footer still shows tab/enter/esc.
func (p *schedulePicker) hint() string {
	switch p.activeCell() {
	case cellRaw:
		return "type cron • ↑/↓ fields • tab next"
	case cellWeekdays:
		return "←/→ day • space toggle • ↑/↓ fields"
	case cellType, cellMeridiem:
		return "←/→ change • ↑/↓ fields • tab next"
	default:
		return "type or ←/→ • ↑/↓ fields • tab next"
	}
}

// stepNumber parses cur (falling back to min when unset/invalid), moves it by
// dir, and wraps within [min,max] so the arrow keys cycle rather than dead-end.
func stepNumber(cur string, dir, min, max int) string {
	v, err := strconv.Atoi(strings.TrimSpace(cur))
	if err != nil {
		v = min
	}
	span := max - min + 1
	v = min + ((v-min+dir)%span+span)%span
	return strconv.Itoa(v)
}

// atoiClamp parses s and clamps it into [min,max], returning def when s is
// unset or non-numeric.
func atoiClamp(s string, min, max, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return clampInt(v, min, max)
}

// to12Hour converts a 24-hour hour into a 12-hour hour and an isPM flag.
func to12Hour(hour24 int) (int, bool) {
	switch {
	case hour24 == 0:
		return 12, false
	case hour24 == 12:
		return 12, true
	case hour24 > 12:
		return hour24 - 12, true
	default:
		return hour24, false
	}
}
