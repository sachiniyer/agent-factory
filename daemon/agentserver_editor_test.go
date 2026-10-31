package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sockpath"
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
		sup.namespace = hs.editorNamespace()
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

// A missing editor on a REMOTE session names the session's host and the install step
// for that host, not the local hint that points at the daemon's machine.
func TestRemoteEditorMissingMessageNamesTheHost(t *testing.T) {
	err := error(&remoteEditorMissingError{host: "build-box"})
	msg := err.Error()
	assert.Contains(t, msg, `"build-box"`)
	assert.Contains(t, msg, "not installed on this session's host")
	assert.Contains(t, msg, "Install it on that host")
	assert.NotEqual(t, vscodeInstallHint, msg)
	assert.ErrorIs(t, err, errVSCodeBinaryMissing, "still reads as a missing editor to callers that only ask that")

	// An unnamed backend (docker, hook, legacy sandbox.ssh) still says it is the host.
	assert.Contains(t, remoteEditorInstallHint(""), "this session's host")
	assert.NotContains(t, remoteEditorInstallHint(""), `""`)

	// The notice page the pane renders carries it, escaped.
	rec := httptest.NewRecorder()
	var missing *remoteEditorMissingError
	require.True(t, errors.As(err, &missing))
	writeVSCodeNoticePage(rec, missing.Error())
	assert.Contains(t, rec.Body.String(), "build-box")
	assert.NotContains(t, rec.Body.String(), "set vscode_server_binary in config.toml)", "not the local hint")
}

// Codex P1 (#5818): a host can run an af daemon AND a session's agent-server. The
// daemon's shutdown sweep reaps every owner record in its directory; the editor an
// agent-server started must never be in it (and the daemon's must never be in the
// agent-server's). Disjoint by construction: separate record namespaces.
func TestDaemonShutdownSweepNeverReapsAnAgentServersEditorAndViceVersa(t *testing.T) {
	shortAFHome(t)
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	mk := func(ns string) *vscodeSupervisor {
		v := newVSCodeSupervisor()
		v.namespace = ns
		v.configuredBinary = func() string { return binary }
		v.startGrace = 5 * time.Second
		t.Cleanup(v.Stop)
		return v
	}
	startOn := func(v *vscodeSupervisor, key, id string) *vscodeServer {
		_, err := v.ensureServerForInstance(key, id, t.TempDir())
		if err != nil && !errors.Is(err, errVSCodeStarting) {
			t.Fatalf("start editor %s: %v", id, err)
		}
		v.mu.Lock()
		defer v.mu.Unlock()
		s := v.servers[key]
		require.NotNil(t, s)
		return s
	}
	hs := &headlessServer{inst: &session.Instance{ID: "session-x"}}
	ns := hs.editorNamespace()
	require.NotEmpty(t, ns)
	require.NotEqual(t, vscodeNamespaceDirName(""), vscodeNamespaceDirName(ns))

	// Direction 1: the daemon's shutdown sweep (a restarted supervisor with empty
	// memory, as after an upgrade) leaves the agent-server's editor running.
	agent := mk(ns)
	agentEditor := startOn(agent, hs.agentEditorKey(), "session-x")
	daemonOld := mk("")
	daemonEditor := startOn(daemonOld, "daemon-key", "daemon-session")
	restarted := newVSCodeSupervisor() // the daemon after a restart: empty memory
	require.NoError(t, restarted.stopAllPersisted())
	select {
	case <-daemonEditor.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon's own sweep did not reap the daemon's editor, so the test proves nothing")
	}
	require.True(t, agentEditor.alive(), "the daemon's shutdown sweep reaped an editor an agent-server started")
	ownerFiles, err := allPersistedVSCodeOwnersIn(ns)
	require.NoError(t, err)
	require.Len(t, ownerFiles, 1, "the agent-server's owner record must be untouched")

	// Direction 2: the agent-server's sweep leaves a daemon editor running.
	daemonEditor2 := startOn(mk(""), "daemon-key-2", "daemon-session-2")
	agentRestarted := newVSCodeSupervisor()
	agentRestarted.namespace = ns
	require.NoError(t, agentRestarted.stopAllPersisted())
	select {
	case <-agentEditor.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent-server's own sweep did not reap its editor, so the test proves nothing")
	}
	require.True(t, daemonEditor2.alive(), "an agent-server's sweep reaped the host daemon's editor")
}

// The product, not just the harness, must fit a real macOS af home: the editor
// socket lives under ~/.agent-factory, and darwin's sun_path ceiling is 103 bytes
// (sockpath.Portable). The names are hashed to a fixed width, so only the home
// varies; this pins the arithmetic for realistic homes with headroom, for both the
// daemon's editor and a W5.4 agent-server's.
func TestEditorSocketPathFitsARealisticMacHomeWithHeadroom(t *testing.T) {
	const headroom = 12 // bytes to spare below the darwin ceiling
	hs := &headlessServer{inst: &session.Instance{ID: "0123456789abcdef0123456789abcdef"}}
	nonce := [4]byte{0xde, 0xad, 0xbe, 0xef}
	for _, home := range []string{
		"/Users/abcdefghijklmnopqrst/.agent-factory", // a 20-character macOS user
		"/Users/sachin/.agent-factory",
	} {
		for _, ns := range []string{"", hs.editorNamespace()} {
			path := filepath.Join(home, vscodeNamespaceDirName(ns), vscodeSocketFileName("workspace-"+hs.inst.ID, nonce))
			assert.LessOrEqual(t, len(path), sockpath.Portable-headroom,
				"%q (%d bytes) must leave %d bytes below the darwin limit %d", path, len(path), headroom, sockpath.Portable)
		}
	}
}

// #6092: an agent-server SIGKILLed on a remote host leaves its editor running (its
// own process group, no parent-death signal). The replacement server gets a fresh
// instance id, so the namespace is keyed on the repo + title the daemon passes on
// every launch, and the replacement's acquisition sweep reaps the old editor by its
// recorded identity.
func TestReplacementAgentServerReapsTheCrashedServersEditor(t *testing.T) {
	f := newCrashEditorFixture(t)
	crashed := f.server("launch-1", "feature")
	require.NoError(t, crashed.acquireEditorNamespace())
	orphan := f.start(crashed)

	// Another workspace on the same host: different title, so a different namespace.
	other := f.server("launch-x", "other")
	require.NoError(t, other.acquireEditorNamespace())
	bystander := f.start(other)

	replacement := f.server("launch-2", "feature")
	require.Equal(t, crashed.editorNamespace(), replacement.editorNamespace(), "the namespace must survive a new instance id")
	require.NotEqual(t, crashed.editorNamespace(), other.editorNamespace())

	// The crash: the kernel releases the dead process's lock.
	require.NoError(t, crashed.editor.lock.Close())
	require.NoError(t, replacement.acquireEditorNamespace())
	select {
	case <-orphan.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the crashed server's editor was not reaped by the replacement")
	}
	require.True(t, bystander.alive(), "another workspace's editor is untouched")
}

// Codex P2 (#6099): the sweep may only reap what a DEAD owner left. Two live
// servers must never share a namespace for editor starts: the one that does not hold
// the lock is refused, so when the first exits and a third sweeps, the second has
// no editor to lose — and once the second does take the lock, a later sweep by a
// third (after the second also exits) is the only way its editor is reaped.
func TestSweepNeverReapsALiveServersEditor(t *testing.T) {
	f := newCrashEditorFixture(t)
	first := f.server("launch-1", "feature")
	require.NoError(t, first.acquireEditorNamespace())
	f.start(first)

	// The second is alive while the first still is: it cannot own the namespace, so
	// it cannot start an editor.
	second := f.server("launch-2", "feature")
	require.ErrorIs(t, second.acquireEditorNamespace(), errEditorWorkspaceBusy)
	_, err := second.ensureEditor()
	require.ErrorIs(t, err, errEditorWorkspaceBusy, "a server without the namespace lock never starts an editor")

	// The first exits (lock released); the second now owns the namespace — its sweep
	// reaps the first's leftover (owner dead) and its own editor starts after.
	require.NoError(t, first.editor.lock.Close())
	require.NoError(t, second.acquireEditorNamespace())
	secondEditor := f.start(second)
	require.True(t, secondEditor.alive())

	// A third while the second is ALIVE: the lock is held, so it neither sweeps nor
	// starts, and the second's editor survives.
	third := f.server("launch-3", "feature")
	require.ErrorIs(t, third.acquireEditorNamespace(), errEditorWorkspaceBusy)
	require.True(t, secondEditor.alive(), "a live owner's editor is never swept")
}

type crashEditorFixture struct {
	t      *testing.T
	binary string
}

func newCrashEditorFixture(t *testing.T) *crashEditorFixture {
	shortAFHome(t)
	return &crashEditorFixture{t: t, binary: writeFakeVSCodeBinary(t, "code-server", nil)}
}

func (f *crashEditorFixture) server(id, title string) *headlessServer {
	hs := &headlessServer{inst: &session.Instance{ID: id}, repoPath: "/srv/repo", title: title}
	sup := hs.editorSupervisor()
	sup.configuredBinary = func() string { return f.binary }
	sup.startGrace = 5 * time.Second
	f.t.Cleanup(sup.Stop)
	return hs
}

func (f *crashEditorFixture) start(hs *headlessServer) *vscodeServer {
	sup := hs.editorSupervisor()
	_, err := sup.ensureServerForInstance(hs.agentEditorKey(), hs.inst.ID, f.t.TempDir())
	if err != nil && !errors.Is(err, errVSCodeStarting) {
		f.t.Fatalf("start editor for %s: %v", hs.inst.ID, err)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	s := sup.servers[hs.agentEditorKey()]
	require.NotNil(f.t, s)
	return s
}

// #6089: the remote start resolves its target before CloseTab can take the last VS
// Code tab away, and CloseTab's stop can run while the start is still in flight. The
// guard after the start re-asks whether the editor still has a tab; if not it stops
// the editor it just started.
func TestGuardRemoteEditorStart_StopsAnEditorWhoseTabWasClosed(t *testing.T) {
	m, repoID, title := newVSCodeCreateFixture(t)
	key := daemonInstanceKey(repoID, title)
	m.mu.Lock()
	inst := m.instances[key]
	m.mu.Unlock()
	require.NotNil(t, inst)
	_, err := m.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode"})
	require.NoError(t, err)

	stops := 0
	stop := func() { stops++ }

	// The tab exists: the editor is wanted and is left running.
	require.NoError(t, m.guardRemoteEditorStart(inst, key, title, stop))
	require.Zero(t, stops)

	// The last VS Code tab closes (its own stop already ran) while a start that had
	// resolved earlier finishes. The guard must stop what that start left behind.
	_, err = m.closeTabRequestedBy(CloseTabRequest{Title: title, RepoID: repoID, TabName: "vscode"}, "test")
	require.NoError(t, err)
	err = m.guardRemoteEditorStart(inst, key, title, stop)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VS Code tab")
	assert.Equal(t, 1, stops, "the editor started after the close was stopped")

	// And the pre-start check refuses to begin one at all.
	require.Error(t, m.remoteEditorWanted(inst, key, title))
}

// #6165 P1: CloseTabByIDWithCommit runs its commit callback under instance.mu, and
// the final remote-editor sweep inside it used to call locking Instance methods —
// a non-reentrant self-deadlock on EVERY last-VS-Code-tab close, local sessions
// included. The close runs under a deadline so a regression fails fast instead of
// hanging the suite. (A remote instance cannot be built with a fake relayer in this
// harness; the stopper is resolved before the callback for both kinds, so the local
// close exercises the same callback path.)
func TestCloseLastVSCodeTab_DoesNotDeadlockUnderTheInstanceLock(t *testing.T) {
	m, repoID, title := newVSCodeCreateFixture(t)
	_, err := m.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode"})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, cerr := m.closeTabRequestedBy(CloseTabRequest{Title: title, RepoID: repoID, TabName: "vscode"}, "test")
		done <- cerr
	}()
	select {
	case cerr := <-done:
		require.NoError(t, cerr)
	case <-time.After(20 * time.Second):
		t.Fatal("closing the last VS Code tab deadlocked: the commit callback re-entered instance.mu")
	}
	m.mu.Lock()
	inst := m.instances[daemonInstanceKey(repoID, title)]
	m.mu.Unlock()
	require.False(t, instanceHasVSCodeTab(inst))
}
