package ui

import (
	"claude-squad/microclaw"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var (
	mcSenderStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7D56F4")).
			Bold(true)
	mcTimestampStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"})
	mcBotMessageStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#36CFC9"))
	mcMessageStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})
	mcStatusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFD700")).
			Bold(true)
)

// MicroClawPane displays microclaw messages in a chat-like viewport.
type MicroClawPane struct {
	viewport viewport.Model
	bridge   *microclaw.Bridge
	messages []microclaw.Message
	status   string
	width    int
	height   int
	err      error
}

// NewMicroClawPane creates a new pane backed by the given bridge.
func NewMicroClawPane(bridge *microclaw.Bridge) *MicroClawPane {
	return &MicroClawPane{
		viewport: viewport.New(0, 0),
		bridge:   bridge,
	}
}

func (p *MicroClawPane) SetSize(width, height int) {
	p.width = width
	p.height = height
	p.viewport.Width = width
	p.viewport.Height = height
}

// Refresh fetches the latest messages and status from microclaw.
func (p *MicroClawPane) Refresh() {
	if p.bridge == nil || !p.bridge.Available() {
		p.err = nil
		p.status = ""
		p.messages = nil
		return
	}

	msgs, err := p.bridge.GetRecentMessages(100)
	if err != nil {
		p.err = err
		return
	}
	p.err = nil
	p.messages = msgs

	status, err := p.bridge.Status()
	if err == nil {
		p.status = status
	}

	// Re-render content into the viewport
	p.renderContent()
}

func (p *MicroClawPane) renderContent() {
	if p.width == 0 || p.height == 0 {
		return
	}

	var sb strings.Builder

	// Status bar at the top
	if p.status != "" {
		sb.WriteString(mcStatusStyle.Render("MicroClaw — "+p.status) + "\n")
		sb.WriteString(strings.Repeat("─", p.width) + "\n")
	}

	if len(p.messages) == 0 {
		sb.WriteString("\n  No messages yet.\n")
	} else {
		for _, msg := range p.messages {
			ts := formatTimestamp(msg.Timestamp)

			sender := msg.SenderName
			if sender == "" {
				sender = "unknown"
			}

			senderStyle := mcSenderStyle
			if msg.IsFromBot == 1 {
				senderStyle = mcBotMessageStyle.Bold(true)
			}

			header := senderStyle.Render(sender) + " " + mcTimestampStyle.Render(ts)
			sb.WriteString(header + "\n")

			style := mcMessageStyle
			if msg.IsFromBot == 1 {
				style = mcBotMessageStyle
			}

			// Word-wrap content to viewport width
			wrapped := wrapText(msg.Content, p.width-2)
			sb.WriteString(style.Render("  "+strings.ReplaceAll(wrapped, "\n", "\n  ")) + "\n\n")
		}
	}

	content := sb.String()
	p.viewport.SetContent(content)
	p.viewport.GotoBottom()
}

func (p *MicroClawPane) ScrollUp() {
	p.viewport.LineUp(1)
}

func (p *MicroClawPane) ScrollDown() {
	p.viewport.LineDown(1)
}

func (p *MicroClawPane) String() string {
	if p.width == 0 || p.height == 0 {
		return ""
	}

	if p.bridge == nil || !p.bridge.Available() {
		return lipgloss.Place(
			p.width, p.height,
			lipgloss.Center, lipgloss.Center,
			lipgloss.JoinVertical(lipgloss.Center,
				FallBackText,
				"",
				"MicroClaw not available.",
				"Set MICROCLAW_DIR or install microclaw.",
			),
		)
	}

	if p.err != nil {
		return lipgloss.Place(
			p.width, p.height,
			lipgloss.Center, lipgloss.Center,
			fmt.Sprintf("Error: %v", p.err),
		)
	}

	return p.viewport.View()
}

// formatTimestamp formats an ISO timestamp into a short display form.
func formatTimestamp(ts string) string {
	if len(ts) >= 16 {
		// "2025-01-15T14:30:00.000Z" → "Jan 15 14:30"
		return ts[5:16]
	}
	return ts
}

// wrapText wraps text to the given width.
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if len(line) <= width {
			lines = append(lines, line)
			continue
		}
		for len(line) > width {
			// Find last space before width
			cut := width
			for i := width; i > 0; i-- {
				if line[i] == ' ' {
					cut = i
					break
				}
			}
			lines = append(lines, line[:cut])
			line = line[cut:]
			if len(line) > 0 && line[0] == ' ' {
				line = line[1:]
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
