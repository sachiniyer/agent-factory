package ui

import "strings"

// fitTaskListPlan windows the task list so the heading, selected task and
// action footer stay visible as selected details expand; small viewports
// surrender detail before identity. It returns the line plan rather than the
// text: for each line drawn, the index into strings.Split(text, "\n") of the
// line shown there, or moreLine for the overflow cue — the one window the text
// and the task manager's click zones are both cut from (#7175).
func fitTaskListPlan(text string, height, footer, selectedStart, selectedEnd int) []int {
	const header = 2
	n := len(strings.Split(strings.TrimSuffix(text, "\n"), "\n"))
	return fitWindowedPlan(n, height, header, footer, selectedStart, selectedEnd)
}
