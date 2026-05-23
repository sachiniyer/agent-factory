package ui

import (
	"errors"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var previewPaneStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

type PreviewPane struct {
	// mu serialises UpdateContent (called off the bubbletea Update goroutine
	// via refreshPanesCmd in app/) against String() (called from the
	// bubbletea renderer). Without it the renderer can observe partially-
	// written previewState while a capture is in flight (#579). Scroll-mode
	// state and viewport mutations go through the same lock for the same
	// reason. ResetToNormalMode is invoked from the renderer thread but is
	// still guarded by the mutex so concurrent UpdateContent calls (from the
	// async refresh path) cannot race with the scroll-mode reset.
	mu sync.Mutex

	width  int
	height int

	previewState previewState
	isScrolling  bool
	viewport     viewport.Model
	// currentInstance is the instance whose content is currently rendered.
	// Tracked so UpdateContent can detect instance switches and drop stale
	// scroll-mode viewport content belonging to the previous instance.
	currentInstance *session.Instance
}

// previewState holds the rendered content of the preview pane.
//
// Invariant: fallback==true iff text is a centered fallback message
// (loading / error / inactive). Writers MUST replace the whole struct
// rather than mutate fields individually, so the two fields can never
// disagree about which rendering branch String() should take (#577).
type previewState struct {
	// fallback is true if the preview pane is displaying fallback text
	fallback bool
	// text is the text displayed in the preview pane
	text string
}

func NewPreviewPane() *PreviewPane {
	return &PreviewPane{
		viewport: viewport.New(0, 0),
	}
}

// IsScrolling reports whether the preview pane is in scroll mode. Locks
// p.mu to match the mutators (#579 race fix).
func (p *PreviewPane) IsScrolling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isScrolling
}

func (p *PreviewPane) SetSize(width, maxHeight int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.width = width
	p.height = maxHeight
	p.viewport.Width = width
	p.viewport.Height = maxHeight
}

// setFallbackState sets the preview state with fallback text and a message
func (p *PreviewPane) setFallbackState(message string) {
	p.previewState = previewState{
		fallback: true,
		text:     lipgloss.JoinVertical(lipgloss.Center, FallBackText, "", message),
	}
}

// Updates the preview pane content with the tmux pane content. Safe to call
// from a goroutine — UpdateContent serialises against String() and the other
// mutators via p.mu so the renderer never observes a half-written state
// (#579). The Preview()/PreviewFullHistory() shell-outs happen while the
// lock is held; the renderer briefly blocks waiting for them, which is the
// cost of removing the partial-state race. The shell-outs are ~3–5ms locally
// so the wait is bounded.
func (p *PreviewPane) UpdateContent(instance *session.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// If the selected instance changed since the last render, drop any
	// scroll-mode viewport content captured from the previous instance.
	// Otherwise switching instances while scrolling leaves the viewport
	// pinned on the previous instance's output (issue #470).
	if instance != p.currentInstance {
		if p.isScrolling {
			p.isScrolling = false
			p.viewport.SetContent("")
			p.viewport.GotoTop()
		}
		p.currentInstance = instance
	}

	switch {
	case instance == nil:
		p.setFallbackState("No agents running yet. Spin up a new instance with 'n' to get started!")
		return nil
	case instance.GetStatus() == session.Loading:
		p.setFallbackState("Setting up workspace...")
		return nil
	}

	var content string
	var err error

	// If in scroll mode but haven't captured content yet, do it now
	if p.isScrolling && p.viewport.Height > 0 && len(p.viewport.View()) == 0 {
		// Capture full pane content including scrollback history using capture-pane -p -S -
		content, err = instance.PreviewFullHistory()
		if err != nil {
			if errors.Is(err, tmux.ErrSessionGone) {
				p.isScrolling = false
				p.setFallbackState("Session no longer running.")
				return nil
			}
			return err
		}

		// Set content in the viewport
		footer := lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"}).
			Render("ESC to exit scroll mode")

		p.viewport.SetContent(lipgloss.JoinVertical(lipgloss.Left, content, footer))
	} else if !p.isScrolling {
		// In normal mode, use the usual preview
		content, err = instance.Preview()
		if err != nil {
			// Tmux session vanished out from under us (#496). Render an
			// inactive-session fallback instead of propagating the error
			// up to handleError, which would log "error capturing pane
			// content" at ERROR every preview tick (every 100ms).
			if errors.Is(err, tmux.ErrSessionGone) {
				p.setFallbackState("Session no longer running.")
				return nil
			}
			return err
		}

		// Always update the preview state with content, even if empty
		// This ensures that newly created instances will display their content immediately
		if len(content) == 0 && !instance.Started() {
			p.setFallbackState("Please enter a name for the instance.")
		} else {
			// Update the preview state with the current content
			p.previewState = previewState{
				fallback: false,
				text:     content,
			}
		}
	}

	return nil
}

// Returns the preview pane content as a string.
func (p *PreviewPane) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.width <= 0 || p.height <= 0 {
		return ""
	}

	// If in scroll/copy mode, always use the viewport
	if p.isScrolling {
		return p.viewport.View()
	}

	if p.previewState.fallback {
		// TabbedWindow.SetSize already subtracts borders/margins/padding from
		// p.height, so we use p.height directly to match normal mode (which
		// pads to the full p.height). Subtracting again here would
		// double-count chrome and leave a trailing blank line (#616).
		availableHeight := p.height

		// Count the number of lines in the fallback text
		fallbackLines := len(strings.Split(p.previewState.text, "\n"))

		// Calculate padding needed above and below to center the content
		totalPadding := availableHeight - fallbackLines
		topPadding := 0
		bottomPadding := 0
		if totalPadding > 0 {
			topPadding = totalPadding / 2
			bottomPadding = totalPadding - topPadding // accounts for odd numbers
		}

		// Build the centered content
		var lines []string
		if topPadding > 0 {
			lines = append(lines, strings.Repeat("\n", topPadding))
		}
		lines = append(lines, p.previewState.text)
		if bottomPadding > 0 {
			lines = append(lines, strings.Repeat("\n", bottomPadding))
		}

		// Center both vertically and horizontally
		return previewPaneStyle.
			Width(p.width).
			Align(lipgloss.Center).
			Render(strings.Join(lines, ""))
	}

	// Normal mode display
	lines := strings.Split(p.previewState.text, "\n")

	// strings.Split produces a trailing empty element when text ends in "\n"
	// (common for terminal capture output). Drop it so the off-by-one does
	// not trigger truncation when content actually fits, and so the truncate
	// branch keeps the right slice of lines (#649).
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// Truncate to the most recent p.height lines (match TerminalPane: show
	// newest output, not oldest — #649).
	if p.height > 0 {
		if len(lines) > p.height {
			lines = lines[len(lines)-p.height:]
		} else {
			// Pad with empty lines to fill available height
			padding := p.height - len(lines)
			lines = append(lines, make([]string, padding)...)
		}
	}

	content := strings.Join(lines, "\n")
	rendered := previewPaneStyle.Width(p.width).Render(content)
	return rendered
}

// ScrollUp scrolls up in the viewport
func (p *PreviewPane) ScrollUp(instance *session.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if instance == nil {
		return nil
	}

	if !p.isScrolling {
		// Entering scroll mode - capture entire pane content including scrollback history
		content, err := instance.PreviewFullHistory()
		if err != nil {
			if errors.Is(err, tmux.ErrSessionGone) {
				p.setFallbackState("Session no longer running.")
				return nil
			}
			return err
		}

		// Set content in the viewport
		footer := lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"}).
			Render("ESC to exit scroll mode")

		contentWithFooter := lipgloss.JoinVertical(lipgloss.Left, content, footer)
		p.viewport.SetContent(contentWithFooter)

		// Position the viewport at the bottom initially
		p.viewport.GotoBottom()

		p.isScrolling = true
		return nil
	}

	// Already in scroll mode, just scroll the viewport
	p.viewport.LineUp(1)
	return nil
}

// ScrollDown scrolls down in the viewport
func (p *PreviewPane) ScrollDown(instance *session.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if instance == nil {
		return nil
	}

	if !p.isScrolling {
		// Entering scroll mode - capture entire pane content including scrollback history
		content, err := instance.PreviewFullHistory()
		if err != nil {
			if errors.Is(err, tmux.ErrSessionGone) {
				p.setFallbackState("Session no longer running.")
				return nil
			}
			return err
		}

		// Set content in the viewport
		footer := lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"}).
			Render("ESC to exit scroll mode")

		contentWithFooter := lipgloss.JoinVertical(lipgloss.Left, content, footer)
		p.viewport.SetContent(contentWithFooter)

		// Position the viewport at the bottom initially
		p.viewport.GotoBottom()

		p.isScrolling = true
		return nil
	}

	// Already in copy mode, just scroll the viewport
	p.viewport.LineDown(1)
	return nil
}

// ResetToNormalMode exits scroll mode and returns to normal mode
func (p *PreviewPane) ResetToNormalMode(instance *session.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Always clear scroll state first so that pressing ESC while no instance
	// is selected (e.g., the sidebar header) does not leave the preview pane
	// stuck on stale viewport content. Mirrors TerminalPane.ResetToNormalMode.
	wasScrolling := p.isScrolling
	if wasScrolling {
		p.isScrolling = false
		// Reset viewport
		p.viewport.SetContent("")
		p.viewport.GotoTop()
	}

	if instance == nil {
		return nil
	}

	if wasScrolling {
		// Immediately update content instead of waiting for next UpdateContent call
		content, err := instance.Preview()
		if err != nil {
			if errors.Is(err, tmux.ErrSessionGone) {
				p.setFallbackState("Session no longer running.")
				return nil
			}
			return err
		}
		p.previewState = previewState{
			fallback: false,
			text:     content,
		}
	}

	return nil
}
