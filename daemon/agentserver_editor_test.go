package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

func startEditorAgentServer(t *testing.T, hook func() (vscodeEndpoint, error)) (session.EditorRelayer, string) {
	t.Helper()
	dir := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", dir)
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), title: "probe", events: newEventsHub()}
	hs.editor.hook = hook
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	closeTCP, tcp, err := startTCPListener(hs.newMux(), cfg.ListenAddr, cfg, tokenGatePolicy{}, withoutWebShell, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeTCP.close() })
	as, err := session.NewRemoteAgentServer(session.AgentServerEndpoint{URL: "http://" + tcp.Addr, Token: tcp.Token}, "probe")
	require.NoError(t, err)
	er, ok := as.(session.EditorRelayer)
	require.True(t, ok)
	return er, dir
}

func TestAgentServerEditorRouteReportsReadyStartingAndMissing(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, agentproto.EditorStateReady},
		{errVSCodeStarting, agentproto.EditorStateStarting},
		{errVSCodeBinaryMissing, agentproto.EditorStateMissing},
	} {
		er, _ := startEditorAgentServer(t, func() (vscodeEndpoint, error) { return vscodeEndpoint{}, tc.err })
		st, err := er.EnsureEditor(context.Background())
		require.NoError(t, err)
		assert.Equal(t, tc.want, st.State)
		if tc.want == agentproto.EditorStateMissing {
			assert.Contains(t, st.Message, "install")
		}
	}
}

// The relay's editor target reaches the server-owned socket, byte for byte.
func TestAgentServerRelayEditorTargetReachesTheEditorSocket(t *testing.T) {
	sockDir := testguard.SocketTempDir(t)
	sock := filepath.Join(sockDir, "ed.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("editor-here")) })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	er, _ := startEditorAgentServer(t, func() (vscodeEndpoint, error) { return vscodeEndpoint{SocketPath: sock}, nil })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return er.DialEditor(ctx) },
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://editor.invalid/", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "editor-here", string(body))
}

// An editor that is not serving is a refusal the caller can report, and the target
// is never caller-chosen: a path or port alongside it is refused outright.
func TestAgentServerRelayEditorTargetRefusals(t *testing.T) {
	er, _ := startEditorAgentServer(t, func() (vscodeEndpoint, error) { return vscodeEndpoint{}, errVSCodeBinaryMissing })
	_, err := er.DialEditor(context.Background())
	require.Error(t, err, "no editor installed on the host")
}

func TestEvictRelayTransportAlsoDropsTheEditorTransport(t *testing.T) {
	m := &Manager{}
	key := daemonInstanceKey("repo", "title")
	m.relayTransports.Store(editorRelayTransportKey(key), relayTransportEntry{transport: &http.Transport{}})
	m.evictRelayTransport(key)
	_, ok := m.relayTransports.Load(editorRelayTransportKey(key))
	assert.False(t, ok)
}
