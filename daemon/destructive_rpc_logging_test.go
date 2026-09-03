package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureDestructiveRPCInfoLog(t *testing.T) *logCapture {
	t.Helper()
	return captureInfo(t)
}

func TestKillSessionRPCLogNamesResolvedTargetAndControlSocket(t *testing.T) {
	manager, repoA, _, repoB, dataB := createDuplicateTitleSessions(t, "feature")
	info := captureDestructiveRPCInfoLog(t)

	var resp KillSessionResponse
	if err := (&controlServer{manager: manager}).KillSession(KillSessionRequest{
		ID: dataB.ID, Title: "stale-display-title",
	}, &resp); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	want := fmt.Sprintf("KillSession requested for session %q (id %s, repo %s) by control socket", dataB.Title, dataB.ID, repoB.ID)
	if got := info.String(); !strings.Contains(got, want) {
		t.Fatalf("KillSession log = %q, want it to contain %q", got, want)
	} else if strings.Contains(got, repoA.ID) {
		t.Fatalf("KillSession log = %q, must name only the id-resolved repo %s", got, repoB.ID)
	}
}

func TestKillSessionHTTPLogNamesPeer(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "http-kill")
	info := captureDestructiveRPCInfoLog(t)
	body, err := json.Marshal(KillSessionRequest{ID: data.ID, Title: "stale-display-title"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/KillSession", bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.44:4242"
	rec := httptest.NewRecorder()
	newHTTPMux(&controlServer{manager: manager}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP KillSession status = %d, body = %s", rec.Code, rec.Body.String())
	}

	want := fmt.Sprintf("KillSession requested for session %q (id %s, repo %s) by HTTP operator peer %s", data.Title, data.ID, repo.ID, req.RemoteAddr)
	if got := info.String(); !strings.Contains(got, want) {
		t.Fatalf("HTTP KillSession log = %q, want it to contain %q", got, want)
	}
}

func TestCloseTabRPCLogNamesResolvedTarget(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "tab-owner")
	created, err := manager.CreateTab(CreateTabRequest{ID: data.ID, Shell: true})
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	info := captureDestructiveRPCInfoLog(t)

	var resp CloseTabResponse
	if err := (&controlServer{manager: manager}).CloseTab(CloseTabRequest{
		ID: data.ID, Title: "stale-display-title", TabID: created.ID,
	}, &resp); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}

	want := fmt.Sprintf("CloseTab requested for tab %q (id %s) in session %q (id %s, repo %s) by control socket", created.Name, created.ID, data.Title, data.ID, repo.ID)
	if got := info.String(); !strings.Contains(got, want) {
		t.Fatalf("CloseTab log = %q, want it to contain %q", got, want)
	}
}
