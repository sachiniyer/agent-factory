package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// #6063: the restore sequence is written only when an attach took the terminal,
// leaves the alt screen, resets the scroll region and cursor, and never clears
// the screen above the shell's cursor (#4785).
func TestRestoreTerminalAtExit(t *testing.T) {
	h := &home{}

	var none bytes.Buffer
	h.restoreTerminalAtExit(&none)
	assert.Empty(t, none.String(), "a run that never attached leaves the terminal exactly as before")

	h.tookTerminal.Store(true)
	var got bytes.Buffer
	h.restoreTerminalAtExit(&got)
	out := got.String()
	assert.Equal(t, terminalExitRestore, out)

	order := []string{"\x1b[?1002l", "\x1b[?2004l", "\x1b[r", "\x1b[?1049l", "\x1b[0m", "\x1b[?25h", "\r\x1b[J"}
	at := -1
	for _, seq := range order {
		i := strings.Index(out[at+1:], seq)
		if !assert.GreaterOrEqual(t, i, 0, "missing %q after the previous step", seq) {
			return
		}
		at += 1 + i
	}
	assert.NotContains(t, out, "\x1b[2J", "the whole main screen must not be cleared (#4785)")
	assert.NotContains(t, out, "\x1b[H", "the cursor stays where the shell left it")
	assert.True(t, strings.HasSuffix(out, "\r\x1b[J"), "erase below the cursor is the last step, on the main screen")
	assert.Less(t, strings.Index(out, "\x1b[?1049l"), strings.LastIndex(out, "\x1b[J"),
		"the erase happens on the main buffer, after leaving the alt screen")
}

// The attach entry points mark the terminal as taken before they release it.
func TestAttachMarksTheTerminalTaken(t *testing.T) {
	h := &home{}
	var out bytes.Buffer
	h.releaseTerminalToAttach(&out)
	assert.True(t, h.tookTerminal.Load())
}
