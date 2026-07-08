package overlay

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	confirmationOverlayHorizontalPadding = 2
	confirmationOverlayVerticalPadding   = 1
)

// ConfirmationOverlay represents a confirmation dialog overlay
type ConfirmationOverlay struct {
	// Whether the overlay has been dismissed
	Dismissed bool
	// Message to display in the overlay
	message string
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
	borderColor lipgloss.Color
}

// NewConfirmationOverlay creates a new confirmation dialog overlay with the given message
func NewConfirmationOverlay(message string) *ConfirmationOverlay {
	return &ConfirmationOverlay{
		Dismissed:   false,
		message:     message,
		width:       50, // Default width
		ConfirmKey:  "y",
		CancelKey:   "n",
		borderColor: lipgloss.Color("#de613e"), // Red color for confirmations
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
	case strings.ToLower(c.ConfirmKey):
		c.Dismissed = true
		if c.OnConfirm != nil {
			c.OnConfirm()
		}
		return true
	default:
		// Ignore other keys in confirmation state
		return false
	}
}

// Render renders the confirmation overlay
func (c *ConfirmationOverlay) Render() string {
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c.borderColor).
		Padding(confirmationOverlayVerticalPadding, confirmationOverlayHorizontalPadding)

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

func (c *ConfirmationOverlay) visibleContent(width, height int) string {
	body := wrapOverlayLines(c.message, width)
	hint := wrapOverlayLines(c.instruction(false), width)
	compactHint := wrapOverlayLines(c.instruction(true), width)
	if height > 0 && (len(body)+1+len(hint) > height || len(hint) > 2) {
		hint = compactHint
	}
	if height <= 0 {
		return strings.Join(append(append([]string{}, body...), append([]string{""}, hint...)...), "\n")
	}
	if len(hint) >= height {
		return strings.Join(hint[:height], "\n")
	}

	gap := 1
	bodyLimit := height - len(hint) - gap
	if bodyLimit < 1 {
		gap = 0
		bodyLimit = height - len(hint)
	}
	body = windowOverlayBody(body, bodyLimit, width)
	lines := append([]string{}, body...)
	if gap > 0 && len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, hint...)
	return strings.Join(lines, "\n")
}

func (c *ConfirmationOverlay) instruction(compact bool) string {
	bold := lipgloss.NewStyle().Bold(true).Render
	if compact {
		return bold(c.ConfirmKey) + " confirm • " +
			bold(c.CancelKey) + "/" + bold("esc") + " cancel"
	}
	return "Press " + bold(c.ConfirmKey) + " to confirm, " +
		bold(c.CancelKey) + " or " + bold("esc") + " to cancel"
}

func windowOverlayBody(lines []string, limit, width int) []string {
	if limit <= 0 {
		return nil
	}
	if len(lines) <= limit {
		return lines
	}
	if limit == 1 {
		return []string{truncateOverlayLine("…", width)}
	}
	out := append([]string{}, lines[:limit-1]...)
	out = append(out, truncateOverlayLine("…", width))
	return out
}
