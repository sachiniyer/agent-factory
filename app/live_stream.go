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
	return func(instance *session.Instance, tab int, full bool) (string, error) {
		if instance == nil || fetch == nil {
			return "", nil
		}
		content, gone, err := fetch(daemon.PreviewRequest{Title: instance.Title, RepoID: repoID, Tab: tab, Full: full})
		if err != nil {
			return "", err
		}
		if gone {
			return "", tmux.ErrSessionGone
		}
		return content, nil
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
func streamDialer(title, repoID string, tab int) termpane.Dialer {
	return func(ctx context.Context, since uint64) (termpane.Stream, error) {
		c, err := apiclient.New()
		if err != nil {
			return nil, err
		}
		sc, err := c.DialStream(ctx, title, repoID, tab, since)
		if err != nil {
			return nil, err
		}
		return &apiStream{sc: sc}, nil
	}
}

// apiStream adapts an apiclient.StreamConn to termpane.Stream using agentproto's
// codec: PTY_OUT binary frames become EventData, the authoritative resize control
// frame becomes EventResize, and INPUT/RESIZE go out as binary frames.
type apiStream struct {
	sc *apiclient.StreamConn
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
				return termpane.Event{Kind: termpane.EventRepaint, Data: msg.Frame.Data}, nil
			}
			continue // INPUT/RESIZE are client→server; ignore any echoed back
		}
		if typ, _ := agentproto.MessageTypeOf(msg.Text); typ == agentproto.MsgResize {
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
