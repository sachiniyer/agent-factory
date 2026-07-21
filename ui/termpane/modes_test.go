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

func TestTerminalModesBecomeUnknownWhileStreamIsDisconnected(t *testing.T) {
	tp, stream := newSingleStreamPane(t, 40, 8)
	child := terminal.Modes{AlternateScreen: true}
	stream.events <- Event{
		Kind:     EventRepaint,
		Data:     append(child.RestoreSequence(), []byte("\x1b[2J\x1b[Htranscript")...),
		Modes:    child,
		HasModes: true,
	}
	require.Eventually(t, func() bool {
		got, known := tp.TerminalModes()
		return known && got == child
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, stream.Close())
	require.Eventually(t, func() bool {
		_, known := tp.TerminalModes()
		return !known
	}, 2*time.Second, 5*time.Millisecond,
		"a dropped stream must not keep advertising an owner that may change during the outage")
}

func TestClampedReconnectRequiresFreshTerminalModes(t *testing.T) {
	first := newFakeStream(0)
	second := newFakeStream(42)
	dialer := &queueDialer{streams: []*fakeStream{first, second}}
	tp := New(dialer.dial, 40, 8)
	t.Cleanup(func() { _ = tp.Close() })

	child := terminal.Modes{AlternateScreen: true}
	first.events <- Event{
		Kind:     EventRepaint,
		Data:     child.RestoreSequence(),
		Modes:    child,
		HasModes: true,
	}
	require.Eventually(t, func() bool {
		got, known := tp.TerminalModes()
		return known && got == child
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool {
		_, connected := second.lastResize()
		return connected
	}, 2*time.Second, 5*time.Millisecond, "the pane must establish the clamped reconnect")
	_, known := tp.TerminalModes()
	require.False(t, known,
		"a reconnect clamped over unretained bytes must not reuse the pre-gap owner")

	host := terminal.Modes{}
	second.events <- Event{
		Kind:     EventRepaint,
		Data:     host.RestoreSequence(),
		Modes:    host,
		HasModes: true,
	}
	require.Eventually(t, func() bool {
		got, current := tp.TerminalModes()
		return current && got == host
	}, 2*time.Second, 5*time.Millisecond,
		"a fresh authoritative repaint must restore ownership after the gap")
}

func TestCursorJumpInvalidatesModesUnlessFreshRepaintCoversIt(t *testing.T) {
	tp, stream := newSingleStreamPane(t, 40, 8)
	child := terminal.Modes{AlternateScreen: true}
	stream.events <- Event{
		Kind:     EventRepaint,
		Data:     append(child.RestoreSequence(), []byte("\x1b[2J\x1b[Htranscript")...),
		Modes:    child,
		HasModes: true,
	}
	require.Eventually(t, func() bool {
		_, known := tp.TerminalModes()
		return known
	}, 2*time.Second, 5*time.Millisecond)

	// Recovery ordering is repaint -> cursor -> data. The repaint describes the
	// post-gap terminal, so its immediately following cursor jump is covered and
	// must not throw away the authority that just arrived.
	stream.feedCursor(77)
	require.Eventually(t, func() bool {
		tp.connMu.Lock()
		defer tp.connMu.Unlock()
		return tp.cursor == 77
	}, 2*time.Second, 5*time.Millisecond)
	_, known := tp.TerminalModes()
	require.True(t, known, "an authoritative repaint covers its recovery cursor jump")

	// Coverage is single-use. A subsequent jump can skip an unretained mode
	// transition even if no printable data arrived between the two cursor frames.
	stream.feedCursor(99)
	require.Eventually(t, func() bool {
		tp.connMu.Lock()
		defer tp.connMu.Unlock()
		return tp.cursor == 99
	}, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		_, known := tp.TerminalModes()
		return !known
	}, 2*time.Second, 5*time.Millisecond,
		"an uncovered cursor jump may have skipped a mode change")
}
