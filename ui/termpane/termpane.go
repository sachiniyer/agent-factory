// Package termpane renders a live terminal inside a workspace pane by consuming
// the daemon's WebSocket PTY stream (#1592 Phase 2 PR6, replacing the #1089
// tmux-attach render client).
//
// A TermPane owns one live subscription to a session tab's PTY stream: a Dialer
// opens a Stream against the daemon (GET /v1/sessions/{id}/stream), raw PTY_OUT
// bytes are fed through a charmbracelet/x/vt terminal emulator into a cell grid,
// and the authoritative resize echo reflows that grid. Render turns the grid into
// an ANSI-styled block the TUI places inside a pane rect — no full-screen takeover.
// Interactive keystrokes and mouse events are encoded by the same emulator and
// sent back as INPUT frames; Resize sends a RESIZE frame (last-resize-wins,
// server-side). Close ends the subscription only; the session keeps running.
//
// The structural reliability win over the old tmux attach client (§6): the stream
// is fanned from the daemon's clientless capture through a bounded ring buffer, so
// a dropped subscriber does NOT take output down — the run loop reconnects and
// replays the gap it missed with ?since=<cursor>. There is therefore NO
// capture-pane fallback and no rebind-retry loop; a WS drop repaints from replay,
// not from a capture snapshot. This package is a pure projection: it never
// creates, kills, or mutates sessions — the daemon stays the sole owner (#960).
//
// The pane is MULTI-WRITER (no lease): every subscriber is read-write and the PTY
// size is last-resize-wins with an authoritative echo. Because the daemon streams
// the pane's own output (pipe-pane) there is no status line to crop — the emulator
// grid is exactly the pane, unlike the tmux attach client which allocated status
// rows outside the rendered window.
package termpane

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/sachiniyer/agent-factory/terminal"
)

const (
	// closeDrainDeadline bounds how long Close waits for the run/send goroutines to
	// drain after the context is cancelled and the emulator pipe closed. They exit
	// in milliseconds in practice; if they don't, Close returns anyway rather than
	// freezing the event loop (a leaked goroutine beats a wedged UI).
	closeDrainDeadline = 2 * time.Second

	// writeTimeout bounds a single INPUT/RESIZE frame write to the stream, so a
	// wedged connection cannot block the event loop or the send pump forever.
	writeTimeout = 5 * time.Second

	// reconnectMinBackoff/reconnectMaxBackoff bound the exponential backoff between
	// dial attempts. The first reconnect is near-immediate (a dropped WS usually
	// reconnects at once and replays via ?since); a session that is genuinely gone
	// backs off so a doomed pane does not hammer the daemon. The owner closes the
	// pane when its binding disappears, so this loop never needs its own give-up.
	reconnectMinBackoff = 50 * time.Millisecond
	reconnectMaxBackoff = 3 * time.Second
)

// EventKind discriminates a stream Event between output bytes, a resize echo, a
// screen repaint, and a cursor re-seed.
type EventKind int

const (
	// EventData carries verbatim PTY output bytes (fed to the emulator, advances the
	// replay cursor).
	EventData EventKind = iota
	// EventResize carries the authoritative last-resize-wins size the emulator
	// reflows to.
	EventResize
	// EventRepaint carries a one-shot screen repaint (fed to the emulator like
	// output) that must NOT advance the replay cursor — it is per-subscriber and not
	// part of the server's ring seq, so counting it would desync ?since.
	EventRepaint
	// EventCursor carries the server's authoritative replay cursor (Seq), which the
	// pane adopts verbatim. The server sends it when IT moved the cursor
	// non-contiguously (a ring eviction or a recovery discard skipped bytes that no
	// longer exist) — a jump our own start + bytes-received arithmetic cannot see, and
	// which would otherwise make the next reconnect replay already-rendered bytes.
	EventCursor
)

// RepaintCursorCoverage says whether a mode-bearing repaint authoritatively
// describes the next cursor jump. Zero is deliberately no coverage: fresh
// subscriber repaints cannot bless bytes evicted later.
type RepaintCursorCoverage uint8

const (
	RepaintCoversNoCursor RepaintCursorCoverage = iota
	RepaintCoversNextCursor
)

// Event is one inbound stream event, selected by Kind: output bytes or a repaint
// (Data), the authoritative size echo (Rows/Cols), or a cursor re-seed (Seq).
type Event struct {
	Kind EventKind
	Data []byte
	Rows uint16
	Cols uint16
	// Modes accompany EventRepaint when HasModes is true. They are the
	// authoritative pre-existing state a fresh/recovered stream subscriber did
	// not observe in the live byte ring.
	Modes    terminal.Modes
	HasModes bool
	// CursorCoverage is nonzero only for a recovery-barrier repaint that
	// authoritatively describes its immediately following cursor re-seed.
	CursorCoverage RepaintCursorCoverage
	// Seq is the server's authoritative replay cursor, valid only when
	// Kind == EventCursor.
	Seq uint64
}

// Stream is one open connection to a session tab's PTY stream. The concrete
// implementation (in the app layer) wraps the apiclient WS dial and agentproto's
// codec; keeping it an interface keeps this package transport-agnostic and lets
// tests drive the emulator/reconnect logic with an in-memory fake.
type Stream interface {
	// StartSeq is the absolute output cursor the server begins sending from — the
	// server may have clamped the requested ?since into its retained ring window.
	StartSeq() uint64
	// Recv blocks for the next inbound event, ctx cancellation, or the connection
	// dropping (returns an error).
	Recv(ctx context.Context) (Event, error)
	// SendInput writes raw key bytes as an INPUT frame (multi-writer).
	SendInput(ctx context.Context, b []byte) error
	// SendResize writes a RESIZE frame (last-resize-wins, server-side).
	SendResize(ctx context.Context, rows, cols uint16) error
	io.Closer
}

// Dialer opens a Stream starting at output cursor `since` (0 = the live tail). The
// run loop calls it with the cursor to replay from after a drop.
type Dialer func(ctx context.Context, since uint64) (Stream, error)

// TermPane is one live embedded terminal: a reconnecting WS subscription + vt
// emulator.
//
// Concurrency: New, Resize, Render, SendKey, SendMouse, and Close are called on
// the bubbletea event loop; the run loop (inbound) and send pump (outbound) run on
// their own goroutines. gridMu guards the emulator grid between the run loop's
// writes and Render's reads; connMu guards the current stream + cursor + desired
// size between the loops and the event-loop mutators; resizeMu serializes the two
// RESIZE senders so the newest size is always the one written last (assertSize).
// resizeMu is ordered before connMu, and no lock here is ever held across stream
// I/O. Close is safe to call more than once.
type TermPane struct {
	gridMu sync.RWMutex
	emu    *vt.Emulator
	// cursorVisible mirrors the terminal's DECTCM state, maintained by the
	// emulator's CursorVisibility callback (fired inside emu.Write, under gridMu's
	// write lock); Render reads it under the read lock. Starts true.
	cursorVisible bool
	// mouseModes is the set of mouse-tracking DECModes (X10/normal/highlight/
	// button-event/any-event) the inner app currently has enabled, maintained by
	// the emulator's EnableMode/DisableMode callbacks (fired inside emu.Write, under
	// gridMu's write lock). MouseTrackingEnabled reads it under the read lock. It
	// mirrors exactly the set emu.SendMouse consults, so "tracking enabled" means
	// "a forwarded wheel/click would actually reach the inner app". Starts empty.
	//
	// It cannot go stale across a terminal reset: a full reset (RIS) runs the
	// emulator's resetModes, which re-drives setMode(ModeReset) for every mouse
	// mode and thus fires DisableMode for each — clearing this set — so the wheel
	// can never stay stuck forwarding to a program that reset the terminal (#1748).
	mouseModes    map[ansi.Mode]bool
	terminalModes terminal.Modes
	// modeAuthority is the complete visibility/connection/recovery state for the
	// snapshot above; modes.go owns every transition so independent flags cannot drift.
	modeAuthority terminalModesAuthority
	width, height int

	dial Dialer

	connMu             sync.Mutex
	stream             Stream // current live stream; nil while (re)connecting
	wantRows, wantCols uint16 // desired size, (re)asserted on each connect
	cursor             uint64 // absolute output cursor for ?since replay

	// resizeMu serializes the two RESIZE senders — Resize() and the run loop's
	// per-connect re-assert — so that reading the desired size and writing it is
	// one indivisible step. It is what makes the server's last-resize-wins rule
	// mean last-INTENT-wins; see assertSize. Ordered BEFORE connMu and never held
	// the other way round.
	resizeMu sync.Mutex

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

// New starts a live pane at the given grid size, dialing the stream through dial.
// It returns immediately — the first dial (and any reconnect) happens on the run
// goroutine, so a not-yet-ready session self-heals via reconnect rather than
// failing construction.
func New(dial Dialer, width, height int) *TermPane {
	width, height = clampSize(width, height)
	ctx, cancel := context.WithCancel(context.Background())
	t := &TermPane{
		emu:           vt.NewEmulator(width, height),
		cursorVisible: true,
		mouseModes:    make(map[ansi.Mode]bool),
		width:         width,
		height:        height,
		dial:          dial,
		wantRows:      uint16(height), //nolint:gosec // clampSize bounds to [1, 4096]
		wantCols:      uint16(width),  //nolint:gosec // clampSize bounds to [1, 4096]
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	// The callback fires from emu.Write — always under gridMu's write lock — so the
	// field write is ordered against Render's locked reads.
	t.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { t.cursorVisible = visible },
		AltScreen: func(on bool) {
			t.terminalModes.AlternateScreen = on
		},
		// Track the inner app's mouse-reporting requests so the host can tell
		// whether the wheel belongs to the program or to pane scrollback (#1024
		// wheel fix). Both callbacks fire from emu.Write under gridMu's write lock,
		// ordering these map writes against MouseTrackingEnabled's locked read.
		EnableMode: func(mode ansi.Mode) {
			if isMouseTrackingMode(mode) {
				t.mouseModes[mode] = true
				t.terminalModes.MouseTracking = true
			}
			setTerminalMode(&t.terminalModes, mode, true)
		},
		DisableMode: func(mode ansi.Mode) {
			delete(t.mouseModes, mode)
			t.terminalModes.MouseTracking = len(t.mouseModes) > 0
			setTerminalMode(&t.terminalModes, mode, false)
		},
	})

	t.wg.Add(2)
	go func() { defer t.wg.Done(); t.sendPump() }()
	go func() { defer t.wg.Done(); t.run() }()
	go func() { t.wg.Wait(); close(t.done) }()
	return t
}

// run is the reconnect loop: dial the stream (replaying from the tracked cursor),
// pump its inbound events into the emulator until it drops, then reconnect. This
// loop IS the reliability guarantee — a dropped subscriber repaints from ?since
// replay, never from a capture fallback (§6.3).
func (t *TermPane) run() {
	backoff := reconnectMinBackoff
	for {
		if t.ctx.Err() != nil {
			return
		}
		t.connMu.Lock()
		since := t.cursor
		t.connMu.Unlock()

		stream, err := t.dial(t.ctx, since)
		if err != nil {
			if t.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, reconnectMaxBackoff)
			continue
		}
		backoff = reconnectMinBackoff

		t.connMu.Lock()
		// Adopt the server's starting cursor: it may have clamped our `since` into
		// the retained ring window, in which case we lost a gap the next full
		// repaint re-synchronises.
		t.cursor = stream.StartSeq()
		t.stream = stream
		t.connMu.Unlock()

		t.gridMu.Lock()
		// Direction matters: a start ahead of our request crossed an unretained
		// gap, while a start behind it is the broker's harmless clamp to live tail.
		t.connectTerminalModesLocked(terminalModeReplayContinuityFor(since, stream.StartSeq()))
		t.gridMu.Unlock()

		// (Re)assert our desired size so the server sizes this tab's window to us —
		// the pane may have resized while we were disconnected (last-resize-wins).
		t.assertSize(stream)

		t.readStream(stream)

		// Recv failed or Close cancelled it. The child may change ownership while
		// there is no byte stream to observe, so input routing must fail closed for
		// the entire disconnected window. Keep the value itself for exact replay.
		t.gridMu.Lock()
		t.disconnectTerminalModesLocked()
		t.gridMu.Unlock()

		t.connMu.Lock()
		if t.stream == stream {
			t.stream = nil
		}
		t.connMu.Unlock()
		_ = stream.Close()
	}
}

// readStream pumps one connection's inbound events into the emulator until it
// errors (drop) or the context is cancelled (Close).
func (t *TermPane) readStream(stream Stream) {
	firstInbound := true
	for {
		ev, err := stream.Recv(t.ctx)
		if err != nil {
			return
		}
		openingCursor := firstInbound && ev.Kind == EventCursor && ev.Seq == stream.StartSeq()
		firstInbound = false
		switch ev.Kind {
		case EventData:
			t.gridMu.Lock()
			_, _ = t.emu.Write(ev.Data)
			// Live callbacks evolve a known base in the no-gap case, while any data
			// consumes the repaint's one-shot recovery-cursor coverage.
			t.observeTerminalDataLocked()
			t.gridMu.Unlock()
			t.connMu.Lock()
			t.cursor += uint64(len(ev.Data))
			t.connMu.Unlock()
		case EventRepaint:
			// Render like output, but do NOT advance the cursor: the repaint is
			// per-subscriber and not part of the server's ring seq (§ EventRepaint).
			t.gridMu.Lock()
			_, _ = t.emu.Write(ev.Data)
			if ev.HasModes {
				// The repaint's DEC prefix already updated supported emulator
				// modes. Assign the snapshot as the authority as well: tmux can
				// report UTF-8 encoding even when an emulator does not expose a
				// callback for it, and all-false is meaningful.
				t.installTerminalModesLocked(ev.Modes, ev.CursorCoverage)
			} else {
				// A recovery repaint can jump over unretained DEC mode changes.
				// Without snapshot metadata the old decision is no longer safe.
				t.invalidateTerminalModesLocked()
			}
			t.gridMu.Unlock()
		case EventCursor:
			t.gridMu.Lock()
			t.observeTerminalCursorLocked(openingCursor)
			t.gridMu.Unlock()
			// The server moved our cursor over bytes it no longer holds (an eviction or
			// a recovery discard). Adopt its position verbatim — our own
			// start + bytes-received count is now stale, and reconnecting on it would ask
			// to replay bytes we already rendered (§ EventCursor).
			t.connMu.Lock()
			t.cursor = ev.Seq
			t.connMu.Unlock()
		case EventResize:
			// Authoritative echo: reflow the emulator to the server's size. We drive
			// the size, so this normally matches wantCols/wantRows; applying it
			// anyway honors a size the server clamped (e.g. an old tmux).
			t.gridMu.Lock()
			t.emu.Resize(int(ev.Cols), int(ev.Rows))
			t.gridMu.Unlock()
		}
	}
}

// sendPump is the persistent outbound pump: it drains the emulator's input pipe
// (the encoded keystrokes SendKey/SendMouse inject, plus terminal-query replies)
// and writes them as INPUT frames to the CURRENT stream. It must ALWAYS drain the
// pipe even while disconnected — the pipe is unbuffered and synchronous, so a
// pump that stopped reading would block the next SendKey on the event loop forever
// (the #1089 lesson). While disconnected it drops the bytes; reconnect is fast and
// input to a disconnected pane is not meaningful.
func (t *TermPane) sendPump() {
	buf := make([]byte, 4096)
	for {
		n, rerr := t.emu.Read(buf)
		if n > 0 {
			t.connMu.Lock()
			stream := t.stream
			t.connMu.Unlock()
			if stream != nil {
				wctx, cancel := context.WithTimeout(t.ctx, writeTimeout)
				_ = stream.SendInput(wctx, buf[:n])
				cancel()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// sleep blocks for d or until the context is cancelled; it reports true if the
// context was cancelled (the caller should stop).
func (t *TermPane) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-t.ctx.Done():
		return true
	case <-timer.C:
		return false
	}
}

// SendKey forwards one focused keystroke to the terminal (interactive mode): a
// paste through the emulator's bracketed-paste-aware Paste, everything else
// through the keymap translation. It reports whether the key was forwarded — false
// means the key has no safe encoding and was IGNORED (never a guessed sequence).
// Event-loop only; the enclosed pipe write is synchronous but sendPump always
// drains it. The lock guards the emulator's mode state (DECCKM, bracketed paste)
// the run loop mutates through emu.Write.
func (t *TermPane) SendKey(msg tea.KeyMsg) bool {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	return forwardKey(t.emu, msg)
}

// forwardKey translates one key message and emits its bytes into the emulator's
// input pipe. Factored out so the translation table can be pinned against a bare
// emulator in tests. The caller holds the lock guarding the emulator's mode state.
func forwardKey(emu *vt.Emulator, msg tea.KeyMsg) bool {
	if msg.Paste {
		emu.Paste(string(msg.Runes))
		return true
	}
	tr, ok := translateKey(msg)
	if !ok {
		return false
	}
	if tr.raw != "" {
		_, _ = io.WriteString(emu.InputPipe(), tr.raw)
		return true
	}
	for _, ev := range tr.events {
		emu.SendKey(ev)
	}
	return true
}

// Resize propagates a new pane geometry: the emulator grid immediately (for
// responsiveness) and a RESIZE frame to the server (last-resize-wins). Event-loop
// only.
func (t *TermPane) Resize(width, height int) {
	width, height = clampSize(width, height)
	t.gridMu.Lock()
	if width == t.width && height == t.height {
		t.gridMu.Unlock()
		return
	}
	t.width, t.height = width, height
	// Re-window the emulator grid to the new size. The pinned x/vt emulator
	// truncates rather than reflows (#1556), so the grid may transiently show a
	// stale continuation row — but the daemon injects a clean capture-pane repaint
	// on every resize (the broker's Snapshot-on-resize), which clears and redraws
	// the reflowed screen a round-trip later. So the client does NOT blank here:
	// blanking would leave the pane empty until the repaint arrives, whereas
	// keeping the re-windowed grid shows content continuously.
	t.emu.Resize(width, height)
	t.gridMu.Unlock()

	t.connMu.Lock()
	t.wantRows, t.wantCols = uint16(height), uint16(width) //nolint:gosec // clampSize bounds to [1, 4096]
	stream := t.stream
	t.connMu.Unlock()
	if stream != nil {
		t.assertSize(stream)
	}
}

// assertSize writes the pane's CURRENT desired size to stream as a RESIZE frame.
//
// The read and the write are one step, under resizeMu, because they have two
// writers: Resize() when the geometry changes, and the run loop once per
// (re)connect, to tell a brand-new stream a size the server has never heard. When
// each merely captured the size and wrote it after unlocking, a Resize() landing
// in the run loop's window updated the size and wrote it FIRST, leaving the run
// loop's now-stale frame to arrive LAST. The server's rule is last-resize-wins, so
// the stale value won and was echoed back, pinning the emulator grid to the old
// geometry while the TUI frame stayed at the new one (#2417). Serializing makes
// the last frame on the wire necessarily the newest intent: whichever sender
// writes last reads the desired size only after acquiring resizeMu, and Resize()
// always writes after committing its value.
//
// Deliberately NOT connMu, though connMu already guards these fields. connMu is
// also taken per inbound data event to advance the replay cursor, and on the
// bubbletea event loop by Resize/Close; holding it across a network write bounded
// by writeTimeout would stall those behind a slow socket. This mirrors sendPump,
// which likewise reads the stream under connMu and writes outside it — no lock in
// this type is ever held across stream I/O.
func (t *TermPane) assertSize(stream Stream) {
	t.resizeMu.Lock()
	defer t.resizeMu.Unlock()
	t.connMu.Lock()
	rows, cols := t.wantRows, t.wantCols
	t.connMu.Unlock()
	wctx, cancel := context.WithTimeout(t.ctx, writeTimeout)
	_ = stream.SendResize(wctx, rows, cols)
	cancel()
}

// Render returns the emulator's grid as exactly height ANSI-styled lines of
// exactly width cells. It only reads the grid — safe at any cadence; the TUI's
// tick drives it. showCursor overlays the terminal cursor (reverse-video) when the
// inner app has it visible — the interactive-mode typing cue. There is no status
// offset: the streamed bytes are the pane itself (§ package doc).
func (t *TermPane) Render(width, height int, showCursor bool) string {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	cursor := cursorNone
	if showCursor && t.cursorVisible {
		pos := t.emu.CursorPosition()
		cursor = cursorAt{x: pos.X, y: pos.Y, show: true}
	}
	visibleHeight := min(height, t.height)
	grid := renderGridWindow(t.emu, width, visibleHeight, 0, cursor)
	return padRenderedRows(grid, width, height-visibleHeight)
}

// Close ends the subscription only — cancel the run loop, drop and close the
// current stream, close the emulator's input pipe to release the send pump — and
// waits (bounded) for both goroutines to drain. The session keeps running
// server-side; closing a pane must never kill the user's agent. Safe to call more
// than once.
func (t *TermPane) Close() error {
	var err error
	t.closeOnce.Do(func() {
		t.cancel()
		t.connMu.Lock()
		stream := t.stream
		t.stream = nil
		t.connMu.Unlock()
		if stream != nil {
			_ = stream.Close() // unblock readStream's Recv
		}
		// Unblock the send pump. Deliberately NOT emu.Close(): the pinned x/vt sets
		// an unsynchronized `closed` flag there that races the pump's Read (-race).
		// Closing the pipe's writer end is internally synchronized; the pump's Read
		// returns EOF.
		if pw, ok := t.emu.InputPipe().(*io.PipeWriter); ok {
			_ = pw.CloseWithError(io.EOF)
		} else {
			_ = t.emu.Close()
		}
		select {
		case <-t.done:
		case <-time.After(closeDrainDeadline):
			err = fmt.Errorf("termpane: goroutines did not drain within %v after close", closeDrainDeadline)
		}
	})
	return err
}

// clampSize bounds a pane geometry to what the emulator and a tmux window accept:
// at least 1x1 (a zero-rected auto-hidden pane must not produce a zero size), at
// most 4096 per axis (also keeps the uint16 casts safe).
func clampSize(width, height int) (int, int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	if width > 4096 {
		width = 4096
	}
	if height > 4096 {
		height = 4096
	}
	return width, height
}
