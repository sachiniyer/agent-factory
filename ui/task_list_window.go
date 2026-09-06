package ui

import "strings"

// fitTaskList keeps the heading, selected task and action footer visible as
// selected details expand. Small viewports surrender detail before identity.
func fitTaskList(text string, width, height, footer, selectedStart, selectedEnd int) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	const header = 2
	budget := height - header - footer
	if height <= 0 || len(lines) <= height || budget <= 0 {
		return fitBlockToSize(text, width, height, footer)
	}
	start := max(header, selectedEnd-budget)
	start = min(start, selectedStart)
	end := min(start+budget, len(lines)-footer)
	body := append([]string{}, lines[:header]...)
	body = append(body, lines[start:end]...)
	body = append(body, lines[len(lines)-footer:]...)
	return fitBlockToSize(strings.Join(body, "\n"), width, height, footer)
}
