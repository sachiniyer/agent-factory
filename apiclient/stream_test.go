package apiclient

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/terminal"
)

func TestDialStreamErrorDoesNotExposeAccessToken(t *testing.T) {
	const token = "af-sentinel-2644-do-not-log"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	c, err := NewRemote("http://"+addr, token)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	_, err = c.DialStream(context.Background(), StreamSession{Title: "probe"}, "", 0, 0)
	if err == nil {
		t.Fatal("dial against a closed port returned nil error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("failed remote WebSocket dial exposed its access_token query value: %s", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("redaction removed the failed endpoint needed for diagnosis: %s", err)
	}
}

// previewServer stands up a Unix-socket HTTP server answering POST /v1/Preview
// like the daemon does, and returns a Client dialing it plus a pointer to the
// last request it saw (so a test can assert the tab/full/title were carried).
func previewServer(t *testing.T, handle func(daemon.PreviewRequest) apiproto.Envelope) (*Client, *daemon.PreviewRequest) {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
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

	resp, err := c.PreviewSnapshot(daemon.PreviewRequest{Title: "alpha", RepoID: "r", Tab: 2, Full: true})
	if err != nil {
		t.Fatalf("PreviewSnapshot: %v", err)
	}
	if resp.Gone {
		t.Fatalf("gone must be false on a normal capture")
	}
	if resp.Content != "PANE-2" {
		t.Fatalf("content = %q, want PANE-2", resp.Content)
	}
	if got.Title != "alpha" || got.RepoID != "r" || got.Tab != 2 || !got.Full {
		t.Fatalf("request lost fields: %+v", *got)
	}
}

func TestPreviewSnapshotCarriesTerminalModes(t *testing.T) {
	wantModes := terminal.Modes{
		AlternateScreen: true,
		MouseButton:     true,
		MouseSGR:        true,
	}
	c, _ := previewServer(t, func(daemon.PreviewRequest) apiproto.Envelope {
		return apiproto.Success(daemon.PreviewResponse{
			Content:  "ALT-GRID",
			Modes:    wantModes,
			HasModes: true,
		})
	})

	snapshot, err := c.PreviewSnapshot(daemon.PreviewRequest{Title: "alpha"})
	if err != nil {
		t.Fatalf("PreviewSnapshot: %v", err)
	}
	if snapshot.Content != "ALT-GRID" || !snapshot.HasModes || snapshot.Modes != wantModes {
		t.Fatalf("PreviewSnapshot = %+v, want content and modes %+v", snapshot, wantModes)
	}
}

// TestPreview_GoneSurfacesFlag proves the session-gone signal survives the HTTP
// boundary as a structured field (the sentinel error can't).
func TestPreview_GoneSurfacesFlag(t *testing.T) {
	c, _ := previewServer(t, func(daemon.PreviewRequest) apiproto.Envelope {
		return apiproto.Success(daemon.PreviewResponse{Gone: true})
	})
	resp, err := c.PreviewSnapshot(daemon.PreviewRequest{Title: "alpha"})
	if err != nil {
		t.Fatalf("PreviewSnapshot: %v", err)
	}
	if !resp.Gone || resp.Content != "" {
		t.Fatalf("want gone=true, empty content; got gone=%v content=%q", resp.Gone, resp.Content)
	}
}

// TestDialStream_HandshakeCarriesQueryAndStartSeq proves DialStream builds the
// right URL (tab/since query params) and reads the X-Af-Stream-Seq handshake
// header back as the starting cursor. A minimal echo server accepts the WS and
// replies with a PTY_OUT frame.
func TestDialStream_HandshakeCarriesQueryAndStartSeq(t *testing.T) {
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
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
	sc, err := c.DialStream(ctx, StreamSession{Title: "alpha", RepoID: "repo-x"}, "", 3, 7)
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

// TestDialStream_StableIDDialsIDOnly pins the #4760 review contract: a
// StreamSession carrying a stable id dials that id with by=id and NO repo_id, so
// the daemon resolves it in the id namespace only and refuses a miss instead of
// falling back to a same-bytes title. The title and repo scope must not leak
// onto the wire, since either would re-open the title namespace.
func TestDialStream_StableIDDialsIDOnly(t *testing.T) {
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	var gotPath string
	var gotQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.PathValue("id")
		gotQuery = r.URL.Query()
		conn, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if aerr != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewWithSocket(sockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := c.DialStream(ctx, StreamSession{ID: "sess-123", Title: "worker", RepoID: "repo-x"}, "tab-9", 0, 0)
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	_ = sc.Conn.Close(websocket.StatusNormalClosure, "")

	if gotPath != "sess-123" {
		t.Fatalf("stream path segment = %q, want the stable id %q", gotPath, "sess-123")
	}
	if got := gotQuery.Get(agentproto.StreamAddressQueryParam); got != agentproto.StreamAddressByID {
		t.Fatalf("%s = %q, want %q", agentproto.StreamAddressQueryParam, got, agentproto.StreamAddressByID)
	}
	if gotQuery.Has("repo_id") {
		t.Fatalf("an id-addressed dial must not send repo_id; query = %v", gotQuery)
	}
	if got := gotQuery.Get("tab_id"); got != "tab-9" {
		t.Fatalf("tab_id = %q, want tab-9", got)
	}
}

// TestDialStream_SurfacesDaemonErrorBodyOnNon101Handshake pins the bug fix for
// "CLI: Attaching to archived sessions fails with opaque WebSocket 500 instead of
// restore guidance". The daemon's /stream route refuses a non-live target BEFORE
// websocket.Accept, answering the dial with a non-101 status whose body is the
// failure envelope writeHTTPError wrote. coder/websocket's dial error carries
// only the mismatched status ("expected 101 ... but got 404"), so DialStream must
// read that body and append the daemon's message — otherwise the actionable
// refusal (here the Archived restore off-ramp) is dropped and the user sees an
// opaque transport fault. This stands up the daemon's exact refusal shape (a 404
// carrying the apiproto failure envelope, no WS upgrade) and asserts the message
// reaches the caller.
func TestDialStream_SurfacesDaemonErrorBodyOnNon101Handshake(t *testing.T) {
	const wantMessage = `target session "archived" is Archived; prompt not delivered; restore it first (af sessions restore -- archived)`
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, _ *http.Request) {
		// Mirror daemon.httpserver.writeHTTPEnvelope: marshal the envelope, set
		// Content-Length, then WriteHeader + body, so the refusal shape matches
		// what the daemon actually sends on the liveness gate.
		body, mErr := apiproto.MarshalIndented(apiproto.Failure(wantMessage))
		if mErr != nil {
			t.Fatalf("marshal envelope: %v", mErr)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewWithSocket(sockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.DialStream(ctx, StreamSession{Title: "archived", RepoID: "repo-x"}, "", 0, 0)
	if err == nil {
		t.Fatal("DialStream against a non-101 refusal returned nil error; want the daemon's refusal")
	}
	if !strings.Contains(err.Error(), wantMessage) {
		t.Fatalf("DialStream dropped the daemon's error body on the non-101 handshake; error must surface the actionable refusal\nerr: %v", err)
	}
	// The mismatched status is still present — the body augments it, it does not
	// replace the transport context — so a failure that is NOT the daemon's
	// refusal (e.g. a proxy 500) still names the status that broke the handshake.
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("DialStream error lost the handshake status; the daemon message must augment, not replace it\nerr: %v", err)
	}
	// The access-token redaction path still wraps the composed error: the dial
	// is a TransportError so the existing classification (warm-up retry, etc.) is
	// unchanged — only the message gained the daemon's body.
	if !IsTransportError(err) {
		t.Fatalf("a non-101 dial must stay a TransportError so callers' classification is unchanged; got %T: %v", err, err)
	}
}

// TestDialStream_Non101WithEmptyBodyKeepsHandshakeStatus proves the body-surfacing
// path does not add noise when there is nothing to surface: a non-101 with an empty
// body (a transport fault, not the daemon's refusal) reports the handshake status
// alone, unchanged from before the body-surfacing fix.
func TestDialStream_Non101WithEmptyBodyKeepsHandshakeStatus(t *testing.T) {
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewWithSocket(sockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.DialStream(ctx, StreamSession{Title: "warming"}, "", 0, 0)
	if err == nil {
		t.Fatal("DialStream against a non-101 empty-body refusal returned nil error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("DialStream error lost the handshake status for an empty body\nerr: %v", err)
	}
	if strings.Contains(err.Error(), "empty response body") {
		t.Fatalf("DialStream appended body-snippet noise for an empty non-101 body; the status alone is the answer\nerr: %v", err)
	}
}
