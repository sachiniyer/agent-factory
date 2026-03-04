package ui

import (
	"claude-squad/log"
	"claude-squad/microclaw"
	"claude-squad/session/tmux"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var mcPaneStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

var mcFooterStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"})

// MicroClawPane manages a tmux session running the interactive microclaw TUI.
// It captures the tmux pane output for display in the tabbed window and
// supports attaching for full interactive use.
type MicroClawPane struct {
	mu           sync.Mutex
	bridge       *microclaw.Bridge
	tmuxSession  *tmux.TmuxSession
	width        int
	height       int
	content      string
	fallback     bool
	fallbackText string
	isScrolling  bool
	viewport     viewport.Model
}

// NewMicroClawPane creates a new pane backed by the given bridge.
func NewMicroClawPane(bridge *microclaw.Bridge) *MicroClawPane {
	return &MicroClawPane{
		bridge:   bridge,
		viewport: viewport.New(0, 0),
	}
}

func (p *MicroClawPane) SetSize(width, height int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.width = width
	p.height = height
	p.viewport.Width = width
	p.viewport.Height = height
	if p.tmuxSession != nil {
		if err := p.tmuxSession.SetDetachedSize(width, height); err != nil {
			log.InfoLog.Printf("microclaw pane: failed to set detached size: %v", err)
		}
	}
}

// setFallbackState sets the pane to display a fallback message.
// Caller must hold p.mu.
func (p *MicroClawPane) setFallbackState(message string) {
	p.fallback = true
	p.fallbackText = lipgloss.JoinVertical(lipgloss.Center, FallBackText, "", message)
	p.content = ""
}

// Refresh captures the tmux pane output for display. Creates the session if needed.
func (p *MicroClawPane) Refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.bridge == nil || !p.bridge.Available() {
		p.setFallbackState("MicroClaw not available.\nSet MICROCLAW_DIR or install microclaw.")
		return
	}

	// Skip content updates while in scroll mode
	if p.isScrolling {
		return
	}

	// Ensure the tmux session is running
	if err := p.ensureSessionLocked(); err != nil {
		p.setFallbackState(fmt.Sprintf("Failed to start MicroClaw session: %v", err))
		return
	}

	if p.tmuxSession == nil || !p.tmuxSession.DoesSessionExist() {
		p.setFallbackState("MicroClaw session not available.")
		return
	}

	content, err := p.tmuxSession.CapturePaneContent()
	if err != nil {
		p.setFallbackState(fmt.Sprintf("Failed to capture content: %v", err))
		return
	}

	p.fallback = false
	p.content = content
}

// ensureSessionLocked creates the tmux session running `cs microclaw` if it doesn't exist.
// Caller must hold p.mu.
func (p *MicroClawPane) ensureSessionLocked() error {
	if p.tmuxSession != nil && p.tmuxSession.DoesSessionExist() {
		return nil
	}

	// Find the binary path — use the running executable
	binary, err := os.Executable()
	if err != nil {
		binary = os.Args[0]
	}

	cmd := binary + " microclaw"
	ts := tmux.NewTmuxSession("microclaw_tui", cmd)

	// Check if session already exists (e.g. from a previous run)
	if ts.DoesSessionExist() {
		if err := ts.Restore(); err != nil {
			_ = ts.Close()
			ts = tmux.NewTmuxSession("microclaw_tui", cmd)
			if err := ts.Start("."); err != nil {
				return fmt.Errorf("failed to start microclaw session: %w", err)
			}
		}
	} else {
		if err := ts.Start("."); err != nil {
			return fmt.Errorf("failed to start microclaw session: %w", err)
		}
	}

	p.tmuxSession = ts

	if p.width > 0 && p.height > 0 {
		if err := ts.SetDetachedSize(p.width, p.height); err != nil {
			log.InfoLog.Printf("microclaw pane: failed to set size: %v", err)
		}
	}

	return nil
}

// Attach attaches to the microclaw tmux session for full interactive use.
func (p *MicroClawPane) Attach() (chan struct{}, error) {
	p.mu.Lock()
	if p.tmuxSession == nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("no microclaw session to attach to")
	}
	if !p.tmuxSession.DoesSessionExist() {
		p.mu.Unlock()
		return nil, fmt.Errorf("microclaw session does not exist")
	}
	ts := p.tmuxSession
	p.mu.Unlock()
	return ts.Attach()
}

// Close kills the microclaw tmux session.
func (p *MicroClawPane) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tmuxSession != nil {
		if err := p.tmuxSession.Close(); err != nil {
			log.InfoLog.Printf("microclaw pane: failed to close session: %v", err)
		}
		p.tmuxSession = nil
	}
	p.content = ""
	p.fallback = false
	p.fallbackText = ""
}

func (p *MicroClawPane) ScrollUp() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.isScrolling {
		p.enterScrollMode()
		return
	}
	p.viewport.LineUp(1)
}

func (p *MicroClawPane) ScrollDown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.isScrolling {
		p.enterScrollMode()
		return
	}
	p.viewport.LineDown(1)
}

func (p *MicroClawPane) enterScrollMode() {
	if p.tmuxSession == nil || !p.tmuxSession.DoesSessionExist() {
		return
	}

	content, err := p.tmuxSession.CapturePaneContentWithOptions("-", "-")
	if err != nil {
		log.InfoLog.Printf("microclaw pane: failed to capture full history: %v", err)
		return
	}

	footer := mcFooterStyle.Render("ESC to exit scroll mode")
	contentWithFooter := lipgloss.JoinVertical(lipgloss.Left, content, footer)
	p.viewport.SetContent(contentWithFooter)
	p.viewport.GotoBottom()
	p.isScrolling = true
}

// IsScrolling returns whether the microclaw pane is in scroll mode.
func (p *MicroClawPane) IsScrolling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isScrolling
}

// ResetToNormalMode exits scroll mode.
func (p *MicroClawPane) ResetToNormalMode() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.isScrolling {
		return
	}
	p.isScrolling = false
	p.viewport.SetContent("")
	p.viewport.GotoTop()
}

func (p *MicroClawPane) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	width := p.width
	height := p.height

	if width == 0 || height == 0 {
		return strings.Repeat("\n", height)
	}

	if p.isScrolling {
		return p.viewport.View()
	}

	if p.fallback {
		availableHeight := height - 3 - 4
		fallbackLines := len(strings.Split(p.fallbackText, "\n"))
		totalPadding := availableHeight - fallbackLines
		topPadding := 0
		bottomPadding := 0
		if totalPadding > 0 {
			topPadding = totalPadding / 2
			bottomPadding = totalPadding - topPadding
		}

		var lines []string
		if topPadding > 0 {
			lines = append(lines, strings.Repeat("\n", topPadding))
		}
		lines = append(lines, p.fallbackText)
		if bottomPadding > 0 {
			lines = append(lines, strings.Repeat("\n", bottomPadding))
		}

		return mcPaneStyle.
			Width(width).
			Align(lipgloss.Center).
			Render(strings.Join(lines, ""))
	}

	// Normal mode: show captured content
	lines := strings.Split(p.content, "\n")

	if height > 0 {
		if len(lines) > height {
			lines = lines[len(lines)-height:]
		} else {
			padding := height - len(lines)
			lines = append(lines, make([]string, padding)...)
		}
	}

	contentStr := strings.Join(lines, "\n")
	return mcPaneStyle.Width(width).Render(contentStr)
}
