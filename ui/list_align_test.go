package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// prLineIndent renders an instance at the given 1-based display index and
// returns the number of leading whitespace cells in front of the "PR #" text.
// The PR line is indented by strings.Repeat(" ", len(prefix)) plus a constant
// style padding, so any idx-dependent change in the indent reflects a change
// in the numbered prefix width (#871).
func prLineIndent(t *testing.T, idx int, spin *spinner.Model) int {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	inst.SetPRInfo(&git.PRInfo{Number: 42, Title: "do a thing", State: "OPEN"})

	r := &InstanceRenderer{spinner: spin}
	r.setWidth(60) // wide enough to render the full PR line

	out := r.Render(inst, idx, false, false)
	for _, line := range strings.Split(out, "\n") {
		clean := ansiEscape.ReplaceAllString(line, "")
		if pos := strings.Index(clean, "PR #"); pos >= 0 {
			return len([]rune(clean[:pos]))
		}
	}
	t.Fatalf("no PR line found in rendered output for idx=%d:\n%s", idx, out)
	return -1
}

// TestInstanceRendererPrefixAlignment guards against the regression in #871:
// the numbered prefix width must stay constant as the index grows so adjacent
// visible rows keep the same branch/PR indentation. Before the fix the prefix
// grew by a cell at the 99→100 boundary (and the 9→10 boundary already worked).
func TestInstanceRendererPrefixAlignment(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	base := prLineIndent(t, 1, &spin)
	for _, idx := range []int{9, 10, 11, 99, 100, 101, 999} {
		got := prLineIndent(t, idx, &spin)
		require.Equalf(t, base, got,
			"PR line indent at idx=%d (%d) must match idx=1 (%d); prefix width drifted",
			idx, got, base)
	}
}

// TestInstanceRendererPrefixBoundaries explicitly pins the two digit-tier
// boundaries that shift the prefix width: 9→10 (already handled) and 99→100
// (the #871 fix). Adjacent rows that straddle a boundary must align.
func TestInstanceRendererPrefixBoundaries(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	require.Equal(t, prLineIndent(t, 9, &spin), prLineIndent(t, 10, &spin),
		"rows 9 and 10 must share the same indent")
	require.Equal(t, prLineIndent(t, 99, &spin), prLineIndent(t, 100, &spin),
		"rows 99 and 100 must share the same indent")
}
