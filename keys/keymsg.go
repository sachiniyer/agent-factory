package keys

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// KeyMsgForString synthesizes the tea.KeyMsg whose String() is s — the inverse
// of tea.KeyMsg.String() over every key a [keys] binding can advertise. It is
// the translator the status-bar hint click path needs: a hint registers a click
// zone with the binding's effective primary key, and clicking it must "press"
// that same key, so the click feeds handleKeyPress the same event the keyboard
// path received.
//
// The keyboard path resolves an action through GlobalKeyStringsMap[msg.String()];
// KeyMsgForString therefore rebuilds the event whose String() round-trips back
// to s, so a click re-derives the same (override-aware) action the keyboard
// press would — including after a [keys] rebind to any valid multi-character
// key the click dispatcher's closed switch never enumerated (ctrl+b, f1,
// pgup, alt+x, …). The few ctrl+letter rebinds Bubble Tea collapses to a name
// (ctrl+i → "tab", ctrl+m → "enter", ctrl+[ → "esc", ctrl+? → "backspace")
// round-trip to that canonical name rather than the raw input, which keeps the
// click on the same dispatch path the keyboard takes for those too.
//
// ok is false for strings with no single-key equivalent.
func KeyMsgForString(s string) (tea.KeyMsg, bool) {
	alt := false
	rest := s
	if strings.HasPrefix(rest, "alt+") {
		alt = true
		rest = rest[len("alt+"):]
	}
	if t, ok := keyTypeForString(rest); ok {
		return tea.KeyMsg{Type: t, Alt: alt}, true
	}
	if r := []rune(rest); len(r) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: r, Alt: alt}, true
	}
	return tea.KeyMsg{}, false
}

// keyTypeForString maps the non-alt portion of a key string to its KeyType. It
// prefers the reverse of Bubble Tea's keyNames (so the canonical spelling
// round-trips exactly) and falls back to the control code for ctrl+<rune>,
// whose canonical spelling may be a different name.
func keyTypeForString(rest string) (tea.KeyType, bool) {
	if t, ok := keyStringTypes[rest]; ok {
		return t, true
	}
	if body, ok := strings.CutPrefix(rest, "ctrl+"); ok {
		if r := []rune(body); len(r) == 1 {
			return ctrlKeyCode(r[0])
		}
	}
	return 0, false
}

// keyStringTypes is the reverse of Bubble Tea's keyNames: the canonical
// tea.KeyMsg.String() spelling of every non-rune special key mapped to the
// KeyType that produces it. Built once at init from the exported KeyType
// constants so it cannot drift from Bubble Tea's own spellings.
var keyStringTypes = func() map[string]tea.KeyType {
	m := make(map[string]tea.KeyType, 128)
	for _, t := range specialKeyTypes {
		s := tea.KeyMsg{Type: t}.String()
		if s == "" || s == "runes" {
			continue
		}
		if _, dup := m[s]; !dup {
			m[s] = t
		}
	}
	return m
}()

// specialKeyTypes is every non-rune KeyType Bubble Tea can emit for a single
// physical key — control codes, named keys, and their ctrl/shift variants —
// keyed into keyStringTypes by their String() spelling. The word spellings of
// the control codes Bubble Tea names (KeyTab/KeyEnter/KeyEsc/KeyBackspace for
// ctrl+i/m/[/?) stand in for those values; the ctrl+rune fallback covers the
// raw spellings.
var specialKeyTypes = []tea.KeyType{
	// Control codes.
	tea.KeyCtrlAt,
	tea.KeyCtrlA, tea.KeyCtrlB, tea.KeyCtrlC, tea.KeyCtrlD, tea.KeyCtrlE,
	tea.KeyCtrlF, tea.KeyCtrlG, tea.KeyCtrlH, tea.KeyCtrlJ, tea.KeyCtrlK,
	tea.KeyCtrlL, tea.KeyCtrlN, tea.KeyCtrlO, tea.KeyCtrlP, tea.KeyCtrlQ,
	tea.KeyCtrlR, tea.KeyCtrlS, tea.KeyCtrlT, tea.KeyCtrlU, tea.KeyCtrlV,
	tea.KeyCtrlW, tea.KeyCtrlX, tea.KeyCtrlY, tea.KeyCtrlZ,
	tea.KeyCtrlBackslash, tea.KeyCtrlCloseBracket, tea.KeyCtrlCaret, tea.KeyCtrlUnderscore,
	// Word spellings of the control codes Bubble Tea names.
	tea.KeyTab, tea.KeyEnter, tea.KeyEsc, tea.KeyBackspace,
	// Named keys and their ctrl/shift variants.
	tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight, tea.KeySpace, tea.KeyShiftTab,
	tea.KeyHome, tea.KeyEnd, tea.KeyCtrlHome, tea.KeyCtrlEnd,
	tea.KeyShiftHome, tea.KeyShiftEnd, tea.KeyCtrlShiftHome, tea.KeyCtrlShiftEnd,
	tea.KeyPgUp, tea.KeyPgDown, tea.KeyCtrlPgUp, tea.KeyCtrlPgDown,
	tea.KeyDelete, tea.KeyInsert,
	tea.KeyCtrlUp, tea.KeyCtrlDown, tea.KeyCtrlLeft, tea.KeyCtrlRight,
	tea.KeyShiftUp, tea.KeyShiftDown, tea.KeyShiftLeft, tea.KeyShiftRight,
	tea.KeyCtrlShiftUp, tea.KeyCtrlShiftDown, tea.KeyCtrlShiftLeft, tea.KeyCtrlShiftRight,
	tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4, tea.KeyF5, tea.KeyF6,
	tea.KeyF7, tea.KeyF8, tea.KeyF9, tea.KeyF10, tea.KeyF11, tea.KeyF12,
	tea.KeyF13, tea.KeyF14, tea.KeyF15, tea.KeyF16,
	tea.KeyF17, tea.KeyF18, tea.KeyF19, tea.KeyF20,
}

// ctrlKeyCode maps a rune to the control-key KeyType Bubble Tea emits for
// ctrl+<rune>, mirroring its KeyCtrl* aliases; ok is false for runes ctrl does
// not turn into a control code (digits, etc.).
func ctrlKeyCode(r rune) (tea.KeyType, bool) {
	switch {
	case 'a' <= r && r <= 'z':
		return tea.KeyType(r - 'a' + 1), true
	case 'A' <= r && r <= 'Z':
		return tea.KeyType(r - 'A' + 1), true
	case r == '@':
		return tea.KeyCtrlAt, true
	case r == '[':
		return tea.KeyCtrlOpenBracket, true
	case r == '\\':
		return tea.KeyCtrlBackslash, true
	case r == ']':
		return tea.KeyCtrlCloseBracket, true
	case r == '^':
		return tea.KeyCtrlCaret, true
	case r == '_':
		return tea.KeyCtrlUnderscore, true
	case r == '?':
		return tea.KeyCtrlQuestionMark, true
	}
	return 0, false
}
