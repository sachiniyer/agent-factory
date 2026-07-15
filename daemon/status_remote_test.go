package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// registerStartedRemote registers a started instance whose agent-server is the
// REAL remoteAgentServer client (#1592 Phase 4) pointed at `url`, so the daemon
// poll drives it over actual HTTP exactly as it does a docker/ssh session. The
// backend is a plain FakeBackend: the remote runtime never consults it, so any
// liveness the poll settles on came from the REST probes, not a tmux stand-in.
func registerStartedRemote(t *testing.T, m *Manager, repoID, repoPath, title, url string, status session.Status) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:             title,
		Path:              repoPath,
		Program:           "claude",
		RemoteAgentServer: &session.AgentServerEndpoint{URL: url, Token: "test-token"},
	})
	if err != nil {
		t.Fatalf("NewInstance(remote): %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(status)
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// TestRefreshStatuses_UnreachableRemoteMarkedLost is the #1782 regression. A
// remote session's agent-server dies (container killed, ssh forward dropped), so
// every REST probe fails with ECONNREFUSED. The poll used to return on the first
// Snapshot error before any liveness check ran, leaving the session pinned at its
// last-known Running/Ready forever — the TUI showed a healthy row for a session
// that was gone, and only manual intervention surfaced it. The failed Snapshot
// must now be confirmed with an independent Alive() probe and settle to Lost.
//
// Port 1 on loopback has no listener, so both probes are refused immediately —
// the "agent-server is unreachable" shape, with no timeout to wait out.
func TestRefreshStatuses_UnreachableRemoteMarkedLost(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Start from Running to prove the pass actively transitions it rather than
	// merely leaving a pre-set status untouched.
	registerStartedRemote(t, manager, repoID, repoPath, "remote-gone", "http://127.0.0.1:1", session.Running)

	manager.RefreshStatuses()

	inst := manager.instances[daemonInstanceKey(repoID, "remote-gone")]
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("in-memory liveness = %v, want LiveLost (an unreachable agent-server must not keep reading as Running)", got)
	}
	// Persisted too, so the status survives a daemon reload and the restore loop
	// can find it — Lost, not Dead: no kill intent, still recovery-eligible (#1108).
	if got := persistedStatus(t, repoID, "remote-gone"); got != session.Lost {
		t.Fatalf("persisted status = %v, want Lost", got)
	}
}

// TestRefreshStatuses_RemoteSnapshotErrorButAliveKeepsStatus pins the other half
// of the #1782 fix: a Snapshot error is not on its own proof of death. Here the
// agent-server is REACHABLE and reports itself alive, but the snapshot probe
// fails — a transient blip. The poll must leave the status for the next tick
// rather than marking a healthy session Lost, which is why the fix confirms with
// Alive() instead of trusting the Snapshot error outright.
func TestRefreshStatuses_RemoteSnapshotErrorButAliveKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/snapshot":
			// The blip: an error envelope, exactly as the real agent-server
			// reports a failed capture.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "capture failed"},
			})
		case "/v1/agent/alive":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"alive": true},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	registerStartedRemote(t, manager, repoID, repoPath, "remote-blip", srv.URL, session.Running)

	manager.RefreshStatuses()

	inst := manager.instances[daemonInstanceKey(repoID, "remote-blip")]
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("in-memory liveness = %v, want LiveRunning (a still-alive session must not be marked Lost on one failed snapshot)", got)
	}
}
