package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// #6974 review: the first trace identity carried r.URL.RawQuery — and on the
// stream route the query IS the credential (?access_token= authorizes the WS
// handshake because a browser cannot set headers on one), so every OpResize
// line wrote the pane's bearer token to agent-factory.log. The trace must name
// the socket by its tab addressing only.
func TestPTYTraceWhoNeverLogsAccessToken(t *testing.T) {
	const token = "s3cr3t-token-do-not-log"
	req := httptest.NewRequest(http.MethodGet,
		"/v1/stream/x?access_token="+token+"&tab_id=pane-7&since=41&tab=2", nil)
	req.RemoteAddr = "10.0.0.9:51000"

	who := ptyTraceWho(req)
	if strings.Contains(who, token) || strings.Contains(who, "access_token") {
		t.Fatalf("trace identity leaks the credential: %q", who)
	}
	if !strings.Contains(who, "10.0.0.9:51000") || !strings.Contains(who, `tab_id="pane-7"`) {
		t.Fatalf("trace identity lost the socket's addressing: %q", who)
	}

	// The emitted line inherits the same guarantee.
	var buf bytes.Buffer
	log.CaptureOutput(t, log.InfoLog, "", &buf)
	prev := ptyStreamTrace
	ptyStreamTrace = true
	t.Cleanup(func() { ptyStreamTrace = prev })

	b := ptyTracedBinding{ptyTabBinding: noopPTYTabBinding{}, who: who}
	if err := b.resize(24, 80); err != nil {
		t.Fatalf("resize: %v", err)
	}
	line := buf.String()
	if strings.Contains(line, token) || strings.Contains(line, "access_token") {
		t.Fatalf("trace line leaks the credential: %q", line)
	}
	if !strings.Contains(line, "OpResize 24x80") || !strings.Contains(line, `tab_id="pane-7"`) {
		t.Fatalf("trace line lost the writer identity: %q", line)
	}
}

// The legacy ordinal socket carries ?tab= rather than ?tab_id=; the identity
// still names it, still without the credential.
func TestPTYTraceWhoOrdinalTab(t *testing.T) {
	const token = "s3cr3t-token-do-not-log"
	req := httptest.NewRequest(http.MethodGet,
		"/v1/stream/x?access_token="+token+"&tab=3", nil)
	req.RemoteAddr = "10.0.0.9:51000"

	who := ptyTraceWho(req)
	if strings.Contains(who, token) || strings.Contains(who, "access_token") {
		t.Fatalf("trace identity leaks the credential: %q", who)
	}
	if !strings.Contains(who, "tab=3") {
		t.Fatalf("ordinal tab missing from identity: %q", who)
	}
}

type noopPTYTabBinding struct{}

func (noopPTYTabBinding) subscribe(session.Seq) (session.PTYSubscription, error) {
	return nil, nil
}
func (noopPTYTabBinding) input([]byte) error          { return nil }
func (noopPTYTabBinding) resize(uint16, uint16) error { return nil }
