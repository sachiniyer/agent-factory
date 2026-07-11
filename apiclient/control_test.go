package apiclient

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// routeServer stands up a real Unix-socket HTTP server that answers a single
// POST /v1/<method> route through the shared apiproto.WriteEnvelope — the exact
// primitive daemon/httpserver.go uses — and returns a Client dialing it. Using
// the real envelope writer makes each round-trip a genuine parity proof rather
// than a mock agreeing with itself, exactly like snapshotServer does for reads.
func routeServer(t *testing.T, method string, handle func(body []byte) apiproto.Envelope) *Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/"+method, func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, handle(body))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewWithSocket(sockPath)
}

// TestControlRoundTrips drives each control method the TUI now routes over HTTP
// through the real transport + envelope, asserting the request field the daemon
// saw and the value the client decoded back. It is the write-side twin of the
// PR2 read-side parity proof: the same structs go in and come out.
func TestControlRoundTrips(t *testing.T) {
	t.Run("CreateSession", func(t *testing.T) {
		var got daemon.CreateSessionRequest
		c := routeServer(t, "CreateSession", func(b []byte) apiproto.Envelope {
			_ = json.Unmarshal(b, &got)
			return apiproto.Success(daemon.CreateSessionResponse{
				Instance: session.InstanceData{Title: got.Title, Program: got.Program},
			})
		})
		inst, err := c.CreateSession(daemon.CreateSessionRequest{Title: "alpha", Program: "claude"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if got.Title != "alpha" || got.Program != "claude" {
			t.Fatalf("daemon saw %+v, want title=alpha program=claude", got)
		}
		if inst == nil || inst.Title != "alpha" {
			t.Fatalf("decoded instance = %+v, want Title=alpha", inst)
		}
	})

	t.Run("ArchiveSession returns path", func(t *testing.T) {
		c := routeServer(t, "ArchiveSession", func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.ArchiveSessionResponse{ArchivedPath: "/arch/alpha"})
		})
		path, err := c.ArchiveSession(daemon.ArchiveSessionRequest{Title: "alpha"})
		if err != nil || path != "/arch/alpha" {
			t.Fatalf("ArchiveSession = %q, %v; want /arch/alpha", path, err)
		}
	})

	t.Run("CreateTab returns resolved name", func(t *testing.T) {
		c := routeServer(t, "CreateTab", func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.CreateTabResponse{Name: "shell-2"})
		})
		name, err := c.CreateTab(daemon.CreateTabRequest{Title: "alpha", Shell: true})
		if err != nil || name != "shell-2" {
			t.Fatalf("CreateTab = %q, %v; want shell-2", name, err)
		}
	})

	t.Run("KillSession success", func(t *testing.T) {
		c := routeServer(t, "KillSession", func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.KillSessionResponse{OK: true})
		})
		if err := c.KillSession(daemon.KillSessionRequest{Title: "alpha"}); err != nil {
			t.Fatalf("KillSession: %v", err)
		}
	})

	t.Run("ResumeFromLimit rides internal route", func(t *testing.T) {
		c := routeServer(t, "ResumeFromLimit", func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.ResumeFromLimitResponse{OK: true})
		})
		if err := c.ResumeFromLimit(daemon.ResumeFromLimitRequest{Title: "alpha"}); err != nil {
			t.Fatalf("ResumeFromLimit: %v", err)
		}
	})

	t.Run("PauseStatusPoll rides internal route", func(t *testing.T) {
		c := routeServer(t, "PauseStatusPoll", func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.PauseStatusPollResponse{OK: true})
		})
		if err := c.PauseStatusPoll(daemon.PauseStatusPollRequest{Title: "alpha"}); err != nil {
			t.Fatalf("PauseStatusPoll: %v", err)
		}
	})
}

// TestSnapshotWithAlarms_CarriesAlarms verifies the TUI read path decodes both
// the session list AND the delivery-failure alarms from one response — the alarm
// is a field on the snapshot (#1238), not a side channel dropped by plain
// Snapshot.
func TestSnapshotWithAlarms_CarriesAlarms(t *testing.T) {
	since := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	c := routeServer(t, "Snapshot", func([]byte) apiproto.Envelope {
		return apiproto.Success(daemon.SnapshotResponse{
			Instances:      []session.InstanceData{{Title: "alpha"}},
			DeliveryAlarms: []daemon.DeliveryAlarm{{TaskName: "watch", Pending: 3, Since: since}},
		})
	})
	resp, err := c.SnapshotWithAlarms(daemon.SnapshotRequest{RepoID: "r"})
	if err != nil {
		t.Fatalf("SnapshotWithAlarms: %v", err)
	}
	if len(resp.Instances) != 1 || resp.Instances[0].Title != "alpha" {
		t.Fatalf("instances = %+v, want one alpha", resp.Instances)
	}
	if len(resp.DeliveryAlarms) != 1 || resp.DeliveryAlarms[0].Pending != 3 {
		t.Fatalf("alarms = %+v, want one pending=3", resp.DeliveryAlarms)
	}
}

// TestControlError_SurfacesEnvelopeMessage verifies a daemon failure envelope
// surfaces as a plain error carrying the daemon's message verbatim — and is NOT
// a TransportError, so the TUI's warm-up retry never spins on a real failure.
func TestControlError_SurfacesEnvelopeMessage(t *testing.T) {
	c := routeServer(t, "KillSession", func([]byte) apiproto.Envelope {
		return apiproto.Failure("session \"ghost\" not found")
	})
	err := c.KillSession(daemon.KillSessionRequest{Title: "ghost"})
	if err == nil || err.Error() != "session \"ghost\" not found" {
		t.Fatalf("want verbatim daemon message, got %v", err)
	}
	if IsTransportError(err) {
		t.Fatal("an envelope/application error must not be classified as a TransportError")
	}
}

// TestTransportError_OnUnreachableSocket verifies that a call to a socket with
// nothing listening yields a TransportError — the signal the TUI retries while a
// just-spawned daemon's HTTP socket finishes binding.
func TestTransportError_OnUnreachableSocket(t *testing.T) {
	c := NewWithSocket(filepath.Join(t.TempDir(), "does-not-exist.sock"))
	err := c.KillSession(daemon.KillSessionRequest{Title: "x"})
	if err == nil {
		t.Fatal("want an error dialing a dead socket")
	}
	if !IsTransportError(err) {
		t.Fatalf("want TransportError for an unreachable socket, got %T: %v", err, err)
	}
}
