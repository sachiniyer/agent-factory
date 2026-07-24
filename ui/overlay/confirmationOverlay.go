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
	// message becomes the part the user MUST read to consent, and only detail
	// may be clipped — announced, never swallowed. With no detail the whole
	// message stays clippable, which is the historical behavior.
	detail string
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
	// Custom styling options
	borderColor lipgloss.TerminalColor
}

// NewConfirmationOverlay creates a new confirmation dialog overlay with the given message
func NewConfirmationOverlay(message string) *ConfirmationOverlay {
	return &ConfirmationOverlay{
		Dismissed:   false,
		message:     message,
		width:       50, // Default width
		ConfirmKey:  defaultConfirmKey,
		CancelKey:   "n",
		borderColor: ui.CurrentTheme().Error,
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
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c.borderColor).
		Padding(confirmationOverlayVerticalPadding, confirmationOverlayHorizontalPadding)
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
	return style.Render(content)
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

// SetDetail sets elaboration rendered below the message, and opts this overlay
// into the critical-content guarantee (#1973). Split the copy so the message
// carries the consequences the user is consenting to and the detail carries the
// explanation: the message then either renders in full — with any clipped detail
// announced — or the overlay refuses to confirm at all. Use it for any confirm
// whose message would be a lie if its tail fell below the fold.
func (c *ConfirmationOverlay) SetDetail(detail string) {
	c.detail = detail
}

// guarded reports whether this overlay carries a critical/detail split, i.e.
// whether its message must be readable for a confirm to be legitimate.
func (c *ConfirmationOverlay) guarded() bool {
	return strings.TrimSpace(c.detail) != ""
}

// detailLines wraps the elaboration. The blank spacer that separates it from
// the message is added only when the detail fits whole (see fitDetail) — under
// pressure a spacer is a line of nothing, and lines of nothing are the first
// thing to surrender.
func (c *ConfirmationOverlay) detailLines(width int) []string {
	if !c.guarded() {
		return nil
	}
	return wrapOverlayLines(c.detail, width)
}

// fitDetail places the elaboration in whatever room the message left, and
// reports the notice the caller must show if anything was dropped.
//
// The notice is returned rather than rendered because when room runs out
// entirely it goes in the blank separator's slot — the gap is a line we were
// spending on nothing, so announcing the clip there costs zero lines. That is
// what lets the consequences fit at the declared 40x10 floor AND still say that
// more text exists, instead of trading one against the other.
func fitDetail(detail []string, room, width int) (lines []string, notice string) {
	switch {
	case len(detail) == 0:
		return nil, ""
	case room >= len(detail)+1:
		// Room for the spacer too — render it as designed.
		return append([]string{""}, detail...), ""
	case room >= 1:
		return windowOverlayBody(detail, room, width), ""
	default:
		return nil, moreLinesNotice(countContentLines(detail))
	}
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

// tooSmallToConfirm reports whether a guarded overlay cannot render its message
// plus the confirm prompt at the given text rect. Such an overlay must refuse
// the action outright: a destructive confirm that cannot show its consequences
// has no business collecting a 'y' (#1973). Unguarded overlays never refuse.
func (c *ConfirmationOverlay) tooSmallToConfirm(width, height int) bool {
	if !c.guarded() || height <= 0 || width <= 0 {
		return false
	}
	critical := wrapOverlayLines(c.message, width)
	budget, _ := bodyBudget(height, len(c.fittedHint(width, height)))
	return budget < len(critical)
}

// fittedHint picks the full or compact confirm prompt for the available height.
func (c *ConfirmationOverlay) fittedHint(width, height int) []string {
	hint := wrapOverlayLines(c.instruction(false), width)
	if height <= 0 {
		return hint
	}
	body := len(wrapOverlayLines(c.message, width)) + len(c.detailLines(width))
	if body+1+len(hint) > height || len(hint) > 2 {
		return wrapOverlayLines(c.instruction(true), width)
	}
	return hint
}

func (c *ConfirmationOverlay) visibleContent(width, height int) string {
	critical := wrapOverlayLines(c.message, width)
	detail := c.detailLines(width)

	if height <= 0 {
		// Unbounded: everything renders, spacer and all.
		lines := append([]string{}, critical...)
		if len(detail) > 0 {
			lines = append(append(lines, ""), detail...)
		}
		return strings.Join(append(lines, append([]string{""}, wrapOverlayLines(c.instruction(false), width)...)...), "\n")
	}

	hint := c.fittedHint(width, height)

	// A guarded overlay that cannot show what it destroys refuses instead of
	// rendering a reassuring fragment above a hidden consequence.
	if c.tooSmallToConfirm(width, height) {
		return c.refusalContent(width, height)
	}

	if !c.guarded() && len(hint) >= height {
		return strings.Join(hint[:height], "\n")
	}

	budget, gap := bodyBudget(height, len(hint))

	var lines []string
	gapLine := ""
	if c.guarded() {
		// The message is never windowed — tooSmallToConfirm already proved it
		// fits. Only the elaboration gives ground, and it says what it dropped.
		detailLines, notice := fitDetail(detail, budget-len(critical), width)
		lines = append(lines, critical...)
		lines = append(lines, detailLines...)
		gapLine = notice
	} else {
		lines = windowOverlayBody(critical, budget, width)
	}

	if gap > 0 && len(lines) > 0 {
		lines = append(lines, gapLine)
	}
	lines = append(lines, hint...)
	return strings.Join(lines, "\n")
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
	bold := lipgloss.NewStyle().Bold(true).Render
	if compact {
		return bold(c.ConfirmKey) + " confirm • " +
			bold(c.CancelKey) + "/" + bold("esc") + " cancel"
	}
	confirmKeys := bold(c.ConfirmKey)
	if c.enterConfirms() {
		// "/enter" mirrors the compact "n/esc" idiom and keeps the full hint on one
		// line at the confirmation's fixed width, so its click zone survives (#2405).
		confirmKeys += "/" + bold("enter")
	}
	return "Press " + confirmKeys + " to confirm, " +
		bold(c.CancelKey) + " or " + bold("esc") + " to cancel"
}

// windowOverlayBody keeps the leading lines and surrenders the tail, replacing
// what it drops with a notice that SAYS how much is missing. The old bare "…"
// was indistinguishable from "there was nothing else to say" — the reader could
// not tell a styled ellipsis from swallowed content, which is how a hidden
// consequence reads as an absent one (#1973).
func windowOverlayBody(lines []string, limit, width int) []string {
	if limit <= 0 {
		return nil
	}
	if len(lines) <= limit {
		return lines
	}
	if limit == 1 {
		return []string{truncateOverlayLine(moreLinesNotice(countContentLines(lines)), width)}
	}
	out := append([]string{}, lines[:limit-1]...)
	return append(out, truncateOverlayLine(moreLinesNotice(countContentLines(lines[limit-1:])), width))
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

// moreLinesNotice names what the clip is hiding and how to read it.
func moreLinesNotice(n int) string {
	if n == 1 {
		return "… 1 more line · resize to read"
	}
	return fmt.Sprintf("… %d more lines · resize to read", n)
}
