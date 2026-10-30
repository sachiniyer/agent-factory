package app

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/charmbracelet/x/vt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// #6071: the scroll-margin reset homes the cursor, so the erase-below must not run
// from the top-left. Pin the exact bytes, then replay them over a screen holding
// prior output and the launch line: everything the shell drew survives and only
// what is below its cursor is erased.
func TestTerminalExitRestoreKeepsTheMainScreen(t *testing.T) {
	const want = "\x1b[?1003l\x1b[?1002l\x1b[?1000l" +
		"\x1b[?1015l\x1b[?1006l\x1b[?1005l" +
		"\x1b[?1004l\x1b[?2004l" +
		"\x1b[?1l\x1b>" +
		"\x1b[?7h" +
		"\x1b7\x1b[r\x1b8" +
		"\x1b[?1049l" +
		"\x1b7\x1b[r\x1b8" +
		"\x1b[0m" +
		"\x1b[?25h" +
		"\r\x1b[J"
	require.Equal(t, want, terminalExitRestore)

	// The terminal state at exit: bubbletea has already left the alt screen on a
	// normal quit ("main"), but a signal can arrive while still on it ("alt").
	for _, startOn := range []string{"main", "alt"} {
		t.Run(startOn, func(t *testing.T) {
			emu := vt.NewEmulator(40, 8)
			_, err := emu.WriteString("old output 1\r\nold output 2\r\n$ af\r\n$ ")
			require.NoError(t, err)
			// Stale attach residue below the shell's cursor.
			_, err = emu.WriteString("\x1b7\x1b[6;1Hstale attach frame\x1b8")
			require.NoError(t, err)
			before := emu.CursorPosition()
			if startOn == "alt" {
				_, err = emu.WriteString("\x1b[?1049h")
				require.NoError(t, err)
			}

			_, err = emu.WriteString(terminalExitRestore)
			require.NoError(t, err)

			got := emu.String()
			for _, keep := range []string{"old output 1", "old output 2", "$ af"} {
				assert.Contains(t, got, keep, "main-screen content above the cursor survives")
			}
			assert.NotContains(t, got, "stale attach frame", "the residue below the cursor is erased")
			assert.Equal(t, before.Y, emu.CursorPosition().Y, "the cursor stays on the shell's line")
		})
	}
}

// #6072: on a fatal signal the attach's own termios restore completes BEFORE the
// signal is re-raised, and the guard adds the escape sequences between them.
func TestExitOnSignalRestoresBeforeReRaising(t *testing.T) {
	h := &home{}
	h.tookTerminal.Store(true)
	var events []string
	var out bytes.Buffer
	exitOnSignal(h, syscall.SIGHUP, &out,
		func() { events = append(events, "restore-attached") },
		func(s syscall.Signal) {
			events = append(events, "kill")
			assert.Equal(t, terminalExitRestore, out.String(), "escape restore is written before the kill")
			assert.Equal(t, syscall.SIGHUP, s)
		})
	assert.Equal(t, []string{"restore-attached", "kill"}, events)

	events = nil
	exitOnSignal(h, os.Interrupt, &out, func() { events = append(events, "restore-attached") }, func(syscall.Signal) { events = append(events, "kill") })
	assert.Equal(t, []string{"restore-attached", "kill"}, events, "os.Interrupt is a syscall.Signal and also re-raises")
}
