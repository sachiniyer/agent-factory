package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// `af accounts handoff` tests (#6776 slice 5). The verbs are daemon calls on
// every transport — never direct file access — so the pin worth testing here
// is the wire: a remote target's HandoffStatus answer is what stdout renders,
// and a reset sends exactly the record the operator named, through the real
// apiproto envelope.

// handoffStubDaemon answers the two handoff routes plus health through the
// real envelope writer, recording what the reset was sent.
type handoffStubDaemon struct {
	server *httptest.Server

	mu        sync.Mutex
	status    daemon.HandoffStatusResponse
	resetResp daemon.HandoffResetResponse
	resetReqs []daemon.HandoffResetRequest
}

func newHandoffStubDaemon(t *testing.T) *handoffStubDaemon {
	t.Helper()
	d := &handoffStubDaemon{}
	d.server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

func (d *handoffStubDaemon) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/health":
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{OK: true, Version: "test"}))
	case "/v1/HandoffStatus":
		_ = apiproto.WriteEnvelope(w, apiproto.Success(d.status))
	case "/v1/HandoffReset":
		var req daemon.HandoffResetRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.resetReqs = append(d.resetReqs, req)
		d.mu.Unlock()
		_ = apiproto.WriteEnvelope(w, apiproto.Success(d.resetResp))
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = apiproto.WriteEnvelope(w, apiproto.Failure(fmt.Sprintf("unknown route %q", r.URL.Path)))
	}
}

func TestAccountsHandoffStatus_RemoteTargetRendersTheServedAnswer(t *testing.T) {
	d := newHandoffStubDaemon(t)
	haltAt := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	d.status = daemon.HandoffStatusResponse{
		Mode:       "shadow",
		Engaged:    true,
		Halted:     true,
		HaltReason: "move-failed",
		HaltAt:     &haltAt,
		Backoffs:   []daemon.HandoffBackoffEntry{{SessionKey: "sess-9", Until: until}},
		RecentMoves: []daemon.HandoffMoveEntry{
			{SessionKey: "sess-1", Title: "alpha", Account: "account-b", At: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)},
		},
		MaxMoves:       3,
		MaxMovesWindow: "30m",
	}
	withAccountsRemoteDaemon(t, d.server.URL)

	stdout, _, err := runAccountsHere(t, "handoff", "status")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	for _, want := range []string{
		"mover: halted: move-failed since 2026-11-06T08:00:00Z",
		"engine: observing",
		"sess-9  until 2026-11-06T11:00:00Z",
		"sess-1 (alpha)  → account-b",
		"max 3 in 30m",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status output missing %q:\n%s", want, stdout)
		}
	}
}

func TestAccountsHandoffStatus_JSONEnvelope(t *testing.T) {
	d := newHandoffStubDaemon(t)
	haltAt := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	d.status = daemon.HandoffStatusResponse{Mode: "shadow", Halted: true, HaltReason: "move-failed", HaltAt: &haltAt}
	withAccountsRemoteDaemon(t, d.server.URL)

	stdout, _, err := runAccountsHere(t, "handoff", "status", "--json")
	if err != nil {
		t.Fatalf("status --json failed: %v", err)
	}
	env := requireEnvelope(t, stdout, "status --json stdout")
	var resp daemon.HandoffStatusResponse
	if err := json.Unmarshal(env.Data, &resp); err != nil {
		t.Fatalf("envelope data is not a HandoffStatusResponse: %v", err)
	}
	if !resp.Halted || resp.HaltReason != "move-failed" {
		t.Fatalf("envelope dropped the halt: %+v", resp)
	}
}

// The named-record grammar is the safety property: what the operator typed is
// what the daemon is asked to clear — never a blanket wipe.
func TestAccountsHandoffReset_RemoteTargetSendsTheNamedRecord(t *testing.T) {
	d := newHandoffStubDaemon(t)
	d.resetResp = daemon.HandoffResetResponse{Changed: true, Summary: "cleared 1 backoff entry for session \"sess-1\""}
	withAccountsRemoteDaemon(t, d.server.URL)

	stdout, _, err := runAccountsHere(t, "handoff", "reset", "backoff", "--session", "sess-1")
	if err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if len(d.resetReqs) != 1 {
		t.Fatalf("daemon saw %d reset calls, want exactly 1", len(d.resetReqs))
	}
	if d.resetReqs[0].Record != "backoff" || d.resetReqs[0].SessionKey != "sess-1" {
		t.Fatalf("daemon was sent %+v, want record=backoff session=sess-1", d.resetReqs[0])
	}
	if !strings.Contains(stdout, "cleared 1 backoff entry") {
		t.Fatalf("reset did not echo the daemon's summary:\n%s", stdout)
	}
}

func TestAccountsHandoffReset_RequiresTheRecordOperand(t *testing.T) {
	d := newHandoffStubDaemon(t)
	withAccountsRemoteDaemon(t, d.server.URL)

	_, stderr, err := runAccountsHere(t, "handoff", "reset", "--json")
	if err == nil {
		t.Fatal("reset without a record succeeded")
	}
	env := requireEnvelope(t, stderr, "stderr")
	if env.Error == nil || !strings.Contains(env.Error.Message, "exactly one record") {
		t.Fatalf("envelope is not the record-operand error: %q", stderr)
	}
	if len(d.resetReqs) != 0 {
		t.Fatalf("an argument error still reached the daemon")
	}
}
