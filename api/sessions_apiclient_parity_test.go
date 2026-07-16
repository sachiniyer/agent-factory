package api

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// parityInstances is a fixture with a spread of populated fields so byte-parity
// is proven over real content, not two empty lists.
func parityInstances() []session.InstanceData {
	created := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	return []session.InstanceData{
		{
			ID:        "id-aaaa",
			Title:     "alpha",
			Path:      "/wt/alpha",
			Branch:    "feat/alpha",
			Status:    session.Status(1),
			Liveness:  session.Liveness(2),
			Height:    40,
			Width:     120,
			CreatedAt: created,
			UpdatedAt: created,
			Program:   "claude",
			TmuxName:  "af_alpha",
			Tabs:      []session.TabData{{Name: "agent"}},
		},
		{
			ID:        "id-bbbb",
			Title:     "beta",
			Path:      "/wt/beta",
			Branch:    "feat/beta",
			Program:   "codex",
			CreatedAt: created,
			UpdatedAt: created,
		},
	}
}

// renderListJSON runs `sessions list --json` (the enveloped read path) against
// whatever snapshotViaDaemon is currently wired to and returns its exact stdout
// bytes.
func renderListJSON(t *testing.T) string {
	t.Helper()
	prevEnvelope := envelopeOutput
	prevRepo := repoFlag
	envelopeOutput = true
	repoFlag = ""
	t.Cleanup(func() { envelopeOutput = prevEnvelope; repoFlag = prevRepo })

	return captureStdout(t, func() {
		if err := sessionsListCmd.RunE(sessionsListCmd, nil); err != nil {
			t.Fatalf("sessions list --json: %v", err)
		}
	})
}

// TestSessionsListJSON_ByteParity_APIClientVsNetRPC is the #1592 PR2 gate: the
// `af sessions list --json` output is BYTE-IDENTICAL whether the read is sourced
// through the new apiclient (HTTP over daemon-http.sock) or the net/rpc path.
//
// The net/rpc side is represented by the exact value daemon.SnapshotNoSpawn
// returns on success — its whole body is `return resp.Instances` — while the
// apiclient side is a REAL HTTP round-trip: a live Unix-socket server encodes
// that same slice through the daemon's shared apiproto envelope, apiclient
// dials it, decodes, and hands the result to the identical rendering path. Equal
// stdout bytes prove the transport swap is invisible to the CLI surface.
func TestSessionsListJSON_ByteParity_APIClientVsNetRPC(t *testing.T) {
	// SocketTempDir, not t.TempDir: this test binds a real daemon-http.sock inside
	// this home, and t.TempDir's test-name-bearing path overruns sun_path on macOS
	// (#1940).
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	canned := parityInstances()

	// --- net/rpc side: SnapshotNoSpawn's success output is exactly its input. ---
	prev := snapshotViaDaemon
	snapshotViaDaemon = func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return canned, nil
	}
	netrpcJSON := renderListJSON(t)
	snapshotViaDaemon = prev

	// --- apiclient side: a real daemon-http.sock round-trip through the envelope. ---
	sockPath, err := daemon.DaemonHTTPSocketPath()
	require.NoError(t, err)
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.SnapshotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SnapshotResponse{Instances: canned}))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	prev = snapshotViaDaemon
	snapshotViaDaemon = apiclient.SnapshotNoSpawn
	apiclientJSON := renderListJSON(t)
	snapshotViaDaemon = prev

	require.NotEmpty(t, apiclientJSON)
	require.Contains(t, apiclientJSON, `"alpha"`, "sanity: fixture must actually render")
	require.Equal(t, netrpcJSON, apiclientJSON,
		"sessions list --json must be byte-identical between the apiclient and net/rpc read paths")
}
