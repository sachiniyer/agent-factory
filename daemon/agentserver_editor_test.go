package daemon

import (
	"context"
	"errors"
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

// Codex P1 (#5818): a relayed editor has no SocketPath on the daemon host but is
// server-owned all the same; classifying it as an arbitrary web target made the
// proxy answer every editor request with a 400. It gets the editor treatment, and
// nothing that merely LOOKS like it does.
func TestRelayedEditorTargetIsClassifiedAsAnEditorAndNothingElseIs(t *testing.T) {
	relayed := webTabTarget{URL: vscodeUpstreamURL, RelayedEditor: true, Kind: session.TabKindVSCode}
	assert.True(t, relayed.isServerOwnedEditor())
	assert.True(t, webTabTarget{URL: vscodeUpstreamURL, SocketPath: "/x.sock", Kind: session.TabKindVSCode}.isServerOwnedEditor(),
		"the local socket editor keeps its classification")

	// A web tab can never claim the exemption, flag or not: its URL names a real host.
	assert.False(t, webTabTarget{URL: "http://vscode.invalid", RelayedEditor: true, Kind: session.TabKindWeb}.isServerOwnedEditor())
	assert.False(t, webTabTarget{URL: "http://vscode.invalid", Kind: session.TabKindVSCode}.isServerOwnedEditor(),
		"a vscode target with neither a socket nor a relay is an arbitrary URL")
}

// Codex P1 (#5818): sessions on one host share an ssh user and an af home, so an
// agent-server must end ITS editor and no other. Two agent-servers on one host each
// run an editor; tearing one down leaves the other serving and its owner record
// intact.
func TestAgentServerTeardownLeavesAnotherSessionsEditorAlone(t *testing.T) {
	shortAFHome(t)
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	newServer := func(id string) *headlessServer {
		hs := &headlessServer{inst: &session.Instance{ID: id}}
		sup := newVSCodeSupervisor()
		sup.configuredBinary = func() string { return binary }
		sup.startGrace = 5 * time.Second
		hs.editor.once.Do(func() { hs.editor.sup = sup })
		t.Cleanup(sup.Stop)
		return hs
	}
	start := func(hs *headlessServer) *vscodeServer {
		_, err := hs.editorSupervisor().ensureServerForInstance(hs.agentEditorKey(), hs.inst.ID, t.TempDir())
		if err != nil && !errors.Is(err, errVSCodeStarting) {
			t.Fatalf("start editor for %s: %v", hs.inst.ID, err)
		}
		hs.editorSupervisor().mu.Lock()
		defer hs.editorSupervisor().mu.Unlock()
		s := hs.editorSupervisor().servers[hs.agentEditorKey()]
		require.NotNil(t, s)
		return s
	}
	a, b := newServer("session-a"), newServer("session-b")
	require.NotEqual(t, a.agentEditorKey(), b.agentEditorKey(), "owner records must not share a key across sessions")
	ea, eb := start(a), start(b)
	require.True(t, ea.alive())
	require.True(t, eb.alive())

	require.NoError(t, a.stopEditor())

	select {
	case <-ea.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("session A's own editor was not stopped")
	}
	require.True(t, eb.alive(), "session B's editor, started by a different agent-server on the same host, must survive A's teardown")

	// A restarted agent-server for A (empty memory) still reaches only A's record.
	a2 := newServer("session-a")
	require.NoError(t, a2.stopEditor())
	require.True(t, eb.alive())
}

// An agent-server with no stable workspace id owns no editor, so it must not act
// as a wildcard over the shared home.
func TestAgentServerWithoutAStableIDStopsNothing(t *testing.T) {
	hs := &headlessServer{inst: &session.Instance{}}
	require.NoError(t, hs.stopEditor())
	_, err := hs.ensureEditor()
	require.Error(t, err)
}
