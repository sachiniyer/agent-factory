package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// A sandbox credential learns the daemon's host-pick capabilities from the one
// route it is admitted to (/v1/Snapshot), because /v1/health is denied to its
// scope. The client-side contract is that every surface checks the advertised
// bit before sending a host pick and refuses otherwise, relying on the bit as
// proof the daemon HONORS the field. These tests drive the REAL tasks-add CLI
// path through the real routedPing reconstruction (api/sessions_hostpick.go)
// against a fake daemon that mirrors what the real one sends a sandbox
// credential, so the preflight-vs-wire contract is exercised end to end.

// reproServer is a fake daemon that mirrors what the real daemon sends a SANDBOX
// credential: a Snapshot carrying the owner's Caller and a configurable
// capability projection, plus a canned AddTask success. It records what the CLI
// sent so a test can prove a host pick was or was not put on the wire.
type reproServer struct {
	mu        sync.Mutex
	caps      *daemon.SnapshotCapabilities
	snapshots int
	adds      []string
}

func newReproServer(t *testing.T, caps *daemon.SnapshotCapabilities) (*reproServer, string) {
	t.Helper()
	s := &reproServer{caps: caps}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.mu.Unlock()
		switch r.URL.Path {
		case "/v1/Snapshot":
			s.snapshots++
			data := daemon.SnapshotResponse{
				Instances: []session.InstanceData{
					{ID: "me-id", Title: "me"},
					{ID: "peer-id", Title: "peer"},
				},
				Caller:       "me-id",
				Capabilities: s.caps,
			}
			_ = apiproto.WriteEnvelope(w, apiproto.Success(data))
		case "/v1/AddTask":
			s.adds = append(s.adds, string(body))
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.AddTaskResponse{OK: true}))
		default:
			_ = apiproto.WriteEnvelope(w, apiproto.Success(map[string]any{"ok": true}))
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *reproServer) addCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.adds)
}

func (s *reproServer) snapshotCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshots
}

// addHostFromFirstAdd decodes the AddTask request body and returns the task's
// Host field, so a test can prove the pick did or did not reach the wire.
func (s *reproServer) addHost(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	bodies := append([]string(nil), s.adds...)
	s.mu.Unlock()
	require.NotEmpty(t, bodies, "no /v1/AddTask request was recorded")
	var req struct {
		Task task.Task `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(bodies[0]), &req))
	return req.Task.Host
}

// runSandboxTasksAdd drives the real tasks-add command against url with the
// sandbox token and a host pick, exactly as an in-sandbox `af` would.
func runSandboxTasksAdd(t *testing.T, url, host string) error {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir()) // never read a local tasks.json
	oldURL, oldTok, oldRepo := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken, repoFlag
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken, repoFlag = url, "sandbox-secret", ""
	t.Cleanup(func() {
		apiclient.FlagDaemonURL, apiclient.FlagDaemonToken, repoFlag = oldURL, oldTok, oldRepo
	})
	return runWithFlags(t, tasksAddCmd, map[string]string{
		"name":           "n",
		"prompt":         "p",
		"cron":           "0 * * * *",
		"target-session": "me",
		"host":           host,
	})
}

// With the truthful HostPick-only projection (what the daemon ships for a
// sandbox credential after the fix), `af tasks add --host` is REFUSED at the
// preflight before anything is sent: the snapshot's snapshot-level TaskHost bit
// is false, so the host-pick gate fails closed and no task is created. This is
// the designed fail-closed protection the bit was introduced to produce.
func TestSandboxTasksAddHostIsRefusedWhenOnlyHostPickIsAdvertised(t *testing.T) {
	srv, url := newReproServer(t, &daemon.SnapshotCapabilities{HostPick: true})

	err := runSandboxTasksAdd(t, url, "box1")
	require.Error(t, err, "a sandbox scope that does not honor --host on tasks must refuse it")
	msg := err.Error()
	assert.Contains(t, msg, "does not support --host here", "the surface names the flag")
	assert.Contains(t, msg, `"box1"`, "the refused pick is named")
	assert.Contains(t, msg, "store the task without a host", "the TaskHost surface's 'Ignored' wording")
	assert.Contains(t, msg, "predates version reporting", "a Snapshot-reconstructed Ping carries no version")
	assert.Contains(t, msg, "nothing was created or changed", "a preflight refusal leaves nothing mutated")
	assert.Equal(t, 0, srv.addCount(), "no /v1/AddTask request may be sent once the bit is withheld")
	assert.GreaterOrEqual(t, srv.snapshotCount(), 1, "the preflight read the capabilities from /v1/Snapshot")
}

// Anti-vacuous and forward-looking guard: the moment a sandbox Snapshot
// advertises TaskHost, the SAME preflight passes and the host pick is sent on
// the wire to /v1/AddTask. The daemon's sandbox AddTask path then clears
// Task.Host (stampSandboxTask) and returns success, so the pick is silently
// dropped — which is exactly why advertising TaskHost is a false claim for the
// scope. If sandbox-scoped task host picks are ever wanted for real, the stamp
// must honor the field and this test's pick ends up honored; until then it pins
// that the projection — not any other code — gates whether the pick is sent.
func TestSandboxTasksAddHostIsSentWhenTaskHostIsAdvertised(t *testing.T) {
	srv, url := newReproServer(t, &daemon.SnapshotCapabilities{
		HostPick: true, DeliverHostPick: true, TaskHost: true,
	})

	err := runSandboxTasksAdd(t, url, "box1")
	require.NoError(t, err, "with TaskHost advertised the preflight passes and the add is sent")
	assert.Equal(t, 1, srv.addCount(), "the pick was put on the wire to /v1/AddTask")
	assert.Equal(t, "box1", srv.addHost(t), "the caller's host pick reached the request body")
}
