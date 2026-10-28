package overlay

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

const (
	confirmationOverlayHorizontalPadding = 2
	confirmationOverlayVerticalPadding   = 1
)

// defaultConfirmKey is the confirm key an un-escalated confirmation uses. A
// dialog that keeps it (ordinary kill, delete-project, handoff, …) also accepts
// enter as an affirmative alias (#2405). A dialog that escalates to a distinct
// key (root #1238, unmerged #2022) does so precisely to require a deliberate,
// non-reflex keystroke, so enter is NOT aliased there.
const defaultConfirmKey = "y"

// ConfirmationOverlay represents a confirmation dialog overlay
type ConfirmationOverlay struct {
	// Whether the overlay has been dismissed
	Dismissed bool
	// Message to display in the overlay
	message string
	// detail is optional elaboration rendered below message. Setting it (via
	// SetDetail) opts this overlay into the critical-content guarantee (#1973):
	// message becomes the part the user MUST read to consent. Since #5171 the
	// whole body scrolls, so "must read" is honoured by reachability — the
	// confirm key still refuses only when the window cannot show a single
	// body row.
	detail string
	// scroll is the first body line currently rendered, when the wrapped body
	// is taller than its window. The confirm prompt never scrolls — it sits
	// pinned under the window — so paging can never hide the buttons.
	scroll int
	// Width of the overlay
	width int
	// Maximum outer dimensions available for rendering.
	maxWidth  int
	maxHeight int
	// Callback function to be called when the user confirms (presses 'y')
	OnConfirm func()
	// Callback function to be called when the user cancels (presses 'n' or 'esc')
	OnCancel func()
	// Custom confirm key (defaults to 'y')
	ConfirmKey string
	// Custom cancel key (defaults to 'n')
	CancelKey string
	// pending, when non-empty, is a static note saying the dialog is still
	// assembling what the user would consent to (#4848). The dialog is already
	// open, so the key visibly registered, but the confirm is withheld and the
	// hint line shows the note in place of the confirm key until SetPending("").
	pending string
}

// NewConfirmationOverlay creates a new confirmation dialog overlay with the given message
func NewConfirmationOverlay(message string) *ConfirmationOverlay {
	return &ConfirmationOverlay{
		Dismissed:  false,
		message:    message,
		width:      50, // Default width
		ConfirmKey: defaultConfirmKey,
		CancelKey:  "n",
	}
}

// HandleKeyPress processes a key press and updates the state
// Returns true if the overlay should be closed
func (c *ConfirmationOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	key := strings.ToLower(msg.String())
	// ESC and Ctrl+C must always cancel. The UI promises "esc to cancel", so
	// check the cancel branch first — if ConfirmKey is misconfigured to "esc"
	// or "ctrl+c", the dialog becomes cancel-only rather than silently
	// confirming a destructive action.
	switch key {
	case strings.ToLower(c.CancelKey), "esc", "ctrl+c":
		c.Dismissed = true
		if c.OnCancel != nil {
			c.OnCancel()
		}
		return true
	}

	// The named confirm key always confirms; enter is an affirmative alias only
	// for an un-escalated dialog (see enterConfirms). An escalated dialog (root
	// #1238, unmerged #2022) must not be dispatchable by the D+enter reflex — the
	// same reason it already rejects a reflexive 'y' (#2405).
	if key == strings.ToLower(c.ConfirmKey) || (key == "enter" && c.enterConfirms()) {
		// A pending dialog has not finished stating the consequences, so a confirm
		// now would consent to copy the user has not seen yet (#4848). Esc still
		// cancels; the confirm key starts working once the result lands.
		if c.pending != "" {
			return false
		}
		// A guarded overlay too small to show its consequences must not collect a
		// confirm (#1973). Refusing here — not merely rendering a warning — is
		// what makes the guarantee real: the render and the key agree, so a confirm
		// typed blind against an unreadable dialog does nothing. The dialog stays
		// open (esc still cancels) so the user can resize and read it.
		rect := c.textRect()
		if c.tooSmallToConfirm(rect.W, rect.H) {
			return false
		}
		c.Dismissed = true
		if c.OnConfirm != nil {
			c.OnConfirm()
		}
		return true
	}

	// Scroll keys page the body when it overflows its window. They sit AFTER
	// the confirm/cancel branches so a dialog that claims 'k' as its
	// deliberate key (root #1238, unmerged #2022) keeps 'k' meaning confirm —
	// the same priority esc already holds over a misconfigured ConfirmKey
	// above. On a dialog that did not claim them, j/k and the arrows move the
	// body window while the confirm prompt stays pinned (#5171).
	switch key {
	case "up", "k":
		c.scrollBy(-1)
	case "down", "j":
		c.scrollBy(1)
	case "pgup":
		c.scrollBy(-c.pageStep())
	case "pgdown":
		c.scrollBy(c.pageStep())
	case "ctrl+u":
		c.scrollBy(-c.halfStep())
	case "ctrl+d":
		c.scrollBy(c.halfStep())
	}

	// Ignore other keys in confirmation state
	return false
}

// enterConfirms reports whether enter acts as an alias for the confirm key. It
// does only while the confirm key is the un-escalated default: escalating to a
// distinct key is the signal that easy affirmatives (a reflexive 'y', enter)
// must not dispatch the action (#1238/#2022/#2405).
func (c *ConfirmationOverlay) enterConfirms() bool {
	return strings.EqualFold(strings.TrimSpace(c.ConfirmKey), defaultConfirmKey)
}

// frameStyle is the overlay's border+padding style, shared by every path that
// needs to know how much room the text actually gets.
func (c *ConfirmationOverlay) frameStyle() lipgloss.Style {
	return ui.DialogStyle()
}

// textRect resolves the text area the message will actually be rendered into.
// HandleKeyPress and Render MUST agree on this: if the key handler judged the
// fit differently from the renderer, the guarantee would be fiction — a dialog
// could refuse on screen while still accepting a 'y', or the reverse.
func (c *ConfirmationOverlay) textRect() layout.Rect {
	style := c.frameStyle()
	fit := fitOverlayContent(c.width, 0, c.maxWidth, c.maxHeight, style)
	if fit.W > 0 {
		style = style.Width(fit.W)
	}
	return overlayTextRect(fit, style)
}

// Render renders the confirmation overlay
func (c *ConfirmationOverlay) Render() string {
	style := c.frameStyle()

	fit := fitOverlayContent(c.width, 0, c.maxWidth, c.maxHeight, style)
	if fit.W > 0 {
		style = style.Width(fit.W)
	}
	textRect := overlayTextRect(fit, style)
	content := c.visibleContent(textRect.W, textRect.H)
	if fit.H > 0 && renderedLineCount(content) >= textRect.H {
		style = style.Height(fit.H)
	}

	// Apply the border style and return
	return ui.RenderDialog(style, content)
}

// SetWidth sets the width of the confirmation overlay
func (c *ConfirmationOverlay) SetWidth(width int) {
	c.width = width
}

// SetMaxSize sets the maximum outer size the rendered confirmation may occupy.
func (c *ConfirmationOverlay) SetMaxSize(width, height int) {
	c.maxWidth = width
	c.maxHeight = height
}

// SetConfirmKey sets the key used to confirm the action
func (c *ConfirmationOverlay) SetConfirmKey(key string) {
	c.ConfirmKey = key
}

// SetPending marks the dialog as still waiting on the result that completes its
// copy, with note as the static text the hint line shows meanwhile (#4848). The
// confirm key is refused until SetPending(""); cancel keeps working.
func (c *ConfirmationOverlay) SetPending(note string) {
	c.pending = note
}

// Pending reports the note set by SetPending, or "" once the dialog is complete.
func (c *ConfirmationOverlay) Pending() string {
	return c.pending
}

// SetDetail sets elaboration rendered below the message, and opts this overlay
// into the critical-content guarantee (#1973). Split the copy so the message
// carries the consequences the user is consenting to and the detail carries the
// explanation: every line then stays reachable by scrolling, or the overlay
// refuses to confirm at all. Use it for any confirm whose message would be a
// lie if its tail fell below the fold.
func (c *ConfirmationOverlay) SetDetail(detail string) {
	c.detail = detail
}

// guarded reports whether this overlay carries a critical/detail split, i.e.
// whether its message must be readable for a confirm to be legitimate.
func (c *ConfirmationOverlay) guarded() bool {
	return strings.TrimSpace(c.detail) != ""
}

// detailLines wraps the elaboration. The blank spacer that separates it from
// the message is part of the body — it scrolls like everything else.
func (c *ConfirmationOverlay) detailLines(width int) []string {
	if !c.guarded() {
		return nil
	}
	return wrapOverlayLines(c.detail, width)
}

// bodyLines is the complete scrollable payload: the message, styled as the
// destructive headline it is, plus the blank separator and elaboration a
// guarded overlay carries. Nothing here is ever dropped — an overflow means a
// window, not a clip (#5171).
func (c *ConfirmationOverlay) bodyLines(width int) []string {
	critical := wrapOverlayLines(c.message, width)
	for i := range critical {
		critical[i] = lipgloss.NewStyle().Foreground(ui.CurrentTheme().Dead).Render(critical[i])
	}
	if detail := c.detailLines(width); len(detail) > 0 {
		critical = append(append(critical, ""), detail...)
	}
	return critical
}

// bodyBudget splits height between the body and the confirm prompt, reserving a
// blank gap between them when there is room. Mirrors the historical math.
func bodyBudget(height, hintLines int) (budget, gap int) {
	gap = 1
	budget = height - hintLines - gap
	if budget < 1 {
		gap = 0
		budget = height - hintLines
	}
	return budget, gap
}

// tooSmallToConfirm reports whether a guarded overlay cannot render even one
// line of what it is about to do plus the confirm prompt. Such an overlay must
// refuse the action outright: a destructive confirm that cannot show any of
// its consequences has no business collecting a 'y' (#1973). One body row is
// enough to decline the refusal — the rest is reachable by scrolling (#5171).
// Unguarded overlays never refuse.
func (c *ConfirmationOverlay) tooSmallToConfirm(width, height int) bool {
	if !c.guarded() || height <= 0 || width <= 0 {
		return false
	}
	return len(c.fittedHint(width, height)) >= height
}

// fittedHint picks the full or compact confirm prompt for the available height.
func (c *ConfirmationOverlay) fittedHint(width, height int) []string {
	hint := wrapOverlayLines(c.instruction(false), width)
	if height <= 0 {
		return hint
	}
	if len(c.bodyLines(width))+1+len(hint) > height || len(hint) > 2 {
		return wrapOverlayLines(c.instruction(true), width)
	}
	return hint
}

// scrollContent resolves the body and the window it scrolls through, using the
// same text rect Render does — the key handler's page math and the renderer's
// window can never disagree about how much fits.
func (c *ConfirmationOverlay) scrollContent() (body []string, budget int) {
	rect := c.textRect()
	if rect.H <= 0 || rect.W <= 0 {
		return nil, 0
	}
	budget, _ = bodyBudget(rect.H, len(c.fittedHint(rect.W, rect.H)))
	if budget < 1 {
		return nil, 0
	}
	return c.bodyLines(rect.W), budget
}

// Scrollable reports whether the body is taller than its window — i.e. exactly
// whether Render will paint a scroll notice. Hosts gate wheel routing on this
// so the notice never advertises a scroll the overlay cannot take.
func (c *ConfirmationOverlay) Scrollable() bool {
	body, budget := c.scrollContent()
	return len(body) > budget
}

func (c *ConfirmationOverlay) ScrollUp()   { c.scrollBy(-1) }
func (c *ConfirmationOverlay) ScrollDown() { c.scrollBy(1) }

// scrollBy moves the body window, clamped to its real range so a resize that
// grew the window — or a key pressed against a fitting body — is a no-op
// rather than a stuck offset.
func (c *ConfirmationOverlay) scrollBy(delta int) {
	body, budget := c.scrollContent()
	max := len(body) - budget
	if max < 0 {
		max = 0
	}
	c.scroll += delta
	if c.scroll > max {
		c.scroll = max
	}
	if c.scroll < 0 {
		c.scroll = 0
	}
}

// pageStep is how far PgUp/PgDn move: one window, keeping a line of context.
func (c *ConfirmationOverlay) pageStep() int {
	_, budget := c.scrollContent()
	if step := budget - 1; step > 1 {
		return step
	}
	return 1
}

// halfStep is how far ctrl+u/ctrl+d move: half a window.
func (c *ConfirmationOverlay) halfStep() int {
	_, budget := c.scrollContent()
	if step := budget / 2; step > 1 {
		return step
	}
	return 1
}

func (c *ConfirmationOverlay) visibleContent(width, height int) string {
	body := c.bodyLines(width)

	if height <= 0 {
		// Unbounded: everything renders, spacer and all.
		return strings.Join(append(body, append([]string{""}, wrapOverlayLines(c.instruction(false), width)...)...), "\n")
	}

	hint := c.fittedHint(width, height)

	// A guarded overlay that cannot show a single line of what it destroys
	// refuses instead of rendering a prompt above content nobody can reach.
	if c.tooSmallToConfirm(width, height) {
		return c.refusalContent(width, height)
	}

	if !c.guarded() && len(hint) >= height {
		return strings.Join(hint[:height], "\n")
	}

	budget, gap := bodyBudget(height, len(hint))

	var lines []string
	if len(body) <= budget {
		// Fits: identical to the pre-scroll layout — whole body, blank gap,
		// prompt. Resetting the offset keeps a dialog resized LARGER from
		// reopening mid-scroll.
		c.scroll = 0
		lines = append(lines, body...)
		if gap > 0 && len(lines) > 0 {
			lines = append(lines, "")
		}
	} else {
		// The body pages through a window while the prompt stays pinned. The
		// scroll notice takes the blank gap's slot — a line that was doing
		// nothing — so announcing hidden lines costs zero body rows.
		if c.scroll > len(body)-budget {
			c.scroll = len(body) - budget
		}
		if c.scroll < 0 {
			c.scroll = 0
		}
		lines = append(lines, body[c.scroll:c.scroll+budget]...)
		if gap > 0 {
			lines = append(lines, c.scrollNotice(body, budget, width))
		}
	}
	lines = append(lines, hint...)
	return strings.Join(lines, "\n")
}

// scrollNotice is the muted footer that replaces the blank gap whenever the
// body overflows: how many content lines are hidden in each direction and the
// keys that reach them. countContentLines skips the blank separator so "N more
// lines" counts lines that carry words, matching the notice's own wording.
func (c *ConfirmationOverlay) scrollNotice(body []string, budget, width int) string {
	var parts []string
	if above := countContentLines(body[:c.scroll]); above > 0 {
		parts = append(parts, moreLinesLabel("↑", above))
	}
	if below := countContentLines(body[c.scroll+budget:]); below > 0 {
		parts = append(parts, moreLinesLabel("↓", below))
	}
	if len(parts) == 0 {
		return ""
	}
	notice := "… " + strings.Join(parts, " · ") + " · " + c.scrollKeysLabel()
	muted := lipgloss.NewStyle().Foreground(ui.CurrentTheme().InkMuted).Render(notice)
	return truncateOverlayLine(muted, width)
}

// moreLinesLabel phrases one direction's hidden count — "↑ 1 more line",
// "↓ 3 more lines".
func moreLinesLabel(arrow string, n int) string {
	if n == 1 {
		return arrow + " 1 more line"
	}
	return fmt.Sprintf("%s %d more lines", arrow, n)
}

// scrollKeysLabel names the keys the scroll notice may advertise: j/k only on
// a dialog that did not claim either letter as its confirm/cancel key —
// telling the reader 'k' scrolls on a dialog where 'k' kills would be a worse
// lie than no hint at all (#1238/#2022 vs #5171). The bare glyph list stays
// short on purpose: at narrow widths a "scroll" verb would be the first thing
// truncated away, taking the keys with it.
func (c *ConfirmationOverlay) scrollKeysLabel() string {
	keys := "↑/↓"
	var extra []string
	if !c.scrollKeyCollides("j") {
		extra = append(extra, "j")
	}
	if !c.scrollKeyCollides("k") {
		extra = append(extra, "k")
	}
	if len(extra) > 0 {
		keys += " or " + strings.Join(extra, "/")
	}
	return keys
}

func (c *ConfirmationOverlay) scrollKeyCollides(key string) bool {
	return strings.EqualFold(c.ConfirmKey, key) || strings.EqualFold(c.CancelKey, key)
}

// refusalContent is what a guarded overlay shows when the window cannot fit its
// consequences: it names why, says how much room is missing, and offers only
// cancel. HandleKeyPress rejects the confirm key in this state, so the action is
// genuinely withheld rather than merely discouraged.
func (c *ConfirmationOverlay) refusalContent(width, height int) string {
	hint := wrapOverlayLines(lipgloss.NewStyle().Bold(true).Render("esc")+" cancel", width)
	budget, gap := bodyBudget(height, len(hint))

	critical := wrapOverlayLines(c.message, width)
	short := len(critical) - budget
	if short < 1 {
		short = 1
	}

	// Pick the longest refusal that actually FITS. Windowing this text would be
	// self-defeating: the refusal exists because content was being swallowed, so
	// a refusal degraded into "… N more lines" would say nothing at exactly the
	// moment saying something is the entire point.
	body := wrapOverlayLines(refusalNotices(short)[len(refusalNotices(short))-1], width)
	for _, candidate := range refusalNotices(short) {
		if wrapped := wrapOverlayLines(candidate, width); len(wrapped) <= budget {
			body = wrapped
			break
		}
	}
	lines := append([]string{}, body...)
	if gap > 0 && len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, hint...)
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// refusalNotices lists the refusal wording from most informative to least. A
// window too small even for the explanation still gets a true sentence — every
// variant leads with "Too small", so the reason survives all the way down.
func refusalNotices(short int) []string {
	unit := "lines"
	if short == 1 {
		unit = "line"
	}
	return []string{
		fmt.Sprintf("Too small to confirm safely — %d more %s needed to show what this destroys. Resize, then try again.", short, unit),
		fmt.Sprintf("Too small to confirm safely — %d more %s needed. Resize.", short, unit),
		"Too small to confirm safely · resize",
		"Too small · resize",
	}
}

func (c *ConfirmationOverlay) instruction(compact bool) string {
	if c.pending != "" {
		// No confirm key while pending: advertising one the handler refuses would
		// read as a dead key. The cancel words stay, so the no-zone still registers.
		return ui.ActionStyle(false).Render(c.pending) + " · " +
			ui.ActionStyle(false).Render(c.CancelKey+"/esc cancel")
	}
	confirm := c.ConfirmKey
	if !compact && c.enterConfirms() {
		confirm += "/enter"
	}
	return ui.ActionStyle(true).Render(confirm+" confirm") + " · " +
		ui.ActionStyle(false).Render(c.CancelKey+"/esc cancel")
}

// countContentLines ignores blank spacers so the notice counts lines that
// actually carry words — "1 more line" must mean one line of text, not a gap.
func countContentLines(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
