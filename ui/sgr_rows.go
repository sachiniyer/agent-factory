package ui

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
)

// carrySGRAcrossRows makes every row of a pane capture start in the SGR state
// in effect at its first cell, and end with nothing left open (#7419).
//
// tmux's `capture-pane -e` is one continuous SGR stream. A line that soft-wraps
// has its state opened once, on its first row, and closed only where the line
// ends, so its continuation rows carry no SGR of their own. A row that ends in a
// hard newline is closed at its own end, so nothing leaks into the next row.
// The pane draws rows one at a time: lipgloss resets at every line end, and a
// scroll viewport can start mid-line. Re-opening the carried state at each row's
// start and closing it at the row's end therefore reproduces what the session's
// own terminal shows. It carries state across exactly the soft wraps, because
// those are the only row ends tmux leaves open.
//
// Rows that start and end with nothing open are returned byte-for-byte.
func carrySGRAcrossRows(content string) string {
	if !strings.Contains(content, "\x1b[") {
		return content
	}
	p := xansi.GetParser()
	defer xansi.PutParser(p)

	rows := strings.Split(content, "\n")
	var pen uv.Style
	for i, row := range rows {
		open := pen
		var state byte
		for rest := row; len(rest) > 0; {
			seq, _, n, newState := xansi.DecodeSequence(rest, state, p)
			if xansi.HasCsiPrefix(seq) && p.Command() == 'm' {
				uv.ReadStyle(p.Params(), &pen)
			}
			state = newState
			rest = rest[n:]
		}
		var prefix, suffix string
		if !open.IsZero() {
			prefix = open.String()
		}
		if !pen.IsZero() {
			suffix = xansi.ResetStyle
		}
		if prefix != "" || suffix != "" {
			rows[i] = prefix + row + suffix
		}
	}
	return strings.Join(rows, "\n")
}
