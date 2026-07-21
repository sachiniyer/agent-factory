package terminal

import (
	"strings"
	"testing"
)

func TestModesRestoreSequenceReplacesOwnershipModes(t *testing.T) {
	m := Modes{
		AlternateScreen: true,
		MouseButton:     true,
		MouseSGR:        true,
	}
	got := string(m.RestoreSequence())
	for _, want := range []string{
		"\x1b[?1049l",
		"\x1b[?9l",
		"\x1b[?1000l",
		"\x1b[?1001l",
		"\x1b[?1002l",
		"\x1b[?1003l",
		"\x1b[?1005l",
		"\x1b[?1006l",
		"\x1b[?1049h",
		"\x1b[?1002h",
		"\x1b[?1006h",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("RestoreSequence() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "\x1b[?1000h") || strings.Contains(got, "\x1b[?1003h") {
		t.Fatalf("RestoreSequence() enabled a tracking mode absent from the snapshot: %q", got)
	}
}

func TestMouseEncodingDoesNotClaimTracking(t *testing.T) {
	if (Modes{MouseSGR: true, MouseUTF8: true}).MouseTrackingEnabled() {
		t.Fatal("encoding flags alone must not claim mouse ownership")
	}
	if !(Modes{MouseStandard: true}).MouseTrackingEnabled() {
		t.Fatal("a tracking flag must claim mouse ownership")
	}
}
