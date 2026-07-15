package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The fake code-server. Rather than shipping a fixture binary or depending on a
// real code-server (which no CI box has — the whole point of the feature is that
// af DETECTS one), these tests re-exec the test binary itself as the editor: a
// tiny shell script on PATH execs it with fakeVSCodeEnv set, TestMain routes that
// to fakeVSCodeServerMain, and it serves real HTTP and real WebSockets on the
// --bind-addr it was handed. That exercises the actual spawn → probe → proxy
// path, including a genuine WS upgrade, with no external dependency.

const (
	// fakeVSCodeEnv routes a re-exec of the test binary to fakeVSCodeServerMain.
	fakeVSCodeEnv = "AF_TEST_FAKE_CODE_SERVER"
	// fakeVSCodeArgsEnv names a file the fake writes its argv to, so a test can
	// assert what the daemon actually asked the editor to do.
	fakeVSCodeArgsEnv = "AF_TEST_FAKE_CODE_SERVER_ARGS"
	// fakeVSCodeHangEnv makes the fake start but never listen, standing in for a
	// cold editor that outruns the start grace.
	fakeVSCodeHangEnv = "AF_TEST_FAKE_CODE_SERVER_HANG"
	// fakeVSCodeMarker is served at the root so a test can prove the response
	// came from the editor and through the proxy.
	fakeVSCodeMarker = "AF_FAKE_CODE_SERVER_OK"
)

// fakeVSCodeServerMain is the fake editor's entry point, running in a re-exec of
// the test binary. It mimics the parts of code-server this feature depends on:
// it binds the --bind-addr it is given, serves the worktree it is given, and
// upgrades WebSockets on any path (as code-server does).
func fakeVSCodeServerMain() {
	args := os.Args[1:]
	if path := os.Getenv(fakeVSCodeArgsEnv); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(args, "\n")), 0o600)
	}
	if os.Getenv(fakeVSCodeHangEnv) != "" {
		// Never listen: the supervisor must report "starting", leave us alive, and
		// not respawn us.
		select {}
	}

	// Parse both dialects. valueFlags is what makes the positional worktree
	// findable: only these consume the next argv entry, so a bare word after a
	// BOOLEAN flag (--disable-telemetry) is correctly read as the worktree.
	valueFlags := map[string]bool{
		"--bind-addr": true, "--auth": true, "--config": true,
		"--host": true, "--port": true,
	}
	addr, host, port, worktree := "", "", "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if valueFlags[a] && i+1 < len(args) {
			switch a {
			case "--bind-addr":
				addr = args[i+1]
			case "--host":
				host = args[i+1]
			case "--port":
				port = args[i+1]
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		worktree = a
	}
	if addr == "" && host != "" && port != "" {
		addr = net.JoinHostPort(host, port) // the openvscode-server dialect
	}
	if addr == "" {
		fmt.Fprintln(os.Stderr, "fake code-server: no --bind-addr / --host+--port")
		os.Exit(2)
	}

	// Optionally fork a child that IGNORES SIGTERM and outlives us, in our process
	// group — the shape of code-server's extension/pty hosts, and what proves the
	// teardown escalates to the whole group rather than stopping at the leader.
	if pidFile := os.Getenv("AF_TEST_ORPHAN_CHILD_PIDFILE"); pidFile != "" {
		child := exec.Command("sh", "-c", "trap '' TERM; echo $$ > "+pidFile+"; while :; do sleep 0.2; done")
		if err := child.Start(); err == nil {
			go func() { _ = child.Wait() }()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		// Echo one message, so the test proves a real bidirectional relay rather
		// than just a 101 status.
		typ, data, err := c.Read(context.Background())
		if err != nil {
			return
		}
		_ = c.Write(context.Background(), typ, data)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Echo back the request path, the worktree served, and the Host the proxy
		// forwarded — the last is what code-server's origin check reads.
		fmt.Fprintf(w, "<html><body>%s worktree=%s path=%s xfh=%s xfp=%s</body></html>",
			fakeVSCodeMarker, worktree, r.URL.Path, r.Header.Get("X-Forwarded-Host"),
			r.Header.Get("X-Forwarded-Prefix"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake code-server: listen %s: %v\n", addr, err)
		os.Exit(2)
	}
	_ = srv.Serve(ln)
}

// writeFakeVSCodeBinary writes an executable shell script that re-execs this test
// binary as the fake editor, and returns its path. name sets the file name, which
// is what flavorForBinary reads to pick the CLI dialect.
func writeFakeVSCodeBinary(t *testing.T, name string, env map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	var exports strings.Builder
	exports.WriteString(fakeVSCodeEnv + "=1 ")
	for k, v := range env {
		exports.WriteString(k + "=" + shellQuote(v) + " ")
	}
	script := "#!/bin/sh\nexec env " + exports.String() + shellQuote(self) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the fake editor: %v", err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newVSCodeFixture builds a manager with one started local instance holding a
// vscode tab, wired to the fake editor, and returns the manager, the instance's
// stable id, the tab index, and the worktree the editor should be serving.
func newVSCodeFixture(t *testing.T, binary string) (m *Manager, sessionID string, tabIdx int, worktree string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const title = "vscodeproxy"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	manager.vscode.configuredBinary = func() string { return binary }
	// Keep the suite fast: the real grace is tuned for a cold Node start.
	manager.vscode.startGrace = 5 * time.Second
	manager.vscode.cooldown = 50 * time.Millisecond
	t.Cleanup(manager.vscode.Stop)

	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "vscode"}); err != nil {
		t.Fatalf("CreateTab(vscode): %v", err)
	}
	// The tab fixture's instance is tmux-mocked and never materializes its
	// worktree on disk, but a real editor is a real process with a real cwd, so
	// the directory has to exist for it to start.
	wt := inst.GetWorktreePath()
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("creating the fixture worktree: %v", err)
	}
	return manager, inst.ID, 1, wt
}

// TestVSCodeTab_SpawnsEditorAndProxiesIt is the headline test: creating a vscode
// tab and requesting its proxy path spawns the editor on a LOOPBACK port with the
// session's worktree, and the daemon proxies its response back.
func TestVSCodeTab_SpawnsEditorAndProxiesIt(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "argv")
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeArgsEnv: argsFile})
	manager, id, idx, worktree := newVSCodeFixture(t, binary)

	mux := newHTTPMux(&controlServer{manager: manager})
	body := getVSCodeProxy(t, mux, id, idx, "")
	if !strings.Contains(body, fakeVSCodeMarker) {
		t.Fatalf("proxied body %q is missing the editor's marker", body)
	}
	// The editor served the SESSION's worktree, not some other directory.
	if !strings.Contains(body, "worktree="+worktree) {
		t.Fatalf("proxied body %q does not report worktree=%s", body, worktree)
	}
	// The proxy stripped its own prefix: code-server always listens at the root
	// and derives its relative asset URLs from the path depth it sees, so a
	// request for the tab root must reach the editor as "/".
	if !strings.Contains(body, "path=/") {
		t.Fatalf("proxied body %q did not reach the editor at the root path", body)
	}

	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading the editor argv: %v", err)
	}
	args := string(argv)
	// --auth none is only safe because the listener is loopback-only.
	if !strings.Contains(args, "--auth\nnone") {
		t.Errorf("editor argv %q is missing --auth none", args)
	}
	if !strings.Contains(args, worktree) {
		t.Errorf("editor argv %q is missing the worktree argument", args)
	}
	bindAddr := argvValue(args, "--bind-addr")
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		t.Fatalf("editor --bind-addr %q is not host:port: %v", bindAddr, err)
	}
	// The security property: never 0.0.0.0, never a routable interface.
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("editor bound %q, want a loopback address", bindAddr)
	}
}

// TestVSCodeTab_RelaysWebSocketUpgrade proves the proxy relays a real WS upgrade.
// code-server is unusable without it — the editor's entire client/server channel
// is a WebSocket — and a recorder-based test cannot prove it (httptest.Recorder
// implements no Hijacker), so this drives a real listener end to end.
func TestVSCodeTab_RelaysWebSocketUpgrade(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, idx, _ := newVSCodeFixture(t, binary)

	srv := httptest.NewServer(newHTTPMux(&controlServer{manager: manager}))
	defer srv.Close()

	// Warm the editor: the first request is what spawns it.
	waitForVSCodeReady(t, srv.Client(), srv.URL+vscodeProxyPath(id, idx, ""))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + vscodeProxyPath(id, idx, "ws")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS upgrade through the proxy failed: %v", err)
	}
	defer c.CloseNow()

	const payload = "af-vscode-ws"
	if err := c.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("WS write: %v", err)
	}
	_, got, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("WS read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("WS echo = %q, want %q", got, payload)
	}
}

// TestVSCodeTab_ForwardsHostForOriginCheck locks the header code-server's origin
// check depends on. code-server resolves its own host from X-Forwarded-Host
// before Host and answers 403 to every WS upgrade whose Origin disagrees, so the
// proxy MUST forward the browser-facing host. This is invisible in normal use
// until WebSockets stop connecting, hence the explicit lock.
func TestVSCodeTab_ForwardsHostForOriginCheck(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, idx, _ := newVSCodeFixture(t, binary)

	mux := newHTTPMux(&controlServer{manager: manager})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, idx, ""), nil)
	req.Host = "af.example.test:8443"
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "xfh=af.example.test:8443") {
		t.Fatalf("proxy did not forward the browser-facing host to the editor: %s", rec.Body.String())
	}
}

// TestVSCodeTab_MissingBinaryRendersInstallHint: a machine with no editor
// installed is a normal state. Creating the tab still succeeds and the pane
// renders an actionable install hint — never a crash or a bare error.
func TestVSCodeTab_MissingBinaryRendersInstallHint(t *testing.T) {
	manager, id, idx, _ := newVSCodeFixture(t, "")
	// An empty PATH plus no configured binary is "nothing installed". Emptied
	// AFTER the fixture, which shells out to git to build the repo.
	t.Setenv("PATH", t.TempDir())

	mux := newHTTPMux(&controlServer{manager: manager})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, idx, ""), nil)
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "code-server is not installed") {
		t.Fatalf("pane body %q does not explain that code-server is missing", body)
	}
	if !strings.Contains(body, "https://github.com/coder/code-server#install") {
		t.Fatalf("pane body %q does not link the install instructions", body)
	}
	if !strings.Contains(body, "vscode_server_binary") {
		t.Fatalf("pane body %q does not mention the config escape hatch", body)
	}
}

// TestVSCodeSupervisor_ReusesOneEditorPerSession: the editor is keyed by SESSION,
// so a second vscode tab (and every extra pane) shares one process rather than
// starting a second one.
func TestVSCodeSupervisor_ReusesOneEditorPerSession(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)
	worktree := t.TempDir()

	first, err := v.ensureServer("k", worktree)
	if err != nil {
		t.Fatalf("first ensureServer: %v", err)
	}
	second, err := v.ensureServer("k", worktree)
	if err != nil {
		t.Fatalf("second ensureServer: %v", err)
	}
	if first != second {
		t.Fatalf("ensureServer started a second editor (%s != %s); it must reuse one per session", first, second)
	}
	if got := len(v.servers); got != 1 {
		t.Fatalf("supervisor holds %d editors, want 1", got)
	}
}

// TestVSCodeSupervisor_StopForKillsTheChild: tab delete / archive / kill must
// leave no editor behind.
func TestVSCodeSupervisor_StopForKillsTheChild(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	if _, err := v.ensureServer("k", t.TempDir()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	server := v.servers["k"]
	pid := server.cmd.Process.Pid

	v.stopFor("k")

	if len(v.servers) != 0 {
		t.Fatalf("stopFor left the editor registered")
	}
	assertProcessGone(t, pid)
	// The port is released, i.e. nothing is still listening on it.
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(server.host, strconv.Itoa(server.port)), 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatalf("something is still listening on the stopped editor's port %d", server.port)
	}
}

// TestVSCodeSupervisor_StopKillsEveryChild: daemon shutdown leaves no orphans.
func TestVSCodeSupervisor_StopKillsEveryChild(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	var pids []int
	for _, key := range []string{"a", "b"} {
		if _, err := v.ensureServer(key, t.TempDir()); err != nil {
			t.Fatalf("ensureServer(%s): %v", key, err)
		}
		pids = append(pids, v.servers[key].cmd.Process.Pid)
	}

	v.Stop()

	for _, pid := range pids {
		assertProcessGone(t, pid)
	}
	// A stopped supervisor refuses to spawn again, so a shutdown race can't
	// resurrect an editor after teardown.
	if _, err := v.ensureServer("c", t.TempDir()); err == nil {
		t.Fatalf("ensureServer succeeded after Stop; it must refuse")
	}
}

// TestVSCodeSupervisor_RespawnsAfterDeath is the self-heal: a crashed editor is
// replaced on the next request rather than needing the tab recreated.
func TestVSCodeSupervisor_RespawnsAfterDeath(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)
	worktree := t.TempDir()

	first, err := v.ensureServer("k", worktree)
	if err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	dead := v.servers["k"]
	// Kill it the way a crash would, out from under the supervisor.
	_ = syscall.Kill(-dead.cmd.Process.Pid, syscall.SIGKILL)
	<-dead.exited
	// The cooldown gates FAILED spawns, not deaths; make sure it can't mask this.
	v.cooldown = 0

	second, err := v.ensureServer("k", worktree)
	if err != nil {
		t.Fatalf("ensureServer after the editor died: %v", err)
	}
	if second == first {
		t.Fatalf("supervisor reused the dead editor's URL %s", second)
	}
	if !v.servers["k"].alive() {
		t.Fatalf("the respawned editor is not alive")
	}
}

// TestVSCodeSupervisor_StillStartingIsNotAFailure: an editor that outruns the
// start grace must be left running and reported as starting — not killed, and not
// respawned on the next request (which would restart the clock forever).
func TestVSCodeSupervisor_StillStartingIsNotAFailure(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeHangEnv: "1"})
	v := newTestVSCodeSupervisor(t, binary)
	v.startGrace = 200 * time.Millisecond
	worktree := t.TempDir()

	if _, err := v.ensureServer("k", worktree); !errors.Is(err, errVSCodeStarting) {
		t.Fatalf("ensureServer err = %v, want errVSCodeStarting", err)
	}
	server := v.servers["k"]
	if server == nil || !server.alive() {
		t.Fatalf("a slow-starting editor was killed; it must be left to finish starting")
	}
	pid := server.cmd.Process.Pid

	// The SAME worktree again: still starting, and still the same process. A
	// respawn here would restart the start clock on every refresh and the editor
	// would never come up.
	if _, err := v.ensureServer("k", worktree); !errors.Is(err, errVSCodeStarting) {
		t.Fatalf("second ensureServer err = %v, want errVSCodeStarting", err)
	}
	if got := v.servers["k"]; got == nil || got.cmd.Process.Pid != pid {
		t.Fatalf("the still-starting editor was respawned instead of waited on")
	}
}

// TestVSCodeSupervisor_ScrubsSessionMarkers is the `af doctor --fix` guard.
// The daemon inherits its environment from whatever autostarted it, so it can be
// carrying AF_SESSION/AF_HOME from a TUI running inside an af_ tmux pane. Those
// markers are fixed in /proc/<pid>/environ at exec and can never be shed, and
// doctor --fix kills any marked process whose session is dead — which would kill
// a perfectly healthy editor. The child must therefore carry neither marker.
func TestVSCodeSupervisor_ScrubsSessionMarkers(t *testing.T) {
	t.Setenv(tmux.EnvMarkerSession, "af_somesession")
	t.Setenv(tmux.EnvMarkerHome, "/some/af/home")

	env := vscodeChildEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, tmux.EnvMarkerSession+"=") {
			t.Errorf("the editor's env carries %s; doctor --fix would attribute it to a session and kill it", kv)
		}
		if strings.HasPrefix(kv, tmux.EnvMarkerHome+"=") {
			t.Errorf("the editor's env carries %s; doctor --fix would attribute it to this af home and kill it", kv)
		}
	}
	// It is a scrub, not a wipe: everything else the editor needs still reaches it.
	if !hasEnvKey(env, "PATH") {
		t.Errorf("the editor's env lost PATH")
	}
}

// TestVSCodeSupervisor_ReaperCannotSeeTheEditor documents and locks WHY the
// daemon-spawned editor needs no reaper allowlist: the tmux teardown reaper
// captures a pane's process-tree descendants and its pane-SID members, and an
// editor spawned by the daemon is neither — it is the daemon's child, in the
// daemon's own kernel session. The property that makes that true is that the
// editor is spawned by the daemon rather than inside a tmux pane, and that it
// keeps its own process GROUP (Setpgid) without joining a pane's SESSION (no
// Setsid), so this asserts exactly that.
func TestVSCodeSupervisor_ReaperCannotSeeTheEditor(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)
	if _, err := v.ensureServer("k", t.TempDir()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	pid := v.servers["k"].cmd.Process.Pid

	// Its own process group (so teardown can signal the whole editor tree)...
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != pid {
		t.Fatalf("editor pgid = %d, want its own group (%d)", pgid, pid)
	}
	// ...but the DAEMON's kernel session, never a tmux pane's. The reaper's
	// SessionMembers rule keys on the pane root's SID, so sharing the test
	// process's SID (not a pane's) is what keeps the editor invisible to it.
	sid, err := unixGetsid(pid)
	if err != nil {
		t.Fatalf("getsid: %v", err)
	}
	selfSid, err := unixGetsid(os.Getpid())
	if err != nil {
		t.Fatalf("getsid(self): %v", err)
	}
	if sid != selfSid {
		t.Fatalf("editor sid = %d, want the spawning process's sid %d (a new session would be a reap surface)", sid, selfSid)
	}
}

// TestResolveVSCodeBinary_PrefersConfigThenPath covers the detection order and
// the deliberate refusal to silently fall back from a configured path.
func TestResolveVSCodeBinary_PrefersConfigThenPath(t *testing.T) {
	dir := t.TempDir()
	openv := filepath.Join(dir, "openvscode-server")
	if err := os.WriteFile(openv, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// Nothing configured: detected from PATH.
	got, err := resolveVSCodeBinary("")
	if err != nil || got != openv {
		t.Fatalf("resolveVSCodeBinary(\"\") = %q, %v; want %q", got, err, openv)
	}
	// code-server wins when both are present.
	cs := filepath.Join(dir, "code-server")
	if err := os.WriteFile(cs, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveVSCodeBinary(""); err != nil || got != cs {
		t.Fatalf("resolveVSCodeBinary(\"\") = %q, %v; want code-server %q to win", got, err, cs)
	}
	// A configured path that does not exist is an ERROR naming the key, never a
	// silent fall back to a different editor than the one the user chose.
	_, err = resolveVSCodeBinary(filepath.Join(dir, "nope"))
	if err == nil || !strings.Contains(err.Error(), "vscode_server_binary") {
		t.Fatalf("resolveVSCodeBinary(missing) err = %v, want one naming vscode_server_binary", err)
	}
	// Nothing installed and nothing configured is the sentinel, not an error.
	t.Setenv("PATH", t.TempDir())
	if _, err := resolveVSCodeBinary(""); !errors.Is(err, errVSCodeBinaryMissing) {
		t.Fatalf("resolveVSCodeBinary with an empty PATH = %v, want errVSCodeBinaryMissing", err)
	}
}

// TestVSCodeArgs_FlavorDialects: the two editors disagree on every flag that
// matters, so the dialect must follow the binary name.
func TestVSCodeArgs_FlavorDialects(t *testing.T) {
	if got := flavorForBinary("/usr/bin/openvscode-server"); got != flavorOpenVSCode {
		t.Errorf("flavorForBinary(openvscode-server) = %v, want flavorOpenVSCode", got)
	}
	if got := flavorForBinary("/usr/bin/code-server"); got != flavorCodeServer {
		t.Errorf("flavorForBinary(code-server) = %v, want flavorCodeServer", got)
	}
	// An unknown name falls back to the code-server dialect.
	if got := flavorForBinary("/opt/my-editor-wrapper"); got != flavorCodeServer {
		t.Errorf("flavorForBinary(wrapper) = %v, want the code-server dialect", got)
	}

	cs := strings.Join(vscodeArgs(flavorCodeServer, "127.0.0.1", 1234, "/wt"), " ")
	if !strings.Contains(cs, "--bind-addr 127.0.0.1:1234") || !strings.Contains(cs, "--auth none") {
		t.Errorf("code-server argv = %q", cs)
	}
	// --abs-proxy-base-path governs code-server's OWN /absproxy feature and does
	// nothing for serving it under a sub-path (coder/code-server#6770). Passing it
	// would be cargo cult; assert we never do.
	if strings.Contains(cs, "abs-proxy-base-path") {
		t.Errorf("code-server argv passes --abs-proxy-base-path, which has no effect on sub-path serving: %q", cs)
	}
	ov := strings.Join(vscodeArgs(flavorOpenVSCode, "127.0.0.1", 1234, "/wt"), " ")
	if !strings.Contains(ov, "--host 127.0.0.1") || !strings.Contains(ov, "--port 1234") ||
		!strings.Contains(ov, "--without-connection-token") {
		t.Errorf("openvscode-server argv = %q", ov)
	}
	for _, argv := range []string{cs, ov} {
		if !strings.HasSuffix(argv, "/wt") {
			t.Errorf("argv %q does not end with the worktree", argv)
		}
	}
}

// --- helpers ---

func newTestVSCodeSupervisor(t *testing.T, binary string) *vscodeSupervisor {
	t.Helper()
	v := newVSCodeSupervisor()
	v.configuredBinary = func() string { return binary }
	v.startGrace = 5 * time.Second
	v.cooldown = 50 * time.Millisecond
	t.Cleanup(v.Stop)
	return v
}

func vscodeProxyPath(id string, idx int, sub string) string {
	return fmt.Sprintf("/v1/webtab/%s/%d/%s", id, idx, sub)
}

// getVSCodeProxy issues the proxy request, retrying while the editor reports that
// it is still starting (the notice page the browser would auto-refresh through).
func getVSCodeProxy(t *testing.T, mux *http.ServeMux, id string, idx int, sub string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, idx, sub), nil))
		body := rec.Body.String()
		if rec.Code == http.StatusOK {
			return body
		}
		if !strings.Contains(body, "still starting") || time.Now().After(deadline) {
			t.Fatalf("GET %s: status %d, body %s", vscodeProxyPath(id, idx, sub), rec.Code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForVSCodeReady polls url until the editor is up and proxying.
func waitForVSCodeReady(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body := make([]byte, 512)
			n, _ := resp.Body.Read(body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body[:n]), fakeVSCodeMarker) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the editor never became ready at %s", url)
}

// argvValue returns the value following flag in a newline-joined argv.
func argvValue(argv, flag string) string {
	parts := strings.Split(argv, "\n")
	for i, p := range parts {
		if p == flag && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// assertProcessGone waits for pid to disappear. Teardown SIGTERMs then SIGKILLs
// the group, so a well-behaved child exits promptly; this tolerates the schedule.
func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// Signal 0 probes existence. The child is reaped by the supervisor's Wait,
		// so a live pid here is a real leak, not a zombie.
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the editor (pid %d) is still running; it leaked", pid)
}

func unixGetsid(pid int) (int, error) {
	sid, _, errno := syscall.Syscall(syscall.SYS_GETSID, uintptr(pid), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}

// TestVSCodeTab_ForwardsProxyPrefix is the codex P2 fix, and it is what makes the
// ADVERTISED fallback editor work at all.
//
// The two editors resolve their base path differently: code-server emits RELATIVE
// URLs from the request path's depth (so stripping the prefix suffices, and this
// header is inert to it), while openvscode-server emits ABSOLUTE ones and reads
// its base from X-Forwarded-Prefix. Without the header, openvscode-server's assets
// and WebSocket address the daemon's ROOT instead of /v1/webtab/..., and nothing
// loads — a break that only shows up on machines where code-server is absent.
//
// Its --server-base-path flag cannot substitute: that bakes ONE prefix into the
// process, but a single per-SESSION editor is reached under a different prefix per
// tab index. Only a per-request header composes with a shared editor.
func TestVSCodeTab_ForwardsProxyPrefix(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, idx, _ := newVSCodeFixture(t, binary)

	mux := newHTTPMux(&controlServer{manager: manager})
	body := getVSCodeProxy(t, mux, id, idx, "")
	want := "xfp=/v1/webtab/" + id + "/" + strconv.Itoa(idx)
	if !strings.Contains(body, want) {
		t.Fatalf("proxied body %q is missing %q; openvscode-server would resolve its assets against the daemon root and never load", body, want)
	}
}

// TestWebTab_DoesNotForwardProxyPrefix: a web tab's target is an ARBITRARY dev
// server. A framework that honors X-Forwarded-Prefix would start rewriting its
// URLs — a behavior change to today's previews that belongs in its own change, not
// smuggled in with this feature.
func TestWebTab_DoesNotForwardProxyPrefix(t *testing.T) {
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Forwarded-Prefix")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	mux, id, idx := newWebTabProxyFixture(t, upstream.URL)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, idx, ""), nil))
	if got != "" {
		t.Fatalf("a web tab's dev server received X-Forwarded-Prefix=%q; only a vscode tab should", got)
	}
}

// TestVSCodeServer_StopKillsChildrenThatOutliveTheLeader is codex's P2 teardown
// finding. code-server is never one process: it spawns an extension host, a pty
// host, and a process per integrated terminal. Returning as soon as the LEADER
// exits would strand any of them that ignores SIGTERM or simply outlives it —
// precisely the leak this supervisor exists to prevent, and invisible to a test
// that only checks the leader.
func TestVSCodeServer_StopKillsChildrenThatOutliveTheLeader(t *testing.T) {
	// The fake editor forks a SIGTERM-ignoring child (see fakeVSCodeServerMain) and
	// reports its pid here, so teardown can be checked against the whole group.
	dir := t.TempDir()
	childPidFile := filepath.Join(dir, "child.pid")
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{
		"AF_TEST_ORPHAN_CHILD_PIDFILE": childPidFile,
	})
	v := newTestVSCodeSupervisor(t, binary)
	if _, err := v.ensureServer("k", t.TempDir()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	leader := v.servers["k"].cmd.Process.Pid

	// Wait for the fake to have spawned its SIGTERM-ignoring child.
	var childPid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(childPidFile); err == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && p > 0 {
				childPid = p
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if childPid == 0 {
		t.Fatal("the fake editor never reported its child pid")
	}

	v.stopFor("k")

	assertProcessGone(t, leader)
	// The real assertion: the child went with it.
	assertProcessGone(t, childPid)
}

// TestVSCodeSupervisor_CoolsDownAnEditorThatDiesBeforeReady is codex's P3. An
// editor that outlives the start grace and THEN dies without ever listening is a
// broken start, not a crash to heal from. Only spawnLocked's own errors used to be
// recorded, so this path respawned on every auto-refresh of the "starting" notice
// — a spawn loop driven by our own UI.
func TestVSCodeSupervisor_CoolsDownAnEditorThatDiesBeforeReady(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeHangEnv: "1"})
	v := newTestVSCodeSupervisor(t, binary)
	v.startGrace = 100 * time.Millisecond
	v.cooldown = time.Hour // any respawn inside the window is the bug
	worktree := t.TempDir()

	if _, err := v.ensureServer("k", worktree); !errors.Is(err, errVSCodeStarting) {
		t.Fatalf("ensureServer err = %v, want errVSCodeStarting", err)
	}
	server := v.servers["k"]
	if server == nil {
		t.Fatal("the still-starting editor was not registered")
	}
	// It dies without ever having listened.
	_ = syscall.Kill(-server.cmd.Process.Pid, syscall.SIGKILL)
	<-server.exited

	_, err := v.ensureServer("k", worktree)
	if errors.Is(err, errVSCodeStarting) || err == nil {
		t.Fatalf("ensureServer respawned a never-ready editor that died (err = %v); the notice's refresh would drive a spawn loop", err)
	}
	if !errors.Is(err, errVSCodeStartExited) {
		t.Fatalf("err = %v, want errVSCodeStartExited", err)
	}
	if _, registered := v.servers["k"]; registered {
		t.Fatal("a new editor was spawned despite the cooldown")
	}
}

// TestVSCodeSupervisor_ReadyThenCrashedStillSelfHeals is the other half of the
// rule above: a crash AFTER the editor served requests must still respawn at once.
// Without this the cooldown would turn self-heal into a stall.
func TestVSCodeSupervisor_ReadyThenCrashedStillSelfHeals(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)
	v.cooldown = time.Hour
	worktree := t.TempDir()

	first, err := v.ensureServer("k", worktree)
	if err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	dead := v.servers["k"]
	_ = syscall.Kill(-dead.cmd.Process.Pid, syscall.SIGKILL)
	<-dead.exited

	second, err := v.ensureServer("k", worktree)
	if err != nil {
		t.Fatalf("a READY-then-crashed editor must respawn immediately, not wait out the cooldown: %v", err)
	}
	if second == first {
		t.Fatalf("reused the dead editor's URL %s", second)
	}
}

// TestEnsureVSCodeServer_StopsAnEditorWhoseTabWasClosedMidSpawn is codex's P2
// race. CloseTab stops the editor under the op-lock this proxy route deliberately
// does NOT take (a spawn is far too slow to hold it), so a request can resolve a
// vscode tab, have that tab closed — and its stopFor already run — while the spawn
// is still in flight, and end up owning an editor that belongs to no tab: nothing
// renders it, and no close/archive/kill path for a tab that no longer exists will
// ever stop it. The re-check AFTER the spawn is what closes that window.
func TestEnsureVSCodeServer_StopsAnEditorWhoseTabWasClosedMidSpawn(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, _, _, _ := newVSCodeFixture(t, binary)
	const title = "vscodeproxy"

	repo := ""
	var inst *session.Instance
	manager.mu.Lock()
	for key, candidate := range manager.instances {
		r, tt := splitDaemonInstanceKey(key)
		if tt == title {
			repo, inst = r, candidate
		}
	}
	manager.mu.Unlock()
	if inst == nil {
		t.Fatal("fixture instance not found")
	}

	// Close the vscode tab: this is the state an in-flight spawn returns into.
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo, TabName: "vscode"}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}

	// The request that resolved BEFORE the close now finishes its spawn.
	_, err := manager.ensureVSCodeServer(inst, repo, title)
	if err == nil {
		t.Fatal("an editor was served for a session whose VS Code tab is gone")
	}
	if !strings.Contains(err.Error(), "was closed") {
		t.Fatalf("err = %v, want one naming the closed tab", err)
	}
	// ...and it did not leave the editor it just started running.
	manager.vscode.mu.Lock()
	_, registered := manager.vscode.servers[daemonInstanceKey(repo, title)]
	manager.vscode.mu.Unlock()
	if registered {
		t.Fatal("the editor started for a now-closed tab was left running; nothing can reach or reap it until daemon shutdown")
	}
}
