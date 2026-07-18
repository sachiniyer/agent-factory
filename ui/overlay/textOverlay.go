package overlay

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/ui"
)

const (
	textOverlayHorizontalPadding = 2
	textOverlayVerticalPadding   = 1
)

// TextOverlay represents a text screen overlay
type TextOverlay struct {
	// Whether the overlay has been dismissed
	Dismissed bool
	// OnDismiss is invoked once when the user dismisses the overlay. It may
	// return a tea.Cmd that callers should feed back into the bubbletea event
	// loop — used by the attach-overlay path (#579) so the post-detach
	// goroutine can dispatch an immediate repaintAfterDetachMsg{} instead of
	// waiting up to one previewTickMsg cycle (~100ms) for the next paint.
	OnDismiss func() tea.Cmd
	// Content to display in the overlay
	content string

	width  int
	height int
	scroll int
}

// NewTextOverlay creates a new text screen overlay with the given title and content
func NewTextOverlay(content string) *TextOverlay {
	return &TextOverlay{
		Dismissed: false,
		content:   content,
		// Default width so PlaceOverlay can center/fade on narrow terminals.
		// Callers should invoke SetWidth once the actual terminal size is known.
		width:  60,
		height: 20,
	}
}

// HandleKeyPress processes a key press and updates the state. Returns the
// caller-supplied OnDismiss cmd (if any) so the bubbletea Update path can
// feed it into tea.Batch, plus true to indicate the overlay should close.
func (t *TextOverlay) HandleKeyPress(msg tea.KeyMsg) (tea.Cmd, bool) {
	// Close on any key
	t.Dismissed = true
	var cmd tea.Cmd
	if t.OnDismiss != nil {
		cmd = t.OnDismiss()
	}
	return cmd, true
}

// Render renders the text overlay
func (t *TextOverlay) Render() string {
	// Create styles
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ui.CurrentTheme().Accent).
		Padding(textOverlayVerticalPadding, textOverlayHorizontalPadding).
		Width(t.width)
	if inner := t.innerHeight(); inner > 0 && t.contentOverflows(inner) {
		style = style.Height(inner)
	}

	// Apply the border style and return
	return style.Render(t.visibleContent())
}

func (t *TextOverlay) SetWidth(width int) {
	t.width = width
}

func (t *TextOverlay) SetHeight(height int) {
	t.height = height
	t.clampScroll(t.innerHeight())
}

func (t *TextOverlay) ScrollUp() {
	if t.scroll > 0 {
		t.scroll--
	}
}

func (t *TextOverlay) ScrollDown() {
	t.scroll++
	t.clampScroll(t.innerHeight())
}

func (t *TextOverlay) innerHeight() int {
	if t.height <= 0 {
		return 0
	}
	inner := t.height - 2 - textOverlayVerticalPadding*2 // border + vertical padding
	if inner < 1 {
		return 1
	}
	return inner
}

func (t *TextOverlay) textWidth() int {
	width := t.width - textOverlayHorizontalPadding*2
	if width < 1 {
		return 1
	}
	return width
}

func (t *TextOverlay) clampScroll(inner int) {
	if t.scroll < 0 {
		t.scroll = 0
	}
	if inner <= 0 {
		return
	}
	lines := t.wrappedContentLines()
	maxScroll := len(lines) - inner
	if maxScroll < 0 {
		maxScroll = 0
	}
	if t.scroll > maxScroll {
		t.scroll = maxScroll
	}
}

func (t *TextOverlay) contentOverflows(inner int) bool {
	return inner > 0 && len(t.wrappedContentLines()) > inner
}

func (t *TextOverlay) visibleContent() string {
	inner := t.innerHeight()
	if inner <= 0 {
		return t.content
	}
	lines := t.wrappedContentLines()
	t.clampScroll(inner)
	if len(lines) <= inner {
		return strings.Join(lines, "\n")
	}
	end := t.scroll + inner
	if end > len(lines) {
		end = len(lines)
	}
	visible := append([]string(nil), lines[t.scroll:end]...)
	if t.scroll > 0 && len(visible) > 0 {
		visible[0] = textOverlayScrollMarker(t.textWidth(), "↑ more")
	}
	if end < len(lines) && len(visible) > 0 {
		visible[len(visible)-1] = textOverlayScrollMarker(t.textWidth(), "↓ more")
	}
	return strings.Join(visible, "\n")
}

// wrappedContentLines splits the content into the physical rows the box will
// actually render. It MUST use the same wrapper lipgloss applies internally
// (ansi.Wrap at width−padding, hard-breaking over-long words) so every logical
// line here maps to exactly one rendered row. wordwrap.String is a *soft* wrap
// that can leave a line one cell past the limit; lipgloss then re-wraps that
// line into two rows, so the scroll/height math — which counts one line as one
// row — under-counts, the box grows past the terminal, and PlaceOverlay dumps
// the raw un-centered frame with its top border clipped (#1998).
func (t *TextOverlay) wrappedContentLines() []string {
	return strings.Split(xansi.Wrap(t.content, t.textWidth(), ""), "\n")
}

func textOverlayScrollMarker(width int, marker string) string {
	if width < 1 {
		width = 1
	}
	return lipgloss.NewStyle().
		Foreground(ui.CurrentTheme().ForegroundDim).
		Render(lipgloss.PlaceHorizontal(width, lipgloss.Center, marker))
}
