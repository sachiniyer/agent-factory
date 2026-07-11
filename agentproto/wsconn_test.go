package agentproto

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestWSRoundTrip drives the WriteFrame/WriteControl/ReadMessage helpers over a
// real coder/websocket connection, both directions: server → client PTY bytes +
// an authoritative resize echo, and client → server an input frame. This proves
// the wire helpers move the codec's binary frames and the JSON control frames
// intact.
func TestWSRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverGot := make(chan Message, 1)
	serverErr := make(chan error, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer c.Close(websocket.StatusInternalError, "")

		// Server → client: a PTY_OUT frame then an authoritative resize echo.
		if err := WriteFrame(ctx, c, PTYOutFrame([]byte("\x1b[32mready\x1b[0m"))); err != nil {
			serverErr <- err
			return
		}
		if err := WriteControl(ctx, c, NewResizeMessage(24, 80)); err != nil {
			serverErr <- err
			return
		}

		// Client → server: expect an input frame.
		msg, err := ReadMessage(ctx, c)
		if err != nil {
			serverErr <- err
			return
		}
		serverGot <- msg
		c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusInternalError, "")

	// Read the binary PTY frame.
	m1, err := ReadMessage(ctx, c)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if !m1.Binary || m1.Frame.Op != OpPTYOut || !bytes.Equal(m1.Frame.Data, []byte("\x1b[32mready\x1b[0m")) {
		t.Fatalf("unexpected first message: %+v", m1)
	}

	// Read the JSON control frame and discriminate it.
	m2, err := ReadMessage(ctx, c)
	if err != nil {
		t.Fatalf("read control: %v", err)
	}
	if m2.Binary {
		t.Fatalf("expected text control frame, got binary")
	}
	if tp, err := MessageTypeOf(m2.Text); err != nil || tp != MsgResize {
		t.Fatalf("control type = %q (err %v), want resize", tp, err)
	}
	var resize ResizeMessage
	if err := json.Unmarshal(m2.Text, &resize); err != nil {
		t.Fatalf("unmarshal resize: %v", err)
	}
	if resize.Rows != 24 || resize.Cols != 80 {
		t.Fatalf("resize = %+v, want 24x80", resize)
	}

	// Client → server: send an input frame and confirm the server decoded it.
	if err := WriteFrame(ctx, c, InputFrame([]byte("q\n"))); err != nil {
		t.Fatalf("write input: %v", err)
	}
	select {
	case got := <-serverGot:
		if !got.Binary || got.Frame.Op != OpInput || !bytes.Equal(got.Frame.Data, []byte("q\n")) {
			t.Fatalf("server got %+v, want INPUT q\\n", got)
		}
	case err := <-serverErr:
		t.Fatalf("server error: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for server")
	}

	c.Close(websocket.StatusNormalClosure, "")
}
