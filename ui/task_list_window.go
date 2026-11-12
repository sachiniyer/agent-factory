package ui

import "strings"

// fitTaskListPlan windows the task list so the heading, selected task and
// action footer stay visible as selected details expand; small viewports
// surrender detail before identity. It returns the line plan rather than the
// text: for each line drawn, the index into strings.Split(text, "\n") of the
// line shown there, or moreLine for the overflow cue — the one window the text
// and the task manager's click zones are both cut from (#7175).
func fitTaskListPlan(text string, height, footer, selectedStart, selectedEnd int) []int {
	n := len(strings.Split(strings.TrimSuffix(text, "\n"), "\n"))
	const header = 2
	budget := height - header - footer
	if height <= 0 || n <= height || budget <= 0 {
		return fitBlockPlan(len(strings.Split(text, "\n")), height, footer)
	}
	start := max(header, selectedEnd-budget)
	start = min(start, selectedStart)
	end := min(start+budget, n-footer)
	body := linesThrough(0, header)
	body = append(body, linesThrough(start, end)...)
	body = append(body, linesThrough(n-footer, n)...)
	plan := fitBlockPlan(len(body), height, footer)
	for i, k := range plan {
		if k != moreLine {
			plan[i] = body[k]
		}
	}
	return plan
}
