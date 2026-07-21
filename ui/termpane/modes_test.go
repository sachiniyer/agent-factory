package termpane

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/terminal"
	"github.com/stretchr/testify/require"
)

func TestRepaintRestoresTerminalOwnershipModes(t *testing.T) {
	tp, stream := newSingleStreamPane(t, 40, 8)
	if _, known := tp.TerminalModes(); known {
		t.Fatal("fresh pane guessed ownership before the stream snapshot")
	}
	stream.events <- Event{Kind: EventData, Data: []byte("\x1b[?1006h")}
	require.Eventually(t, func() bool {
		got, known := tp.TerminalModes()
		return got.MouseSGR && !known
	}, 2*time.Second, 5*time.Millisecond,
		"one live mode change must not pretend every pre-existing mode is known")

	child := terminal.Modes{
		AlternateScreen: true,
		MouseTracking:   true,
		MouseButton:     true,
		MouseSGR:        true,
	}
	stream.events <- Event{
		Kind:     EventRepaint,
		Data:     append(child.RestoreSequence(), []byte("\x1b[2J\x1b[Htranscript")...),
		Modes:    child,
		HasModes: true,
	}
	require.Eventually(t, func() bool {
		got, known := tp.TerminalModes()
		return known && got == child && tp.MouseTrackingEnabled()
	}, 2*time.Second, 5*time.Millisecond,
		"fresh repaint must restore alternate-screen, tracking, and mouse encoding")

	// A recovery repaint is authoritative in the other direction too: stale
	// child ownership cannot survive after the pane returned to the primary
	// screen while this subscriber missed output.
	host := terminal.Modes{}
	stream.events <- Event{
		Kind:     EventRepaint,
		Data:     append(host.RestoreSequence(), []byte("\x1b[2J\x1b[Hcomposer")...),
		Modes:    host,
		HasModes: true,
	}
	require.Eventually(t, func() bool {
		got, known := tp.TerminalModes()
		return known && got == host && !tp.MouseTrackingEnabled()
	}, 2*time.Second, 5*time.Millisecond,
		"recovery repaint must clear stale alternate-screen and mouse ownership")

	stream.events <- Event{Kind: EventRepaint, Data: []byte("\x1b[2J\x1b[Hlegacy")}
	require.Eventually(t, func() bool {
		_, known := tp.TerminalModes()
		return !known
	}, 2*time.Second, 5*time.Millisecond,
		"a metadata-free recovery repaint must invalidate the prior owner")
}
