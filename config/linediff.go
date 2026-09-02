package config

import (
	"fmt"
	"strings"
)

// diffContextLines is how many unchanged lines frame each hunk. Two is enough
// to place a moved key in a config file without reprinting the file.
const diffContextLines = 2

// diffLineBudget caps the quadratic table below. A config file is tens of
// lines; anything past this is not a config a human hand-edited, and printing a
// whole-file replacement beats spending seconds and megabytes on a prettier
// rendering of it.
const diffLineBudget = 2000

// unifiedLineDiff renders a unified diff of before → after, labelled with path.
// `af config migrate` shows it so the reader sees the exact edit that landed in
// their file rather than a summary of it — the file is theirs, and it was just
// rewritten under them.
//
// Returns "" when the two texts are identical.
func unifiedLineDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	beforeLines, afterLines := splitDiffLines(before), splitDiffLines(after)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", path, path)
	for _, hunk := range diffHunks(beforeLines, afterLines) {
		out.WriteString(hunk)
	}
	return out.String()
}

// splitDiffLines splits text into lines, dropping the empty element a trailing
// newline produces so a final "\n" is not rendered as a changed blank line.
func splitDiffLines(text string) []string {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// diffEdit is one rendered line: ' ' context, '-' removed, '+' added.
type diffEdit struct {
	op   byte
	text string
}

// diffHunks renders the edit script as unified hunks with diffContextLines of
// context, collapsing runs of unchanged lines between them.
func diffHunks(before, after []string) []string {
	edits := diffEdits(before, after)
	// Mark every line within context distance of a change as printable.
	keep := make([]bool, len(edits))
	for i, edit := range edits {
		if edit.op == ' ' {
			continue
		}
		for j := max(0, i-diffContextLines); j < min(len(edits), i+diffContextLines+1); j++ {
			keep[j] = true
		}
	}
	var hunks []string
	var current strings.Builder
	beforeLine, afterLine := 1, 1
	hunkBefore, hunkAfter, countBefore, countAfter := 0, 0, 0, 0
	flush := func() {
		if current.Len() == 0 {
			return
		}
		hunks = append(hunks, fmt.Sprintf("@@ -%d,%d +%d,%d @@\n%s", hunkBefore, countBefore, hunkAfter, countAfter, current.String()))
		current.Reset()
		countBefore, countAfter = 0, 0
	}
	for i, edit := range edits {
		if !keep[i] {
			flush()
		} else {
			if current.Len() == 0 {
				hunkBefore, hunkAfter = beforeLine, afterLine
			}
			current.WriteByte(edit.op)
			current.WriteString(edit.text)
			current.WriteByte('\n')
			if edit.op != '+' {
				countBefore++
			}
			if edit.op != '-' {
				countAfter++
			}
		}
		if edit.op != '+' {
			beforeLine++
		}
		if edit.op != '-' {
			afterLine++
		}
	}
	flush()
	return hunks
}

// diffEdits produces the edit script for before → after.
//
// It trims the common prefix and suffix first — for a key relocation that is
// almost the whole file — and runs a longest-common-subsequence table over
// what is left. Past diffLineBudget cells the table is skipped and the
// remaining middle is rendered as a wholesale replacement, which is still a
// correct diff, just a coarser one.
func diffEdits(before, after []string) []diffEdit {
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix &&
		before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	midBefore := before[prefix : len(before)-suffix]
	midAfter := after[prefix : len(after)-suffix]

	edits := make([]diffEdit, 0, len(before)+len(after))
	for _, line := range before[:prefix] {
		edits = append(edits, diffEdit{' ', line})
	}
	edits = append(edits, middleEdits(midBefore, midAfter)...)
	for _, line := range before[len(before)-suffix:] {
		edits = append(edits, diffEdit{' ', line})
	}
	return edits
}

func middleEdits(before, after []string) []diffEdit {
	if len(before) == 0 || len(after) == 0 || len(before)*len(after) > diffLineBudget {
		edits := make([]diffEdit, 0, len(before)+len(after))
		for _, line := range before {
			edits = append(edits, diffEdit{'-', line})
		}
		for _, line := range after {
			edits = append(edits, diffEdit{'+', line})
		}
		return edits
	}

	// lcs[i][j] is the length of the longest common subsequence of before[i:]
	// and after[j:].
	lcs := make([][]int, len(before)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(after)+1)
	}
	for i := len(before) - 1; i >= 0; i-- {
		for j := len(after) - 1; j >= 0; j-- {
			if before[i] == after[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
		}
	}

	edits := make([]diffEdit, 0, len(before)+len(after))
	i, j := 0, 0
	for i < len(before) && j < len(after) {
		switch {
		case before[i] == after[j]:
			edits = append(edits, diffEdit{' ', before[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			edits = append(edits, diffEdit{'-', before[i]})
			i++
		default:
			edits = append(edits, diffEdit{'+', after[j]})
			j++
		}
	}
	for ; i < len(before); i++ {
		edits = append(edits, diffEdit{'-', before[i]})
	}
	for ; j < len(after); j++ {
		edits = append(edits, diffEdit{'+', after[j]})
	}
	return edits
}
