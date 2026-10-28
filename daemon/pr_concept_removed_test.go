package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

func TestBranchPRConceptRemoved(t *testing.T) {
	for _, route := range HTTPRoutes() {
		if route.Path == "/v1/SetPRInfo" || route.Path == "/v1/RefreshPRInfo" {
			t.Errorf("removed branch PR route still advertised: %s", route.Path)
		}
	}
	var record session.InstanceData
	if err := json.Unmarshal([]byte(`{"title":"legacy","pr_info":{"number":42,"state":"MERGED"}}`), &record); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["pr_info"]; ok {
		t.Error("legacy pr_info must be ignored and absent from sessions JSON")
	}
	if record.Title != "legacy" {
		t.Fatalf("legacy record title lost: %q", record.Title)
	}
}

func TestRemovedBranchPRRoutesReturn404(t *testing.T) {
	mux := newHTTPMux(&controlServer{})
	for _, path := range []string{"/v1/SetPRInfo", "/v1/RefreshPRInfo"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"title":"legacy","pr_info":{"number":42}}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d %s; want 404", path, rec.Code, rec.Body.String())
		}
	}
}
