// Package terminal defines terminal state shared by PTY producers and clients.
package terminal

import "strings"

// Modes are the terminal modes that determine which side owns scrolling and
// how mouse input must be encoded. They are a snapshot of the child terminal,
// not AF configuration: applications may change them at runtime.
type Modes struct {
	AlternateScreen bool `json:"alternate_screen"`
	MouseTracking   bool `json:"mouse_tracking"`
	MouseStandard   bool `json:"mouse_standard"`
	MouseButton     bool `json:"mouse_button"`
	MouseAll        bool `json:"mouse_all"`
	MouseUTF8       bool `json:"mouse_utf8"`
	MouseSGR        bool `json:"mouse_sgr"`
}

// MouseTrackingEnabled reports whether the child requested mouse events. The
// UTF-8 and SGR flags only select an encoding; neither claims the mouse alone.
func (m Modes) MouseTrackingEnabled() bool {
	return m.MouseTracking || m.MouseStandard || m.MouseButton || m.MouseAll
}

// RestoreSequence returns DEC mode bytes that replace, rather than merge with,
// a client's current ownership modes. A fresh subscriber did not observe the
// application's earlier DECSETs, while a repaint after a lost replay gap may
// have stale modes; reset-first makes the snapshot authoritative in both cases.
func (m Modes) RestoreSequence() []byte {
	var b strings.Builder
	// Normalize the alternate buffer before selecting the captured one. This is
	// idempotent for a fresh emulator and also drops a stale alternate buffer on
	// a reconnect repaint before the captured grid is installed.
	b.WriteString("\x1b[?1049l")
	// tmux exposes exactly these mutually-exclusive tracking modes plus the two
	// independent encoding flags. Reset every one before applying the snapshot.
	b.WriteString("\x1b[?9l\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l")
	if m.AlternateScreen {
		b.WriteString("\x1b[?1049h")
	}
	if m.MouseStandard {
		b.WriteString("\x1b[?1000h")
	}
	if m.MouseButton {
		b.WriteString("\x1b[?1002h")
	}
	if m.MouseAll {
		b.WriteString("\x1b[?1003h")
	}
	if m.MouseTracking && !m.MouseStandard && !m.MouseButton && !m.MouseAll {
		// tmux normally reports the exact standard/button/all flag alongside
		// mouse_any. Keep an aggregate-only producer useful without inventing a
		// motion mode: normal tracking is the least permissive representation.
		b.WriteString("\x1b[?1000h")
	}
	if m.MouseUTF8 {
		b.WriteString("\x1b[?1005h")
	}
	if m.MouseSGR {
		b.WriteString("\x1b[?1006h")
	}
	return []byte(b.String())
}
