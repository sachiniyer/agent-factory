package task

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// claudeComposerRule matches one of the two full-width "─" rules that
	// enclose Claude Code's composer.
	claudeComposerRule = regexp.MustCompile(`^─{8,}$`)
	// claudeSpinnerLine matches the status line Claude Code draws above its
	// composer while a turn runs: a spinner glyph and a verb ending in "…",
	// followed by "(" and the elapsed time, e.g. "✻ Orbiting… (2m 14s · ↓ 10.1k
	// tokens)", or by nothing in the first second of a turn. A finished turn
	// leaves "✻ Baked for 10m 46s · done 10:54 PM", which has no "…". The verb is
	// not matched by name because Claude Code picks it at random and users can
	// configure their own.
	claudeSpinnerLine = regexp.MustCompile(`^[·✢✳✶✻✽*]\s+[^()]*…\s*(?:\(|$)`)
)

// claudeComposer finds the bottom-most Claude Code composer in the ANSI-stripped
// pane lines: a rule, a line that starts with "❯", any wrapped input lines, and
// a second rule. It returns the indexes of both rules.
//
// Only the last two rules count. A permission or question dialog replaces the
// composer with a box that has no such pair, and an older composer frame
// printed or quoted in the transcript above it must not stand in for the live
// one.
func claudeComposer(lines []string) (top, bottom int, ok bool) {
	var rules []int
	for i, line := range lines {
		if claudeComposerRule.MatchString(strings.TrimRight(line, " ")) {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return -1, -1, false
	}
	top, bottom = rules[len(rules)-2], rules[len(rules)-1]
	if top+1 < bottom && strings.HasPrefix(lines[top+1], "❯") {
		return top, bottom, true
	}
	return -1, -1, false
}

// isClaudeWorkingContent reports whether a Claude Code pane shows a turn in
// progress. It reads only the current frame: the footer below the composer, and
// the nearest line above the composer that starts in column 0. Claude Code
// indents its transcript under "●" and "⎿" markers, and puts tips and hints such
// as "⎿ Tip: …" or "◐ medium · /effort" between the status line and the
// composer, so the first unindented line above the composer is the status line
// when a turn runs. Transcript text further up cannot match, which keeps a
// finished session from reading as busy forever.
func isClaudeWorkingContent(content string) bool {
	lines := strings.Split(paneAnsiEscape.ReplaceAllString(content, ""), "\n")
	top, bottom, ok := claudeComposer(lines)
	if !ok {
		return false
	}
	for _, line := range lines[bottom+1:] {
		if escToInterruptHint.MatchString(line) {
			return true
		}
	}
	for i := top - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], " ")
		if line == "" {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(line); r == ' ' || r == '\t' {
			continue
		}
		return claudeSpinnerLine.MatchString(line)
	}
	return false
}
