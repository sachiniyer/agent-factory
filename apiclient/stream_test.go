package apiclient

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// previewServer stands up a Unix-socket HTTP server answering POST /v1/Preview
// like the daemon does, and returns a Client dialing it plus a pointer to the
// last request it saw (so a test can assert the tab/full/title were carried).
func previewServer(t *testing.T, handle func(daemon.PreviewRequest) apiproto.Envelope) (*Client, *daemon.PreviewRequest) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	got := &daemon.PreviewRequest{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Preview", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.PreviewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		*got = req
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, handle(req))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewWithSocket(sockPath), got
}

// TestPreview_CarriesTabAndReturnsContent proves the tab/full selector and the
// content round-trip: the client POSTs the request the daemon Preview RPC expects
// and decodes {content} back.
func TestPreview_CarriesTabAndReturnsContent(t *testing.T) {
	c, got := previewServer(t, func(req daemon.PreviewRequest) apiproto.Envelope {
		return apiproto.Success(daemon.PreviewResponse{Content: "PANE-" + strconv.Itoa(req.Tab)})
	})

	content, gone, err := c.Preview(daemon.PreviewRequest{Title: "alpha", RepoID: "r", Tab: 2, Full: true})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if gone {
		t.Fatalf("gone must be false on a normal capture")
	}
	if content != "PANE-2" {
		t.Fatalf("content = %q, want PANE-2", content)
	}
	if got.Title != "alpha" || got.RepoID != "r" || got.Tab != 2 || !got.Full {
		t.Fatalf("request lost fields: %+v", *got)
	}
}

// TestPreview_GoneSurfacesFlag proves the session-gone signal survives the HTTP
// boundary as a structured field (the sentinel error can't).
func TestPreview_GoneSurfacesFlag(t *testing.T) {
	c, _ := previewServer(t, func(daemon.PreviewRequest) apiproto.Envelope {
		return apiproto.Success(daemon.PreviewResponse{Gone: true})
	})
	content, gone, err := c.Preview(daemon.PreviewRequest{Title: "alpha"})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !gone || content != "" {
		t.Fatalf("want gone=true, empty content; got gone=%v content=%q", gone, content)
	}
}

// TestDialStream_HandshakeCarriesQueryAndStartSeq proves DialStream builds the
// right URL (tab/since query params) and reads the X-Af-Stream-Seq handshake
// header back as the starting cursor. A minimal echo server accepts the WS and
// replies with a PTY_OUT frame.
func TestDialStream_HandshakeCarriesQueryAndStartSeq(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Af-Stream-Seq", "42")
		conn, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if aerr != nil {
			return
		}
		_ = agentproto.WriteFrame(context.Background(), conn, agentproto.PTYOutFrame([]byte("hello")))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewWithSocket(sockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := c.DialStream(ctx, "alpha", "repo-x", "", 3, 7)
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	defer func() { _ = sc.Conn.Close(websocket.StatusNormalClosure, "") }()

	if sc.StartSeq() != 42 {
		t.Fatalf("StartSeq = %d, want 42 (from X-Af-Stream-Seq)", sc.StartSeq())
	}
	// The query must carry tab + since + repo_id.
	for _, want := range []string{"tab=3", "since=7", "repo_id=repo-x"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("stream query %q missing %q", gotQuery, want)
		}
	}
	msg, err := agentproto.ReadMessage(ctx, sc.Conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !msg.Binary || string(msg.Frame.Data) != "hello" {
		t.Fatalf("want PTY_OUT 'hello', got %+v", msg)
	}
}
