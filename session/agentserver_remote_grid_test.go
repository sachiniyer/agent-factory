package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// previewServer answers /v1/agent/preview with data and records the request.
func previewServer(t *testing.T, data map[string]any, got *agentPreviewReq) *remoteAgentClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "error": nil})
	}))
	t.Cleanup(srv.Close)
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "tok"}, "probe")
	if err != nil {
		t.Fatalf("newRemoteAgentClient: %v", err)
	}
	return rc
}

// A grid-capable agent-server's answer reaches the repaint whole: the per-row
// grid (not the joined content), the cursor and the measured size (#5571 W4.2).
func TestRemotePreviewGridCarriesGridCursorAndSize(t *testing.T) {
	var req agentPreviewReq
	rc := previewServer(t, map[string]any{
		"content": "joined line", "has_grid": true, "grid": "row0\nrow1\n",
		"has_cursor": true, "cursor_row": 1, "cursor_col": 4,
		"has_size": true, "rows": 24, "cols": 80,
	}, &req)
	snap, err := rc.previewGrid(3)
	if err != nil {
		t.Fatalf("previewGrid: %v", err)
	}
	if !req.Grid || req.Tab != 3 {
		t.Fatalf("request = %+v, want grid=true tab=3", req)
	}
	if string(snap.Screen) != "row0\nrow1\n" {
		t.Fatalf("Screen = %q, want the grid, not the joined content", snap.Screen)
	}
	if !snap.HasCursor || snap.CursorRow != 1 || snap.CursorCol != 4 {
		t.Fatalf("cursor = %+v", snap)
	}
	if !snap.HasSize || snap.Rows != 24 || snap.Cols != 80 {
		t.Fatalf("size = %+v", snap)
	}
}

// An agent-server that predates the grid ignores the request flag and answers
// the joined content: the client degrades to today's screen-only repaint.
func TestRemotePreviewGridFallsBackOnLegacyAgentServer(t *testing.T) {
	var req agentPreviewReq
	rc := previewServer(t, map[string]any{"content": "joined line"}, &req)
	snap, err := rc.previewGrid(0)
	if err != nil {
		t.Fatalf("previewGrid: %v", err)
	}
	if string(snap.Screen) != "joined line" || snap.HasCursor || snap.HasSize {
		t.Fatalf("legacy fallback = %+v, want the joined screen only", snap)
	}
}

// Lines-above crosses the wire with its known bit, so a partial remote capture
// is not read as complete (#3169).
func TestRemotePreviewCarriesLinesAbove(t *testing.T) {
	var req agentPreviewReq
	rc := previewServer(t, map[string]any{
		"content": "x", "lines_above": 42, "lines_above_known": true,
	}, &req)
	snap, err := rc.preview(0, false)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if snap.LinesAbove != 42 || !snap.LinesAboveKnown {
		t.Fatalf("lines above = %d known=%v, want 42 known", snap.LinesAbove, snap.LinesAboveKnown)
	}
}
