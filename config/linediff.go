package config

import (
	"fmt"
	"strings"
)

// diffContextLines is how many unchanged lines frame each hunk. Two is enough
// to place a moved key in a config file without reprinting the file.
const diffContextLines = 2

// diffLineBudget caps the quadratic table below, in cells and independently in
// each dimension. A config file is tens of lines; anything past this is not a
// config a human hand-edited, and printing a whole-file replacement beats
// spending seconds and megabytes on a prettier rendering of it.
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
// what is left. Past diffLineBudget the table is skipped and the remaining
// middle is rendered as a wholesale replacement, which is still a correct
// diff, just a coarser one.
//
// No allocation here is sized from a sum of two lengths. The slices grow by
// append instead, and the one genuinely sized allocation — the LCS table — is
// reached only after both of its dimensions have been compared against a
// constant, so its size cannot be derived from an unbounded input length
// (CodeQL go/allocation-size-overflow, which fires on exactly that shape).
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

	var edits []diffEdit
	for _, line := range before[:prefix] {
		edits = append(edits, diffEdit{' ', line})
	}
	edits = append(edits, middleEdits(before[prefix:len(before)-suffix], after[prefix:len(after)-suffix])...)
	for _, line := range before[len(before)-suffix:] {
		edits = append(edits, diffEdit{' ', line})
	}
	return edits
}

// middleEdits diffs the changed middle, falling back to a wholesale
// replacement when the LCS table would exceed diffLineBudget cells.
func middleEdits(before, after []string) []diffEdit {
	rows, cols := len(before), len(after)
	// Each dimension is bounded against the constant on its own, not only via
	// the product: it is what makes the table's size below provably small
	// rather than "small because two other numbers multiply to something
	// small", for a reader and for the analyzer alike.
	if rows == 0 || cols == 0 || rows > diffLineBudget || cols > diffLineBudget || rows*cols > diffLineBudget {
		return replaceWholesale(before, after)
	}

	// cell(i, j) is the length of the longest common subsequence of before[i:]
	// and after[j:], in one flat table of (rows+1)×(cols+1) — both factors are
	// now at most diffLineBudget.
	stride := cols + 1
	lcs := make([]int, (rows+1)*stride)
	cell := func(i, j int) int { return lcs[i*stride+j] }
	for i := rows - 1; i >= 0; i-- {
		for j := cols - 1; j >= 0; j-- {
			if before[i] == after[j] {
				lcs[i*stride+j] = cell(i+1, j+1) + 1
				continue
			}
			lcs[i*stride+j] = max(cell(i+1, j), cell(i, j+1))
		}
	}

	var edits []diffEdit
	i, j := 0, 0
	for i < rows && j < cols {
		switch {
		case before[i] == after[j]:
			edits = append(edits, diffEdit{' ', before[i]})
			i, j = i+1, j+1
		case cell(i+1, j) >= cell(i, j+1):
			edits = append(edits, diffEdit{'-', before[i]})
			i++
		default:
			edits = append(edits, diffEdit{'+', after[j]})
			j++
		}
	}
	for ; i < rows; i++ {
		edits = append(edits, diffEdit{'-', before[i]})
	}
	for ; j < cols; j++ {
		edits = append(edits, diffEdit{'+', after[j]})
	}
	return edits
}

// replaceWholesale renders every before line as removed and every after line
// as added — the coarse but correct rendering used past the budget.
func replaceWholesale(before, after []string) []diffEdit {
	var edits []diffEdit
	for _, line := range before {
		edits = append(edits, diffEdit{'-', line})
	}
	for _, line := range after {
		edits = append(edits, diffEdit{'+', line})
	}
	return edits
}
