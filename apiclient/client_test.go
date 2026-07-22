package apiclient

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// richInstances is a fixture exercising a spread of InstanceData fields —
// scalars, times, an enum, a slice, and a nested struct — so a successful
// round-trip proves the client decodes the daemon envelope back into
// byte-identical structs, not just the two obvious title/path columns.
func richInstances() []session.InstanceData {
	created := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC)
	return []session.InstanceData{
		{
			ID:        "id-aaaa",
			Title:     "alpha",
			Path:      "/home/u/.af/worktrees/alpha",
			Branch:    "feat/alpha",
			Status:    session.Status(1),
			Liveness:  session.Liveness(2),
			Height:    40,
			Width:     120,
			CreatedAt: created,
			UpdatedAt: updated,
			Program:   "claude",
			TmuxName:  "af_alpha",
			Tabs:      []session.TabData{{Name: "agent"}},
		},
		{
			ID:        "id-bbbb",
			Title:     "beta",
			Path:      "/home/u/.af/worktrees/beta",
			Branch:    "feat/beta",
			Program:   "codex",
			CreatedAt: created,
			UpdatedAt: updated,
		},
	}
}

// snapshotServer stands up a REAL Unix-socket HTTP server that answers
// POST /v1/Snapshot exactly the way the daemon does — encoding the response
// through the shared apiproto.WriteEnvelope, the identical primitive
// daemon/httpserver.go uses — and returns a Client dialing it. Using the real
// envelope writer (not a hand-rolled body) is what makes the round-trip a
// genuine parity proof rather than a mock agreeing with itself.
func snapshotServer(t *testing.T, handle func(daemon.SnapshotRequest) apiproto.Envelope) *Client {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.SnapshotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, handle(req))
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return NewWithSocket(sockPath)
}

// TestClientSnapshot_RoundTripsStructsByteIdentically drives the real transport:
// the client dials a Unix socket, POSTs a Snapshot request, decodes the daemon's
// {data,error} envelope, and returns []session.InstanceData that is
// byte-identical to what the server put in. This is the client-side half of the
// #1592 PR2 byte-parity proof.
func TestClientSnapshot_RoundTripsStructsByteIdentically(t *testing.T) {
	want := richInstances()
	var gotReq daemon.SnapshotRequest
	c := snapshotServer(t, func(req daemon.SnapshotRequest) apiproto.Envelope {
		gotReq = req
		return apiproto.Success(daemon.SnapshotResponse{Instances: want})
	})

	got, err := c.Snapshot(daemon.SnapshotRequest{RepoID: "repo-x"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if gotReq.RepoID != "repo-x" {
		t.Fatalf("repo scoping lost: server saw RepoID=%q, want repo-x", gotReq.RepoID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded structs differ from server payload:\n got=%+v\nwant=%+v", got, want)
	}

	// The stronger claim: the client's result re-marshals to the exact bytes the
	// server's payload does. This is the byte-parity the envelope guarantees.
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("JSON not byte-identical after round-trip:\n got=%s\nwant=%s", gotJSON, wantJSON)
	}
}

// TestSnapshotNoSpawn_NoDaemon_FallsBackSignal verifies that with nothing
// listening the drop-in read returns daemon.ErrDaemonUnavailable — the exact
// sentinel the net/rpc twin returns — so the CLI read path falls back to disk
// rather than failing or spawning a daemon.
func TestSnapshotNoSpawn_NoDaemon_FallsBackSignal(t *testing.T) {
	// Point the config-dir resolver at an empty home so New() resolves a socket
	// path that no daemon is serving.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	got, err := SnapshotNoSpawn(daemon.SnapshotRequest{})
	if !errors.Is(err, daemon.ErrDaemonUnavailable) {
		t.Fatalf("want ErrDaemonUnavailable, got %v", err)
	}
	if got != nil {
		t.Fatalf("want nil instances on unavailable daemon, got %+v", got)
	}
}

// TestSnapshot_FailureEnvelope_SurfacesMessage verifies a daemon failure
// envelope (error != null) surfaces as a Go error carrying the daemon's message
// verbatim, byte-identical to what the net/rpc client would carry since both
// wrap the same controlServer error string.
func TestSnapshot_FailureEnvelope_SurfacesMessage(t *testing.T) {
	c := snapshotServer(t, func(daemon.SnapshotRequest) apiproto.Envelope {
		return apiproto.Failure("boom: repo not found")
	})

	_, err := c.Snapshot(daemon.SnapshotRequest{})
	if err == nil || err.Error() != "boom: repo not found" {
		t.Fatalf("want verbatim daemon message, got %v", err)
	}
}

// A mutation_committed envelope keeps surfacing its human-readable failure,
// while also preserving the durable outcome through the HTTP client boundary.
func TestFailureEnvelope_PreservesMutationCommittedOutcome(t *testing.T) {
	c := snapshotServer(t, func(daemon.SnapshotRequest) apiproto.Envelope {
		return apiproto.FailureWithCode(
			"task update committed, but scheduler reload failed",
			apiproto.ErrorCodeMutationCommitted,
		)
	})

	_, err := c.Snapshot(daemon.SnapshotRequest{})
	if err == nil {
		t.Fatal("want the post-commit failure to remain visible")
	}
	if !IsMutationCommitted(err) {
		t.Fatalf("want mutation-committed outcome, got %T: %v", err, err)
	}
	if err.Error() != "task update committed, but scheduler reload failed" {
		t.Fatalf("want verbatim daemon message, got %q", err)
	}
}
