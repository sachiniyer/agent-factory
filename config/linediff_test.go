package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedLineDiffReportsNothingForIdenticalText(t *testing.T) {
	assert.Empty(t, unifiedLineDiff("config.toml", "a = 1\n", "a = 1\n"))
}

// TestUnifiedLineDiffShowsAMovedKeyInContext is the shape `af config migrate`
// prints: the removed flat line and the added grouped one, each framed by
// enough unchanged lines to place them in the file.
func TestUnifiedLineDiffShowsAMovedKeyInContext(t *testing.T) {
	before := "# top\nschema_version = 1\nlisten_addr = 'x'\ndefault_program = 'codex'\nbranch_prefix = 'me/'\n"
	after := "# top\nschema_version = 1\ndefault_program = 'codex'\nbranch_prefix = 'me/'\n\n[network]\nlisten_addr = 'x'\n"

	diff := unifiedLineDiff("config.toml", before, after)
	require.NotEmpty(t, diff)
	assert.True(t, strings.HasPrefix(diff, "--- config.toml\n+++ config.toml\n"))
	assert.Contains(t, diff, "-listen_addr = 'x'")
	assert.Contains(t, diff, "+[network]")
	assert.Contains(t, diff, "+listen_addr = 'x'")
	assert.Contains(t, diff, " schema_version = 1", "unchanged lines are shown as context")
	assert.Contains(t, diff, "@@ -", "hunks are headed")

	// Applying the removals and additions must reconstruct `after` exactly —
	// the diff describes the edit that actually happened, not an approximation.
	var rebuilt []string
	for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "@@"):
		case strings.HasPrefix(line, "-"):
		default:
			rebuilt = append(rebuilt, line[1:])
		}
	}
	assert.Equal(t, splitDiffLines(after), rebuilt)
}

// TestUnifiedLineDiffCollapsesUnchangedRuns keeps a one-line change in a long
// file from reprinting the file.
func TestUnifiedLineDiffCollapsesUnchangedRuns(t *testing.T) {
	var before strings.Builder
	for i := 0; i < 60; i++ {
		before.WriteString("k")
		before.WriteString(strings.Repeat("x", i%3))
		before.WriteString(" = 1\n")
	}
	after := strings.Replace(before.String(), "k = 1\n", "k = 2\n", 1)

	diff := unifiedLineDiff("config.toml", before.String(), after)
	assert.Less(t, strings.Count(diff, "\n"), 12, "an isolated change prints a hunk, not the file")
	assert.Contains(t, diff, "-k = 1")
	assert.Contains(t, diff, "+k = 2")
}

// TestUnifiedLineDiffFallsBackPastTheBudget pins the coarse path: past the
// quadratic budget the middle is rendered as a wholesale replacement, which is
// still a correct diff rather than a wrong one.
func TestUnifiedLineDiffFallsBackPastTheBudget(t *testing.T) {
	before := make([]string, 0, 200)
	after := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		before = append(before, "a"+strings.Repeat("x", i))
		after = append(after, "b"+strings.Repeat("x", i))
	}
	edits := middleEdits(before, after)
	require.Len(t, edits, 400)
	assert.Equal(t, byte('-'), edits[0].op)
	assert.Equal(t, byte('+'), edits[200].op)
}
