package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/terminal"
	"github.com/sachiniyer/agent-factory/ui/termpane"
)

func testAPIStream(t *testing.T, write func(context.Context, *websocket.Conn) error) *apiStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	serverErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if err := write(ctx, conn); err != nil {
			serverErr <- err
		}
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + srv.URL[len("http"):]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	select {
	case err := <-serverErr:
		t.Fatalf("stream server: %v", err)
	default:
	}
	return &apiStream{sc: &apiclient.StreamConn{Conn: conn}}
}

func TestAPIStreamBindsTerminalModesToRepaint(t *testing.T) {
	want := terminal.Modes{
		AlternateScreen: true,
		MouseTracking:   true,
		MouseButton:     true,
		MouseSGR:        true,
	}
	stream := testAPIStream(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := agentproto.WriteControl(ctx, conn, agentproto.NewTerminalModesMessage(want)); err != nil {
			return err
		}
		return agentproto.WriteFrame(ctx, conn, agentproto.RepaintFrame([]byte("ALT-GRID")))
	})

	ev, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Kind != termpane.EventRepaint || string(ev.Data) != "ALT-GRID" || !ev.HasModes || ev.Modes != want {
		t.Fatalf("event = %+v, want repaint and modes %+v", ev, want)
	}
}

func TestAPIStreamBindsRecoveryCursorCoverageToRepaint(t *testing.T) {
	want := terminal.Modes{AlternateScreen: true}
	stream := testAPIStream(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := agentproto.WriteControl(
			ctx, conn, agentproto.NewTerminalModesMessageCoveringNextCursor(want),
		); err != nil {
			return err
		}
		return agentproto.WriteFrame(ctx, conn, agentproto.RepaintFrame([]byte("RECOVERED-GRID")))
	})

	ev, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Kind != termpane.EventRepaint || ev.CursorCoverage != termpane.RepaintCoversNextCursor {
		t.Fatalf("event = %+v, want recovery repaint covering its next cursor", ev)
	}
}

func TestAPIStreamMalformedModesCannotReusePriorSnapshot(t *testing.T) {
	stream := testAPIStream(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := agentproto.WriteControl(ctx, conn, agentproto.NewTerminalModesMessage(terminal.Modes{AlternateScreen: true})); err != nil {
			return err
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"terminal_modes","modes":"bad"}`)); err != nil {
			return err
		}
		return agentproto.WriteFrame(ctx, conn, agentproto.RepaintFrame([]byte("GRID")))
	})

	ev, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Kind != termpane.EventRepaint || ev.HasModes {
		t.Fatalf("event = %+v, malformed replacement must fail closed", ev)
	}
}
