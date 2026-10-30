package overlay

import (
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// pasteRunes builds the bracketed-paste KeyRunes shape bubbletea delivers: one
// message, Paste: true, runes preserved verbatim including control runes.
func pasteRunes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(s)}
}

// TestSearchOverlayPasteSanitizesQueryAndStillMatches is the search-overlay half
// of the #2640 paste hardening. A branch/file name copied out of terminal output
// commonly carries a trailing newline, and a bracketed paste delivers it as a
// single KeyRunes message whose runes are preserved verbatim (including
// '\n'/'\r'/'\t'). Before the fix, the control runes entered the query verbatim:
// no sanitized title could match, so the result list collapsed to "No matches
// found" with no visible cause. The shared ui.EditLine now strips control runes,
// so a pasted query stays a single printable line and still matches.
func TestSearchOverlayPasteSanitizesQueryAndStillMatches(t *testing.T) {
	instances := []*session.Instance{{Title: "alphabeta"}}
	s := NewSearchOverlay(instances)
	s.SetWidth(60)

	require.False(t, s.HandleKeyPress(pasteRunes("alpha\nbeta")),
		"a paste edits the query; it does not close the overlay")

	require.Equal(t, "alphabeta", s.query,
		"the pasted control runes must be stripped, leaving the printable query")
	for _, r := range s.query {
		assert.False(t, unicode.IsControl(r), "no control rune may remain in the query: %U", r)
	}

	matches := s.ResultInstances()
	require.Len(t, matches, 1, "the sanitized query must still match the title")
	assert.Equal(t, "alphabeta", matches[0].Title)

	out := renderedText(s.Render())
	assert.Contains(t, out, "alphabeta", "the matched result row must render")
	assert.NotContains(t, out, "No matches found",
		"a sanitized paste must not collapse the result list:\n%s", out)
}

// TestSearchOverlayPasteKeepsFrameSingleLine pins the geometry half of the bug:
// the render plan budgets the query as one logical line and RegisterZones offsets
// result rows from firstResultLine, which counts logical lines. An embedded '\n'
// split the query into extra physical terminal lines, so the dialog grew past its
// budgeted height and every result row's pointer zone landed one row too high per
// newline. With control runes stripped at the shared helper, a pasted query is
// byte-for-byte the same single-line query as its typed counterpart, so the frame
// is the same height and the registered zone sits on the row that renders it.
func TestSearchOverlayPasteKeepsFrameSingleLineAndZonesAligned(t *testing.T) {
	instances := []*session.Instance{{Title: "alphabeta"}}

	// The control query: type "alphabeta" one rune at a time.
	clean := NewSearchOverlay(instances)
	clean.SetWidth(60)
	typeInto(clean.HandleKeyPress, "alphabeta")
	require.Equal(t, "alphabeta", clean.query)

	// The same query delivered as ONE bracketed-paste KeyRunes message carrying
	// a control rune verbatim — the exact shape that broke the frame.
	pasted := NewSearchOverlay(instances)
	pasted.SetWidth(60)
	require.False(t, pasted.HandleKeyPress(pasteRunes("alpha\nbeta")))
	require.Equal(t, "alphabeta", pasted.query, "the paste must sanitize to the typed query")

	cleanLines := strings.Split(clean.Render(), "\n")
	pastedLines := strings.Split(pasted.Render(), "\n")
	require.Len(t, pastedLines, len(cleanLines),
		"a pasted control-rune query must not add physical lines beyond the typed query:\n%s",
		pasted.Render())

	// The query row must stay one physical line: no rendered row may carry an
	// embedded newline, so the layout the render plan budgeted is preserved.
	for i, line := range pastedLines {
		assert.NotContains(t, xansi.Strip(line), "\n",
			"rendered row %d must not contain an embedded newline:\n%s", i, pasted.Render())
	}

	// Pointer zones stay aligned: the matched result row's zone must sit on the
	// rendered line that contains its title. With the bug, firstResultLine
	// counted one logical line while the query consumed an extra physical line,
	// so the zone Y missed the row by the number of embedded newlines.
	reg := zones.NewRegistry()
	origin := layout.Point{X: 5, Y: 2}
	pasted.RegisterZones(reg, origin)

	row, ok := reg.Find(zones.OverlaySearchRow(0))
	require.True(t, ok, "the matched result row must register a clickable zone; got %v", reg.IDs())
	require.Less(t, row.Y-origin.Y, len(pastedLines), "zone Y must land inside the frame")
	renderedLine := xansi.Strip(pastedLines[row.Y-origin.Y])
	assert.Contains(t, renderedLine, "alphabeta",
		"the row zone must land on the line rendering the result title, not shifted by control runes:\n%s",
		pasted.Render())
}
