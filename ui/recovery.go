package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/theme"
)

// RecoveryContent uses the generated roles for a condition and one next action.
// Details wrap in body ink; only a failure heading uses the dead role.
func RecoveryContent(condition, detail, action string, failed bool, width int) string {
	return recoveryContent(condition, detail, action, failed, width, theme.Styles())
}

func recoveryContent(condition, detail, action string, failed bool, width int, styles theme.StyleSet) string {
	if width < 1 {
		return ""
	}
	title := styles.Title
	if failed {
		title = styles.Error.Bold(true)
	}
	lines := []string{title.Width(width).Align(lipgloss.Center).Render(truncateStatusText(condition, width))}
	if detail != "" {
		lines = append(lines, styles.Body.Width(width).Align(lipgloss.Center).Render(sanitizeError(detail)))
	}
	if action != "" {
		lines = append(lines, "", styles.Body.Width(width).Align(lipgloss.Center).Render(action))
	}
	return strings.Join(lines, "\n")
}

// RecoveryScreen is unframed and exactly fills its allocation, including narrow terminals.
func RecoveryScreen(r layout.Rect, condition, detail, action string, failed bool) string {
	return recoveryScreen(r, condition, detail, action, failed, theme.Styles())
}

func recoveryScreen(r layout.Rect, condition, detail, action string, failed bool, styles theme.StyleSet) string {
	if r.Empty() {
		return ""
	}
	content := recoveryContent(condition, detail, action, failed, r.W, styles)
	if detail != "" && lipgloss.Height(content) > r.H {
		// Raw daemon errors can be much taller than a narrow terminal. Keep
		// the condition and next action visible; only the detail gives way.
		heading := recoveryContent(condition, "", "", failed, r.W, styles)
		body := styles.Body.Width(r.W).Align(lipgloss.Center)
		next := body.Render(action)
		budget := r.H - lipgloss.Height(heading)
		if action != "" {
			budget -= 1 + lipgloss.Height(next)
		}
		parts := []string{heading}
		if budget > 0 {
			details := strings.Split(body.Render(sanitizeError(detail)), "\n")
			if len(details) > budget {
				details = details[:budget]
			}
			parts = append(parts, strings.Join(details, "\n"))
		}
		if action != "" {
			parts = append(parts, "", next)
		}
		content = strings.Join(parts, "\n")
	}
	return layout.ClampToRect(styles.Body.Render(lipgloss.Place(r.W, r.H,
		lipgloss.Center, lipgloss.Center, content)), r)
}
