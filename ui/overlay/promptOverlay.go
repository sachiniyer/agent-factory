package overlay

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
)

// promptOverlayChrome is the number of content rows Render spends on anything
// that is not the textarea: the title, the blank under it, the blank above the
// hint, and the hint. The textarea gets whatever is left.
const promptOverlayChrome = 4

// promptOverlayDefaultHeight is the textarea height used when the overlay has
// no height budget yet (SetMaxSize not called, e.g. before the first
// WindowSizeMsg). Big enough for a paragraph without dominating the screen.
const promptOverlayDefaultHeight = 6

// PromptOverlay is the free-text entry overlay used for a new session's
// initial prompt (#1936). It is textarea-backed rather than hand-rolled like
// SearchOverlay's query string because a prompt is multi-line prose: the CLI's
// --prompt and the web's promptArea both take newlines, so the TUI field that
// closes that gap has to as well. A single-string buffer would have to spend
// Enter on submit and could never hold a hand-typed second line.
//
// So Enter inserts a newline here and does NOT submit, matching the task form's
// prompt field (ui/task_pane_edit.go), the other multi-line prompt input in the
// TUI. Tab and Esc both close the overlay KEEPING the text: this is a field of
// the create form, not a dialog, so backing out of it must not destroy what was
// typed. Cancelling the whole create stays on ctrl+c, exactly as in the naming
// flow the overlay returns to.
//
// Switching on msg.Type (not msg.String()) is load-bearing for paste: Bubble
// Tea delivers a bracketed paste as one KeyRunes message whose contents are
// "not to be interpreted further", so a pasted tab or newline reaches the
// textarea as text instead of tripping the Tab/Enter cases above.
type PromptOverlay struct {
	textarea  textarea.Model
	title     string
	canceled  bool
	width     int
	maxWidth  int
	maxHeight int
}

// NewPromptOverlay creates a prompt overlay seeded with value, so reopening it
// shows what is already attached to the pending session rather than a blank
// box.
func NewPromptOverlay(title, value string) *PromptOverlay {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.Placeholder = "Sent to the agent as soon as it is ready…"
	// No cap: the daemon delivers this verbatim to the agent, and an initial
	// prompt is legitimately a paragraph or a pasted spec.
	ta.CharLimit = 0
	ta.MaxHeight = 0
	// The cursor-line highlight spans the full textarea width, which reads as a
	// selection bar inside a bordered box. Drop it (the task form does the same).
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.SetValue(value)
	// Land the cursor after the seeded text so reopening resumes typing rather
	// than inserting at the top.
	ta.CursorEnd()
	ta.Focus()

	return &PromptOverlay{
		textarea: ta,
		title:    title,
		width:    60,
	}
}

// HandleKeyPress processes a key press. Returns true if the overlay should
// close.
func (p *PromptOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyCtrlC:
		p.canceled = true
		return true
	case tea.KeyTab, tea.KeyEsc:
		return true
	}
	// Everything else — including Enter — is text. The blink cmd is dropped
	// deliberately: this repo has no animated indicators (#1766).
	p.textarea, _ = p.textarea.Update(msg)
	return false
}

// IsCanceled reports whether the overlay closed on ctrl+c, which cancels the
// whole session create rather than just this field.
func (p *PromptOverlay) IsCanceled() bool {
	return p.canceled
}

// Value returns the text typed so far.
func (p *PromptOverlay) Value() string {
	return p.textarea.Value()
}

// SetWidth sets the preferred overlay width.
func (p *PromptOverlay) SetWidth(width int) {
	p.width = width
}

// SetMaxSize sets the maximum outer size the rendered overlay may occupy.
func (p *PromptOverlay) SetMaxSize(width, height int) {
	p.maxWidth = width
	p.maxHeight = height
}

// Render renders the prompt overlay.
func (p *PromptOverlay) Render() string {
	t := ui.CurrentTheme()
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
	hintStyle := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	// Themed styles are resolved per render, like every sibling overlay, so a
	// theme change lands without rebuilding the overlay.
	placeholder := lipgloss.NewStyle().Foreground(t.ForegroundDim)
	p.textarea.FocusedStyle.Placeholder = placeholder
	p.textarea.BlurredStyle.Placeholder = placeholder

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Accent).
		Padding(1, 2)
	fit := fitOverlayContent(p.width, 0, p.maxWidth, p.maxHeight, style)
	if fit.W <= 0 {
		fit.W = p.width
	}
	if fit.W <= 0 {
		fit.W = 1
	}
	textRect := overlayTextRect(fit, style)

	rows := promptOverlayDefaultHeight
	if textRect.H > 0 {
		rows = textRect.H - promptOverlayChrome
		if rows < 1 {
			rows = 1
		}
	}
	p.textarea.SetWidth(textRect.W)
	p.textarea.SetHeight(rows)

	lines := []string{truncateOverlayLine(titleStyle.Render(p.title), textRect.W), ""}
	lines = append(lines, strings.Split(p.textarea.View(), "\n")...)
	lines = append(lines, "")

	hint := "enter newline • tab done • ctrl+c cancel"
	if lipgloss.Width(hint) > textRect.W {
		hint = "tab done • ctrl+c cancel"
	}
	lines = append(lines, truncateOverlayLine(hintStyle.Render(hint), textRect.W))

	style = style.Width(fit.W)
	if fit.H > 0 && len(lines) >= textRect.H {
		style = style.Height(fit.H)
	}
	return style.Render(strings.Join(lines, "\n"))
}
