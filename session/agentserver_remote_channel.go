package session

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
)

// --- remote WS clientlessChannel: the sandbox stream as a broker channel ---

// remoteClientlessChannel is the remote runtime's clientlessChannel: it binds ONE
// tab's ptyBroker to the sandbox's WS PTY stream. Where tmuxClientlessChannel
// drives pipe-pane/send-keys/resize-window on a local tmux pane, this drives the
// same three verbs over a single bidirectional WebSocket to the `af agent-server`:
//
//   - StartCapture dials the tab's WS and returns a reader that yields the
//     sandbox's PTY_OUT bytes (dropping the remote's own repaint/resize-echo control
//     frames — the daemon-side broker re-derives those locally, so feeding them into
//     the ring would double-count them in the seq).
//   - SendRaw/Resize write INPUT/RESIZE frames back over that same socket (the
//     agent-server accepts input from any subscriber — the multi-writer model).
//   - Snapshot fetches the current screen over REST Preview so the broker can paint
//     a fresh local subscriber, exactly as the tmux channel's capture-pane does.
//
// This is what lets remoteAgentServer reuse ptyBroker unchanged: the broker never
// knows whether its bytes come from a local pipe or a remote socket.
type remoteClientlessChannel struct {
	rc  *remoteAgentClient
	tab int
	// tabID, when set, addresses the tab by stable id (#5571 W4.4) and tab is
	// unused: stream dial and repaint both bind the id inside the sandbox.
	tabID string

	mu     sync.Mutex
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	// remoteSeq is this channel's cursor in the REMOTE broker's ring (#5571
	// W1.2): the OpHello start seq of the current subscription plus the bytes
	// it has delivered since. A re-dial sends it as ?since=, so bytes the
	// sandbox produced while the tunnel was down replay into the daemon-side
	// ring as contiguous output instead of a silent scrollback hole. Written
	// by readLoop under mu; read by StartCapture under the same lock.
	remoteSeq uint64
	// dialSeq is the ?since the live subscription requested, and helloPending
	// marks its OpHello as not yet seen. The first OpHello's seq is the
	// post-clamp start position — equal to dialSeq iff the remote honored the
	// cursor exactly. Any clamp (down over an evicted head, or up over a base
	// ahead of the cursor) leaves a gap the local ring cannot backfill, so the
	// recovery repaint is still owed; replayContiguous records the verdict.
	dialSeq          uint64
	helloPending     bool
	replayContiguous bool
	// writeMu serializes SendRaw/Resize writes: coder/websocket allows one
	// concurrent reader + one writer, but two goroutines writing (an input and a
	// resize) would race the frame boundary.
	writeMu sync.Mutex
}

var _ clientlessChannel = (*remoteClientlessChannel)(nil)

// PaneDead is always false here: a dead REMOTE pane is the remote daemon's
// business — its own broker serves the repaint-only stream, which reaches this
// channel as ordinary upstream bytes (#5330). Nothing local ever needs the
// verdict this method answers for.
func (c *remoteClientlessChannel) PaneDead() bool {
	return false
}

func (c *remoteClientlessChannel) tabLabel() string {
	if c.tabID != "" {
		return c.tabID
	}
	return strconv.Itoa(c.tab)
}

func newRemoteClientlessChannel(rc *remoteAgentClient, tab int) *remoteClientlessChannel {
	return &remoteClientlessChannel{rc: rc, tab: tab}
}

// StartCapture dials the tab's WS stream and returns a reader over the
// sandbox's PTY_OUT bytes. The dial carries ?since=remoteSeq whenever the
// channel holds a cursor from an earlier capture (#5571 W1.2): the remote
// broker replays exactly the bytes produced while the socket was down, so a
// transport blip costs a repaint at most, never a hole in the scrollback. The
// first dial — or one after the remote clamped a stale cursor — is a live
// tail, same as before. The reader goroutine also drains the socket's
// control/repaint frames (dropping them) so the connection stays healthy —
// coder/websocket auto-responds to the server's keepalive pings only while a
// read is in flight, so this loop IS the keepalive. It is the broker's
// first-Subscribe entry point, so the returned reader is consumed immediately
// and never blocks the socket.
func (c *remoteClientlessChannel) StartCapture() (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return nil, fmt.Errorf("remote clientless capture already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := c.rc.dialStream(ctx, c.tab, c.tabID, c.remoteSeq)
	if err != nil {
		cancel()
		return nil, err
	}
	c.conn, c.ctx, c.cancel = conn, ctx, cancel
	c.dialSeq, c.helloPending, c.replayContiguous = c.remoteSeq, true, false
	pr, pw := io.Pipe()
	go c.readLoop(ctx, conn, pw)
	return pr, nil
}

// readLoop copies the sandbox's PTY_OUT bytes into the broker's pipe until the
// socket errors or capture is stopped. It DROPS the remote's OpRepaint and every
// control frame (MsgResize echo, MsgExit): the daemon-side broker paints its own
// per-subscriber repaint via Snapshot and manages its own resize echo, so passing
// the remote's through would double-count repaint bytes in the ring seq and echo
// resizes twice. This makes the remote socket behave to the broker exactly like
// tmux pipe-pane: a future-only stream of raw output.
//
// The one frame it READS rather than drops is OpHello (#5571 W1.2): its seq is
// the remote ring position this subscription actually started from — the
// ?since cursor post-clamp — so remoteSeq tracks startSeq + Σ|PTY_OUT| with
// the clamp already priced in. It is updated AFTER the bytes are written to
// the pipe, so a failed delivery never advances the cursor past output the
// broker never saw.
func (c *remoteClientlessChannel) readLoop(ctx context.Context, conn *websocket.Conn, pw *io.PipeWriter) {
	defer func() { _ = pw.Close() }()
	for {
		msg, err := agentproto.ReadMessage(ctx, conn)
		if err != nil {
			return
		}
		if !msg.Binary {
			continue
		}
		switch msg.Frame.Op {
		case agentproto.OpHello:
			c.mu.Lock()
			c.reseedRemoteSeqLocked(conn, msg.Frame.Seq)
			if c.helloPending && c.remoteSeqWritableLocked(conn) {
				c.replayContiguous = msg.Frame.Seq == c.dialSeq
				c.helloPending = false
			}
			c.mu.Unlock()
		case agentproto.OpPTYOut:
			n, werr := pw.Write(msg.Frame.Data)
			c.mu.Lock()
			c.advanceRemoteSeqLocked(conn, uint64(n))
			c.mu.Unlock()
			if werr != nil {
				return
			}
		}
	}
}

// remoteSeqWritableLocked reports whether one readLoop's conn may still write
// remoteSeq: dropped only when c.conn is a DIFFERENT live socket — a stale
// loop's frame must never move the cursor a newer subscription already
// re-seeded. Post-StopCapture (c.conn nil) it still applies: bytes the pipe
// provably delivered belong in the cursor, and the next dial's ?since would
// otherwise replay them as duplicated output. Callers hold c.mu.
func (c *remoteClientlessChannel) remoteSeqWritableLocked(conn *websocket.Conn) bool {
	return c.conn == nil || c.conn == conn
}

// reseedRemoteSeqLocked SETS the cursor from an OpHello — a subscription's
// post-clamp start, or a mid-stream re-seed after the remote fast-forwarded
// over evicted bytes (a jump is authoritative, not additive — even to 0 on a
// fresh remote ring).
func (c *remoteClientlessChannel) reseedRemoteSeqLocked(conn *websocket.Conn, seed uint64) {
	if c.remoteSeqWritableLocked(conn) {
		c.remoteSeq = seed
	}
}

// advanceRemoteSeqLocked ADDs to the cursor for bytes just delivered to the
// broker's pipe.
func (c *remoteClientlessChannel) advanceRemoteSeqLocked(conn *websocket.Conn, delta uint64) {
	if c.remoteSeqWritableLocked(conn) {
		c.remoteSeq += delta
	}
}

// ReplaysHistory reports whether THIS capture actually delivers contiguous
// retained output — the remote honored the ?since cursor exactly, proven by
// the subscription's OpHello seq matching the requested dialSeq. A clamped
// OpHello means the remote ring evicted our cursor during the outage and the
// gap is unrecoverable from the stream: the recovery repaint is still owed.
// False whenever no capture is live or the hello has not landed — both are
// the repaint-safe side.
func (c *remoteClientlessChannel) ReplaysHistory() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil && c.replayContiguous
}

// StopCapture cancels the read loop and closes the WS, so the broker's read side
// (the pipe) returns EOF. Idempotent.
func (c *remoteClientlessChannel) StopCapture() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	c.cancel()
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.conn, c.ctx, c.cancel = nil, nil, nil
	return err
}

// SendRaw writes verbatim input bytes to the sandbox PTY as an INPUT frame over
// the capture socket (multi-writer: the agent-server accepts input from any
// subscriber). It requires the capture socket, which the broker always opens on
// the first Subscribe before any input can be routed here.
func (c *remoteClientlessChannel) SendRaw(b []byte) error {
	conn, ctx, err := c.activeConn()
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, remoteWSWriteTimeout)
	defer cancel()
	return agentproto.WriteFrame(wctx, conn, agentproto.InputFrame(b))
}

// Resize sends the winning size to the sandbox PTY as a RESIZE frame. The broker
// tracks last-resize-wins + echoes the authoritative size to local subscribers;
// this just forwards the size to the remote (whose own echo is dropped by readLoop).
func (c *remoteClientlessChannel) Resize(rows, cols uint16) error {
	conn, ctx, err := c.activeConn()
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, remoteWSWriteTimeout)
	defer cancel()
	return agentproto.WriteFrame(wctx, conn, agentproto.ResizeFrame(rows, cols))
}

// Snapshot returns the sandbox's current visible screen over REST Preview — the
// repaint the broker injects so a fresh local subscriber sees the screen before
// the first live byte, mirroring tmux capture-pane. The WS stream carries only
// future output, so without this a just-opened remote pane would render blank.
func (c *remoteClientlessChannel) Snapshot() (PaneSnapshot, error) {
	var snapshot PreviewSnapshot
	var err error
	if c.tabID != "" {
		snapshot, err = c.rc.previewByID(c.tabID, false)
	} else {
		snapshot, err = c.rc.preview(c.tab, false)
	}
	if err != nil {
		return PaneSnapshot{}, err
	}
	// REST Preview carries only the screen, not the cursor position, so the repaint
	// omits cursor restore for remote panes (HasCursor stays false). It is also
	// -J-joined (the agent-server's Preview uses CapturePaneContent), NOT the grid form
	// buildRepaint's per-row positioning wants (#1688) — so a wrapped logical line
	// re-wraps by the client width here. This is a known screen-only best-effort
	// limitation of the still-dark remote runtime (no cursor cascade to corrupt, since
	// HasCursor is false); productizing it needs the agent-server to expose a grid
	// capture + cursor over REST, mirroring tmuxClientlessChannel.Snapshot.
	return PaneSnapshot{
		Screen:   []byte(snapshot.Content),
		Modes:    snapshot.Modes,
		HasModes: snapshot.HasModes,
	}, nil
}

// activeConn returns the live capture socket and its context, or an error if
// capture is not running (no subscriber has opened the stream yet).
func (c *remoteClientlessChannel) activeConn() (*websocket.Conn, context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, nil, fmt.Errorf("remote pty stream for tab %s is not open", c.tabLabel())
	}
	return c.conn, c.ctx, nil
}
