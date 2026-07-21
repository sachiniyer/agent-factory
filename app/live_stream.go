package app

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/termpane"
)

// newTabPaneSource builds the ui.PreviewSource each TabPane captures through: the
// daemon Preview RPC (#1592 Phase 2 PR6, sole capturer). It captures the fetcher
// seam and repoID at construction so the off-loop capture goroutine never reads
// home state directly. A gone session maps back to tmux.ErrSessionGone, the exact
// sentinel TabPane's fallback logic already keys on (it doesn't survive the HTTP
// boundary as an error).
func (m *home) newTabPaneSource() ui.PreviewSource {
	repoID := m.repoID
	fetch := m.previewFetcher
	return func(instance *session.Instance, tab int, full bool) (ui.PreviewSnapshot, error) {
		if instance == nil || fetch == nil {
			return ui.PreviewSnapshot{}, nil
		}
		// Address the capture by the tab's stable id (#1738) so it can't grab the
		// wrong tab after a reorder/close. Only an empty id uses the legacy ordinal;
		// a non-empty id that no longer resolves is refused, never fallen back.
		tabID, _ := instance.TabIDAt(tab)
		resp, err := fetch(daemon.PreviewRequest{Title: instance.Title, RepoID: repoID, Tab: tab, TabID: tabID, Full: full})
		if err != nil {
			return ui.PreviewSnapshot{}, err
		}
		if resp.Gone {
			return ui.PreviewSnapshot{}, tmux.ErrSessionGone
		}
		return ui.PreviewSnapshot{
			Content: resp.Content,
			Owner:   scrollOwnerForSnapshot(resp.Modes, resp.HasModes),
		}, nil
	}
}

// This file is the glue between the transport-agnostic ui/termpane emulator and
// the daemon's WS PTY data plane (#1592 Phase 2 PR6): it turns a (title, repoID,
// tab) into a termpane.Dialer backed by apiclient.DialStream, and wraps each WS
// connection as a termpane.Stream that translates agentproto frames to/from the
// emulator's Event model. Keeping the codec here means ui/termpane depends on
// neither the websocket library nor agentproto — only its own Stream interface.

// streamDialer builds the termpane.Dialer for one pane's (session title, repoID,
// tab). The returned dialer opens a fresh apiclient WS subscription starting at
// the requested replay cursor; the termpane run loop calls it on connect and on
// every reconnect.
func streamDialer(title, repoID, tabID string, tab int) termpane.Dialer {
	return func(ctx context.Context, since uint64) (termpane.Stream, error) {
		c, err := apiclient.NewTargeted()
		if err != nil {
			return nil, err
		}
		sc, err := c.DialStream(ctx, title, repoID, tabID, tab, since)
		if err != nil {
			return nil, err
		}
		return &apiStream{sc: sc}, nil
	}
}

// apiStream adapts an apiclient.StreamConn to termpane.Stream using agentproto's
// codec: PTY_OUT binary frames become EventData, HELLO becomes EventCursor, the
// authoritative resize control frame becomes EventResize, and INPUT/RESIZE go out as
// binary frames.
type apiStream struct {
	sc           *apiclient.StreamConn
	pendingModes *agentproto.TerminalModesMessage
}

var _ termpane.Stream = (*apiStream)(nil)

func (s *apiStream) StartSeq() uint64 { return s.sc.StartSeq() }

// Recv reads the next inbound event, skipping control frames the emulator does not
// consume (exit, and any stray non-PTY_OUT binary frame). It loops rather than
// returning on those so a single ignored frame does not look like a drop.
func (s *apiStream) Recv(ctx context.Context) (termpane.Event, error) {
	for {
		msg, err := agentproto.ReadMessage(ctx, s.sc.Conn)
		if err != nil {
			return termpane.Event{}, err
		}
		if msg.Binary {
			switch msg.Frame.Op {
			case agentproto.OpPTYOut:
				return termpane.Event{Kind: termpane.EventData, Data: msg.Frame.Data}, nil
			case agentproto.OpRepaint:
				ev := termpane.Event{Kind: termpane.EventRepaint, Data: msg.Frame.Data}
				if s.pendingModes != nil {
					ev.Modes, ev.HasModes = s.pendingModes.Modes, true
					if s.pendingModes.CoversNextCursor {
						ev.CursorCoverage = termpane.RepaintCoversNextCursor
					}
					s.pendingModes = nil
				}
				return ev, nil
			case agentproto.OpHello:
				// The server's authoritative cursor. The opening hello merely restates the
				// X-Af-Stream-Seq header the pane already adopted (harmlessly idempotent);
				// a MID-STREAM one is load-bearing — it reports a jump the server made over
				// evicted/discarded bytes, which our byte-counting cursor cannot see.
				return termpane.Event{Kind: termpane.EventCursor, Seq: msg.Frame.Seq}, nil
			}
			continue // INPUT/RESIZE are client→server; ignore any echoed back
		}
		typ, _ := agentproto.MessageTypeOf(msg.Text)
		if typ == agentproto.MsgTerminalModes {
			var mm agentproto.TerminalModesMessage
			// A malformed replacement must not lend the prior repaint's modes to
			// the next grid. Clear first so ownership metadata is fail-closed.
			s.pendingModes = nil
			if json.Unmarshal(msg.Text, &mm) == nil {
				s.pendingModes = &mm
			}
			// Modes and repaint are emitted consecutively by the daemon. Keep
			// reading so termpane applies both under one emulator lock.
			continue
		}
		if typ == agentproto.MsgResize {
			var rm agentproto.ResizeMessage
			if json.Unmarshal(msg.Text, &rm) == nil {
				return termpane.Event{Kind: termpane.EventResize, Rows: rm.Rows, Cols: rm.Cols}, nil
			}
		}
		// Other control frames (exit, ...) carry no grid state — wait for the next.
	}
}

func (s *apiStream) SendInput(ctx context.Context, b []byte) error {
	return agentproto.WriteFrame(ctx, s.sc.Conn, agentproto.InputFrame(b))
}

func (s *apiStream) SendResize(ctx context.Context, rows, cols uint16) error {
	return agentproto.WriteFrame(ctx, s.sc.Conn, agentproto.ResizeFrame(rows, cols))
}

func (s *apiStream) Close() error {
	return s.sc.Conn.Close(websocket.StatusNormalClosure, "")
}
