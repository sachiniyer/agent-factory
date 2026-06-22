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
// in the numbered prefix width (#871). indexWidth is pinned to 5 to model a
// list large enough to contain idx=10000: every row in that list pads to 5
// digits, so all indices must share one indent (#923).
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
	r.setWidth(60)   // wide enough to render the full PR line
	r.indexWidth = 5 // model a list whose largest index is 10000 (5 digits)

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
	for _, idx := range []int{9, 10, 11, 99, 100, 101, 999, 1000, 1001, 9999, 10000} {
		got := prLineIndent(t, idx, &spin)
		require.Equalf(t, base, got,
			"PR line indent at idx=%d (%d) must match idx=1 (%d); prefix width drifted",
			idx, got, base)
	}
}

// renderRow renders an instance at the given 1-based display index and returns
// the ANSI-stripped rendered output.
func renderRow(t *testing.T, idx int, spin *spinner.Model) string {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	r := &InstanceRenderer{spinner: spin}
	r.setWidth(60) // wide enough to render the full index prefix and title

	out := r.Render(inst, idx, false, false)
	return ansiEscape.ReplaceAllString(out, "")
}

// TestInstanceRendererPrefixContent guards against the regression in #939: the
// trim-loop introduced by #923 held the prefix width constant by deleting the
// rightmost char per digit tier, which corrupted content — the dot vanished at
// idx≥100 and a digit at idx≥1000 ("1000" rendered as "100"). The fixed-width
// pad must show the full index followed by a dot at every magnitude.
func TestInstanceRendererPrefixContent(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	for _, tc := range []struct {
		idx  int
		want string
	}{
		{1, "1. "},
		{9, "9. "},
		{99, "99. "},
		{100, "100. "},
		{999, "999. "},
		{1000, "1000. "},
		{9999, "9999. "},
		{10000, "10000. "},
	} {
		out := renderRow(t, tc.idx, &spin)
		require.Containsf(t, out, tc.want,
			"rendered row for idx=%d must contain %q (full number + dot), got:\n%s",
			tc.idx, tc.want, out)
	}
}

// TestInstanceRendererPrefixBoundaries explicitly pins the digit-tier
// boundaries that shift the prefix width: 9→10 (originally handled), 99→100
// (the #871 fix), and 999→1000 / 9999→10000 (the #923 fix). Adjacent rows that
// straddle a boundary must align. The app generates titles up to 10000, so the
// 4-digit boundary is reachable.
func TestInstanceRendererPrefixBoundaries(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	require.Equal(t, prLineIndent(t, 9, &spin), prLineIndent(t, 10, &spin),
		"rows 9 and 10 must share the same indent")
	require.Equal(t, prLineIndent(t, 99, &spin), prLineIndent(t, 100, &spin),
		"rows 99 and 100 must share the same indent")
	require.Equal(t, prLineIndent(t, 999, &spin), prLineIndent(t, 1000, &spin),
		"rows 999 and 1000 must share the same indent")
	require.Equal(t, prLineIndent(t, 9999, &spin), prLineIndent(t, 10000, &spin),
		"rows 9999 and 10000 must share the same indent")
}
