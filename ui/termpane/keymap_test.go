package termpane

import (
	"io"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emuBytes runs f against a bare emulator (after feeding it prep — e.g. mode-
// setting sequences the inner application would emit) and returns every byte
// f pushed into the emulator's input pipe. The pipe is synchronous, so a
// dedicated reader must be running before f writes; the sentinel written
// afterwards proves the capture drained completely — including the
// nothing-was-written case the ignore contract depends on.
func emuBytes(t *testing.T, prep string, f func(emu *vt.Emulator)) string {
	t.Helper()
	emu := vt.NewEmulator(80, 24)
	// Close the input pipe so the reader goroutine below exits — via the
	// pipe's own writer end, NOT emu.Close(): the pinned x/vt Close sets an
	// unsynchronized flag that races the reader's concurrent Read (the same
	// upstream race TermPane.Close designs around).
	t.Cleanup(func() {
		if pw, ok := emu.InputPipe().(*io.PipeWriter); ok {
			_ = pw.CloseWithError(io.EOF)
		}
	})
	if prep != "" {
		_, err := emu.Write([]byte(prep))
		require.NoError(t, err)
	}

	var mu sync.Mutex
	var got []byte
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := emu.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	f(emu)

	const sentinel = "\x00SENTINEL\x00"
	emu.SendText(sentinel)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= len(sentinel) && string(got[len(got)-len(sentinel):]) == sentinel
	}, 5*time.Second, time.Millisecond, "input pipe never drained")

	mu.Lock()
	defer mu.Unlock()
	return string(got[:len(got)-len(sentinel)])
}

func runes(s string, alt bool) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Alt: alt}
}

func typed(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func typedAlt(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t, Alt: true} }

// TestKeyTranslationTable pins the tea.KeyMsg → PTY-bytes mapping (#1089
// PR 2): the exact sequences vim/less/agent CLIs will receive. prep feeds the
// emulator mode-setting sequences first, standing in for the inner
// application (DECCKM application cursor keys, bracketed paste).
func TestKeyTranslationTable(t *testing.T) {
	const (
		appCursorKeys  = "\x1b[?1h"
		bracketedPaste = "\x1b[?2004h"
	)
	cases := []struct {
		name string
		prep string
		msg  tea.KeyMsg
		want string
	}{
		// Literal text.
		{name: "rune a", msg: runes("a", false), want: "a"},
		{name: "rune A", msg: runes("A", false), want: "A"},
		{name: "multi-rune with accents", msg: runes("héllo", false), want: "héllo"},
		{name: "alt+x legacy ESC prefix", msg: runes("x", true), want: "\x1bx"},
		{name: "space", msg: typed(tea.KeySpace), want: " "},

		// Simple specials.
		{name: "enter", msg: typed(tea.KeyEnter), want: "\r"},
		{name: "tab forwards (not host focus)", msg: typed(tea.KeyTab), want: "\t"},
		{name: "shift+tab", msg: typed(tea.KeyShiftTab), want: "\x1b[Z"},
		{name: "backspace", msg: typed(tea.KeyBackspace), want: "\x7f"},
		{name: "escape", msg: typed(tea.KeyEscape), want: "\x1b"},

		// Arrows honor DECCKM (vim sets application cursor keys).
		{name: "up normal", msg: typed(tea.KeyUp), want: "\x1b[A"},
		{name: "up application mode", prep: appCursorKeys, msg: typed(tea.KeyUp), want: "\x1bOA"},
		{name: "left normal", msg: typed(tea.KeyLeft), want: "\x1b[D"},

		// Nav block.
		{name: "home", msg: typed(tea.KeyHome), want: "\x1b[H"},
		{name: "end", msg: typed(tea.KeyEnd), want: "\x1b[F"},
		{name: "pgup", msg: typed(tea.KeyPgUp), want: "\x1b[5~"},
		{name: "pgdown", msg: typed(tea.KeyPgDown), want: "\x1b[6~"},
		{name: "insert", msg: typed(tea.KeyInsert), want: "\x1b[2~"},
		{name: "delete", msg: typed(tea.KeyDelete), want: "\x1b[3~"},

		// F-keys.
		{name: "F1", msg: typed(tea.KeyF1), want: "\x1bOP"},
		{name: "F5", msg: typed(tea.KeyF5), want: "\x1b[15~"},
		{name: "F12", msg: typed(tea.KeyF12), want: "\x1b[24~"},

		// Control keys.
		{name: "ctrl+a", msg: typed(tea.KeyCtrlA), want: "\x01"},
		{name: "ctrl+c", msg: typed(tea.KeyCtrlC), want: "\x03"},
		{name: "ctrl+w (full-screen detach key forwards while interactive)", msg: typed(tea.KeyCtrlW), want: "\x17"},
		{name: "ctrl+z", msg: typed(tea.KeyCtrlZ), want: "\x1a"},
		{name: "ctrl+@", msg: typed(tea.KeyCtrlAt), want: "\x00"},
		{name: "ctrl+backslash", msg: typed(tea.KeyCtrlBackslash), want: "\x1c"},
		{name: "ctrl+underscore", msg: typed(tea.KeyCtrlUnderscore), want: "\x1f"},
		{name: "alt+ctrl+b", msg: typedAlt(tea.KeyCtrlB), want: "\x1b\x02"},

		// Modifier+navigation family: pre-encoded xterm CSI, the sequences
		// the pinned x/vt SendKey drops (the spike's swallowed-Ctrl-Up
		// gotcha). shift=1 alt=2 ctrl=4, param = 1+bits.
		{name: "ctrl+up", msg: typed(tea.KeyCtrlUp), want: "\x1b[1;5A"},
		{name: "ctrl+down", msg: typed(tea.KeyCtrlDown), want: "\x1b[1;5B"},
		{name: "shift+down", msg: typed(tea.KeyShiftDown), want: "\x1b[1;2B"},
		{name: "shift+right", msg: typed(tea.KeyShiftRight), want: "\x1b[1;2C"},
		{name: "ctrl+shift+left", msg: typed(tea.KeyCtrlShiftLeft), want: "\x1b[1;6D"},
		{name: "alt+up", msg: typedAlt(tea.KeyUp), want: "\x1b[1;3A"},
		{name: "alt+ctrl+right", msg: typedAlt(tea.KeyCtrlRight), want: "\x1b[1;7C"},
		{name: "shift+home", msg: typed(tea.KeyShiftHome), want: "\x1b[1;2H"},
		{name: "ctrl+end", msg: typed(tea.KeyCtrlEnd), want: "\x1b[1;5F"},
		{name: "ctrl+shift+home", msg: typed(tea.KeyCtrlShiftHome), want: "\x1b[1;6H"},
		{name: "ctrl+pgup", msg: typed(tea.KeyCtrlPgUp), want: "\x1b[5;5~"},
		{name: "ctrl+pgdown", msg: typed(tea.KeyCtrlPgDown), want: "\x1b[6;5~"},
		{name: "alt+delete", msg: typedAlt(tea.KeyDelete), want: "\x1b[3;3~"},
		{name: "alt+insert", msg: typedAlt(tea.KeyInsert), want: "\x1b[2;3~"},
		// Modified arrows are mode-INDEPENDENT: DECCKM must not change them.
		{name: "ctrl+up ignores DECCKM", prep: appCursorKeys, msg: typed(tea.KeyCtrlUp), want: "\x1b[1;5A"},

		// Paste rides the emulator's bracketed-paste awareness.
		{name: "paste plain", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi there"), Paste: true}, want: "hi there"},
		{name: "paste bracketed", prep: bracketedPaste,
			msg:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi there"), Paste: true},
			want: "\x1b[200~hi there\x1b[201~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := emuBytes(t, tc.prep, func(emu *vt.Emulator) {
				require.True(t, forwardKey(emu, tc.msg), "key must be encodable")
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestUncoveredKeysAreIgnoredNotGuessed pins the other half of the #1089
// input contract: a key with no safe encoding produces NO bytes — never a
// wrong sequence — and reports itself ignored. ctrl+] is in this set as a
// tripwire: it is the host's only reserved key while interactive and the app
// router never forwards it, so an encoding appearing here would silently
// mask a routing bug.
func TestUncoveredKeysAreIgnoredNotGuessed(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.KeyMsg
	}{
		{name: "F13", msg: typed(tea.KeyF13)},
		{name: "F20", msg: typed(tea.KeyF20)},
		{name: "ctrl+]", msg: typed(tea.KeyCtrlCloseBracket)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := emuBytes(t, "", func(emu *vt.Emulator) {
				assert.False(t, forwardKey(emu, tc.msg), "key must report ignored")
			})
			assert.Empty(t, got, "an ignored key must emit no bytes")
		})
	}
}

// TestSendKeyReachesThePTY drives the full TermPane path: encoded keystrokes
// must arrive at the attached process. The scripted PTY runs cat with
// terminal echo on, so forwarded bytes come straight back through the
// PTY → emulator pump and land in the grid.
func TestSendKeyReachesThePTY(t *testing.T) {
	tp := startScript(t, "cat", 40, 6)
	for _, r := range "typed-1089" {
		require.True(t, tp.SendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
	}
	waitForRender(t, tp, 40, 6, "typed-1089")
}

// TestSendKeyNeverBlocksAfterClientDeath pins the pump-drain contract: the
// emulator's input pipe is unbuffered, so if its pump exited when the attach
// client died, the SECOND post-death SendKey would block the bubbletea event
// loop forever (the 100ms reaping tick never getting to run — a frozen TUI).
func TestSendKeyNeverBlocksAfterClientDeath(t *testing.T) {
	tp := startScript(t, "printf gone", 20, 4)
	select {
	case <-tp.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("scripted client never exited")
	}

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < 50; i++ {
			tp.SendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		}
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("SendKey blocked after the attach client died")
	}
	require.NoError(t, tp.Close())

	// And after Close (pipe torn down) SendKey still must not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tp.SendKey(tea.KeyMsg{Type: tea.KeyEnter})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SendKey blocked after Close")
	}
}
