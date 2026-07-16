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
	"os/signal"
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
// unix socket it was handed. That exercises the actual spawn → probe → proxy
// path, including a genuine WS upgrade, with no external dependency.
//
// It mimics BOTH dialects' socket flags (code-server --socket/--socket-mode,
// openvscode-server --socket-path) and — like the real editors — creates the
// socket under its own umask, applying --socket-mode only AFTER listening. That
// is what lets the tests prove the daemon's own 0700 directory and post-listen
// chmod hold the security property rather than the editor's cooperation (#1873).

const (
	// fakeVSCodeEnv routes a re-exec of the test binary to fakeVSCodeServerMain.
	fakeVSCodeEnv = "AF_TEST_FAKE_CODE_SERVER"
	// fakeVSCodeArgsEnv names a file the fake writes its argv to, so a test can
	// assert what the daemon actually asked the editor to do.
	fakeVSCodeArgsEnv = "AF_TEST_FAKE_CODE_SERVER_ARGS"
	// fakeVSCodeHangEnv makes the fake start but never listen, standing in for a
	// cold editor that outruns the start grace.
	fakeVSCodeHangEnv = "AF_TEST_FAKE_CODE_SERVER_HANG"
	// fakeVSCodeIgnoreTermEnv makes the fake ignore SIGTERM, so a stop() against
	// it takes the full stop grace before escalating to SIGKILL. That turns the
	// teardown/respawn overlap into a deterministic window instead of a race a
	// test would only lose sometimes.
	fakeVSCodeIgnoreTermEnv = "AF_TEST_FAKE_CODE_SERVER_IGNORE_TERM"
	// fakeVSCodeFlavorEnv carries the name the fake was installed under, so it can
	// behave like THAT editor. The fake is re-exec'd as the test binary, so it
	// cannot read its own name from argv the way flavorForBinary reads the path.
	fakeVSCodeFlavorEnv = "AF_TEST_FAKE_CODE_SERVER_FLAVOR"
	// fakeVSCodeMarker is served at the root so a test can prove the response
	// came from the editor and through the proxy.
	fakeVSCodeMarker = "AF_FAKE_CODE_SERVER_OK"
)

// fakeVSCodeServerMain is the fake editor's entry point, running in a re-exec of
// the test binary. It mimics the parts of code-server this feature depends on:
// it binds the socket it is given, serves the worktree it is given, and upgrades
// WebSockets on any path (as code-server does).
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
	if os.Getenv(fakeVSCodeIgnoreTermEnv) != "" {
		// Outlive SIGTERM so the supervisor's stop must wait out its grace, the
		// way a wedged editor would.
		signal.Ignore(syscall.SIGTERM)
	}

	// Parse both dialects. valueFlags is what makes the positional worktree
	// findable: only these consume the next argv entry, so a bare word after a
	// BOOLEAN flag (--disable-telemetry) is correctly read as the worktree.
	//
	// The two dialects name the worktree differently, and the fake mimics that
	// rather than papering over it: code-server takes it POSITIONALLY, while
	// openvscode-server reads --default-folder and IGNORES a positional entirely
	// (webClientServer resolves the folder only from that flag). A fake that
	// accepted a positional for both would have kept passing while the real
	// openvscode came up on an empty workbench.
	valueFlags := map[string]bool{
		"--socket": true, "--socket-mode": true, "--auth": true, "--config": true,
		"--socket-path": true, "--default-folder": true,
	}
	socket, socketMode, worktree := "", "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if valueFlags[a] && i+1 < len(args) {
			switch a {
			case "--socket", "--socket-path":
				socket = args[i+1]
			case "--socket-mode":
				socketMode = args[i+1]
			case "--default-folder":
				// The openvscode dialect: the folder arrives as a flag value. The
				// real one reads ONLY this, never a positional path.
				worktree = args[i+1]
			}
			i++
			continue
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		// A positional worktree — the code-server dialect, and ONLY that dialect.
		//
		// openvscode-server must ignore it, exactly as the real one does: its
		// parser accepts a positional path and never reads it, resolving the
		// workbench folder from --default-folder alone. Honoring it here would make
		// the fake more forgiving than the editor it stands in for, and a test that
		// asserts the worktree is served would then PASS against an argv that opens
		// a real openvscode on nothing.
		if !isFakeOpenVSCode() {
			worktree = a
		}
	}
	if socket == "" {
		fmt.Fprintln(os.Stderr, "fake code-server: no --socket / --socket-path")
		os.Exit(2)
	}

	// Optionally fork a child that IGNORES SIGTERM and outlives us, in our process
	// group — the shape of code-server's extension/pty hosts, and what proves the
	// teardown escalates to the whole group rather than stopping at the leader.
	if pidFile := os.Getenv("AF_TEST_ORPHAN_CHILD_PIDFILE"); pidFile != "" {
		// The loop is BOUNDED (~60s), not `while :`. This child ignores SIGTERM by
		// design, so an unbounded one outlives any run that dies before stopFor can
		// SIGKILL the group — a panic, a ^C, `go test -timeout`, or the childPid==0
		// t.Fatal above — and then loops forever on the machine, immune to every
		// polite kill. (Found live on this box: a stranded child from a run 7.4h
		// earlier, its TempDir long gone.) The test needs it for ~0.15s; the cap is
		// pure margin, and it makes the worst case self-healing.
		script := "trap '' TERM; echo $$ > " + pidFile +
			"; i=0; while [ $i -lt 300 ]; do sleep 0.2; i=$((i+1)); done"
		child := exec.Command("sh", "-c", script)
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
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	// Bind the socket under the default umask, exactly as the real editors do —
	// the daemon must not be able to rely on the child for the mode.
	ln, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake code-server: listen %s: %v\n", socket, err)
		os.Exit(2)
	}
	// code-server chmods only AFTER it is listening, and openvscode-server has no
	// --socket-mode at all. Reproduce both faithfully: honor the mode if we were
	// given one, and otherwise leave whatever umask produced.
	if socketMode != "" {
		mode, perr := strconv.ParseUint(socketMode, 8, 32)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "fake code-server: bad --socket-mode %q: %v\n", socketMode, perr)
			os.Exit(2)
		}
		if cerr := os.Chmod(socket, os.FileMode(mode)); cerr != nil {
			fmt.Fprintf(os.Stderr, "fake code-server: chmod %s: %v\n", socket, cerr)
			os.Exit(2)
		}
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
	// Tell the fake which editor it is standing in for: the dialects differ in
	// ways that matter (openvscode ignores a positional worktree), and a fake that
	// blurs them would let a broken argv pass.
	exports.WriteString(fakeVSCodeFlavorEnv + "=" + shellQuote(name) + " ")
	for k, v := range env {
		exports.WriteString(k + "=" + shellQuote(v) + " ")
	}
	script := "#!/bin/sh\nexec env " + exports.String() + shellQuote(self) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the fake editor: %v", err)
	}
	return path
}

// isFakeOpenVSCode reports whether the fake is standing in for
// openvscode-server, mirroring flavorForBinary's name test on the daemon side.
func isFakeOpenVSCode() bool {
	return strings.Contains(strings.ToLower(os.Getenv(fakeVSCodeFlavorEnv)), "openvscode")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shortAFHome points AGENT_FACTORY_HOME at a SHORT temporary directory and
// returns it.
//
// t.TempDir() is not usable for an af home in these tests: it embeds the test's
// name, which here runs to 40-80 characters, and the editor's socket path is
// built under the af home. That is enough on its own to blow the ~104-byte
// sockaddr_un limit and fail a test for a reason unrelated to what it asserts.
// A real af home (~/.agent-factory/vscode/<hash>.sock is ~50 bytes) is nowhere
// near the limit, so this keeps the harness from inventing a length problem —
// while TestVSCodeSocketPath_RejectsOverlongPath still covers the limit itself.
func shortAFHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "af")
	if err != nil {
		t.Fatalf("creating a short af home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_FACTORY_HOME", dir)
	return dir
}

// newVSCodeFixture builds a manager with one started local instance holding a
// vscode tab, wired to the fake editor, and returns the manager, the instance's
// stable id, the tab index, and the worktree the editor should be serving.
func newVSCodeFixture(t *testing.T, binary string) (m *Manager, sessionID, tabID, worktree string) {
	t.Helper()
	shortAFHome(t)
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
	// The proxy is id-keyed (#1810), so hand back the tab's STABLE id, not its
	// ordinal — the route has no ordinal form to address it by.
	tabs := inst.GetTabs()
	vsID := tabs[len(tabs)-1].ID
	if vsID == "" {
		t.Fatal("the vscode tab has no stable id; the id-keyed proxy cannot address it")
	}
	return manager, inst.ID, vsID, wt
}

// TestVSCodeTab_SpawnsEditorAndProxiesIt is the headline test: creating a vscode
// tab and requesting its proxy path spawns the editor on a LOOPBACK port with the
// session's worktree, and the daemon proxies its response back.
func TestVSCodeTab_SpawnsEditorAndProxiesIt(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "argv")
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeArgsEnv: argsFile})
	manager, id, tabID, worktree := newVSCodeFixture(t, binary)

	mux := newHTTPMux(&controlServer{manager: manager})
	body := getVSCodeProxy(t, mux, id, tabID, "")
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
	// The security property: the editor listens on a unix socket and NOTHING else.
	// A loopback port used to stand here, which is exactly what #1873 removed — an
	// --auth none editor on a port is a second, unguarded route for any local
	// process. See vscode_socket_test.go for the perms and no-TCP regressions.
	socketPath := argvValue(args, "--socket")
	if socketPath == "" {
		t.Fatalf("editor argv %q is missing --socket", args)
	}
	fi, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("the editor socket %s does not exist: %v", socketPath, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("editor endpoint %s is not a socket (mode %v)", socketPath, fi.Mode())
	}
}

// TestVSCodeTab_RelaysWebSocketUpgrade proves the proxy relays a real WS upgrade.
// code-server is unusable without it — the editor's entire client/server channel
// is a WebSocket — and a recorder-based test cannot prove it (httptest.Recorder
// implements no Hijacker), so this drives a real listener end to end.
func TestVSCodeTab_RelaysWebSocketUpgrade(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, tabID, _ := newVSCodeFixture(t, binary)

	srv := httptest.NewServer(newHTTPMux(&controlServer{manager: manager}))
	defer srv.Close()

	// Warm the editor: the first request is what spawns it.
	waitForVSCodeReady(t, srv.Client(), srv.URL+vscodeProxyPath(id, tabID, ""))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + vscodeProxyPath(id, tabID, "ws")
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
	manager, id, tabID, _ := newVSCodeFixture(t, binary)

	mux := newHTTPMux(&controlServer{manager: manager})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, ""), nil)
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
	manager, id, tabID, _ := newVSCodeFixture(t, "")
	// An empty PATH plus no configured binary is "nothing installed". Emptied
	// AFTER the fixture, which shells out to git to build the repo.
	t.Setenv("PATH", t.TempDir())

	mux := newHTTPMux(&controlServer{manager: manager})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, ""), nil)
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
		t.Fatalf("ensureServer started a second editor (%+v != %+v); it must reuse one per session", first, second)
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
	// The endpoint is gone: nothing answers on the socket, and the file itself is
	// unlinked rather than left as litter in the socket directory.
	if conn, err := net.DialTimeout("unix", server.socketPath, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatalf("something is still listening on the stopped editor's socket %s", server.socketPath)
	}
	if _, err := os.Lstat(server.socketPath); !os.IsNotExist(err) {
		t.Fatalf("the stopped editor's socket %s was not unlinked (err=%v)", server.socketPath, err)
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

	if _, err := v.ensureServer("k", worktree); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	dead := v.servers["k"]
	deadPid := dead.cmd.Process.Pid
	// Kill it the way a crash would, out from under the supervisor.
	_ = syscall.Kill(-deadPid, syscall.SIGKILL)
	<-dead.exited
	// The cooldown gates FAILED spawns, not deaths; make sure it can't mask this.
	v.cooldown = 0

	if _, err := v.ensureServer("k", worktree); err != nil {
		t.Fatalf("ensureServer after the editor died: %v", err)
	}
	// Identity is the PROCESS, not the endpoint: a session's socket path is derived
	// from its key, so a respawn deliberately reuses the path. Comparing endpoints
	// would therefore prove nothing about whether anything was actually restarted.
	revived := v.servers["k"]
	if revived == nil || !revived.alive() {
		t.Fatal("the respawned editor is not alive")
	}
	if revived.cmd.Process.Pid == deadPid {
		t.Fatalf("supervisor handed back the dead editor (pid %d) instead of respawning", deadPid)
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

	csArgv := vscodeArgs(flavorCodeServer, "/run/af/e.sock", "/wt")
	cs := strings.Join(csArgv, " ")
	if !strings.Contains(cs, "--socket /run/af/e.sock") || !strings.Contains(cs, "--auth none") {
		t.Errorf("code-server argv = %q", cs)
	}
	// code-server has a mode flag; use it. The daemon chmods as well (the flag is
	// applied only after listen), but dropping it here would leave the socket at
	// the umask for the whole startup window.
	if !strings.Contains(cs, "--socket-mode 0600") {
		t.Errorf("code-server argv is missing --socket-mode 0600: %q", cs)
	}
	// No TCP listener may survive on either flavor: an --auth none editor on a port
	// is a second, unguarded route for any local process (#1873).
	for _, tcpFlag := range []string{"--bind-addr", "--host", "--port"} {
		if strings.Contains(cs, tcpFlag) {
			t.Errorf("code-server argv still asks for a TCP listener via %s: %q", tcpFlag, cs)
		}
	}
	// --abs-proxy-base-path governs code-server's OWN /absproxy feature and does
	// nothing for serving it under a sub-path (coder/code-server#6770). Passing it
	// would be cargo cult; assert we never do.
	if strings.Contains(cs, "abs-proxy-base-path") {
		t.Errorf("code-server argv passes --abs-proxy-base-path, which has no effect on sub-path serving: %q", cs)
	}
	// code-server's dialect: the worktree is POSITIONAL.
	if !strings.HasSuffix(cs, "/wt") {
		t.Errorf("code-server argv %q does not end with the worktree", cs)
	}

	ovArgv := vscodeArgs(flavorOpenVSCode, "/run/af/e.sock", "/wt")
	ov := strings.Join(ovArgv, " ")
	if !strings.Contains(ov, "--socket-path /run/af/e.sock") || !strings.Contains(ov, "--without-connection-token") {
		t.Errorf("openvscode-server argv = %q", ov)
	}
	for _, tcpFlag := range []string{"--bind-addr", "--host", "--port"} {
		if strings.Contains(ov, tcpFlag) {
			t.Errorf("openvscode-server argv still asks for a TCP listener via %s: %q", tcpFlag, ov)
		}
	}
	// openvscode-server has NO --socket-mode; passing one would abort its startup on
	// an unknown flag. Its socket is secured by the daemon's own chmod and the 0700
	// directory instead (#1873).
	if strings.Contains(ov, "--socket-mode") {
		t.Errorf("openvscode-server argv passes --socket-mode, which it does not accept: %q", ov)
	}
	// openvscode-server's dialect, and the whole point of this half: it resolves
	// the workbench folder ONLY from --default-folder. Its parser accepts '_', so a
	// positional worktree is taken and then never read — the editor comes up on an
	// empty workbench and NOTHING reports an error. Lock the named flag...
	if got := argvValueSpaced(ovArgv, "--default-folder"); got != "/wt" {
		t.Errorf("openvscode-server argv %q does not pass --default-folder /wt; the workbench would open empty", ov)
	}
	// ...and lock the absence of the ignored positional, which is what regressing
	// back to it would look like.
	ovValueFlags := map[string]bool{"--socket-path": true, "--default-folder": true}
	if pos := bareArgs(ovArgv, ovValueFlags); len(pos) != 0 {
		t.Errorf("openvscode-server argv %q passes positional args %v; openvscode never reads them", ov, pos)
	}
	// --server-base-path bakes ONE prefix into a process reached under a different
	// prefix per tab: it would fix tab 0 and break every other. X-Forwarded-Prefix
	// (per request, and honored in precedence over the flag) is the mechanism.
	if strings.Contains(ov, "server-base-path") {
		t.Errorf("openvscode-server argv passes --server-base-path, which would break every tab but the first: %q", ov)
	}
}

// TestVSCodeArgs_ScrubsIPCHookFromChildEnv is codex's [33]. code-server's
// shouldOpenInExistingInstance reads VSCODE_IPC_HOOK_CLI UNCONDITIONALLY, before
// it starts any server: when it is set the CLI forwards the folder to that
// existing editor and exits, and --bind-addr is never honored. A daemon launched
// from any VS Code / code-server integrated terminal inherits it, and then EVERY
// editor it spawns dies during startup while the worktree pops open in the user's
// own window — for the daemon's whole life, since environ is fixed at exec. af's
// own VS Code tab has an integrated terminal that sets it, so running `af` inside
// an af VS Code tab is enough to poison the daemon.
func TestVSCodeArgs_ScrubsIPCHookFromChildEnv(t *testing.T) {
	t.Setenv("VSCODE_IPC_HOOK_CLI", "/run/user/1000/vscode-ipc-deadbeef.sock")

	env := vscodeChildEnv()
	if hasEnvKey(env, "VSCODE_IPC_HOOK_CLI") {
		t.Error("the editor's env carries VSCODE_IPC_HOOK_CLI; code-server would forward the worktree to the inherited editor and exit instead of serving")
	}
	// A scrub, not a wipe.
	if !hasEnvKey(env, "PATH") {
		t.Error("the editor's env lost PATH")
	}
}

// TestVSCodeServer_ReaperKillsTheGroupBeforeClosingExited locks the ordering the
// teardown fix rests on. exited is what stop() returns on, and its callers act the
// instant it does — ArchiveSession MOVES the worktree. If exited closed before the
// group SIGKILL were issued, stop() could return while the editor's children were
// still alive and holding that directory open.
func TestVSCodeServer_ReaperKillsTheGroupBeforeClosingExited(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the throwaway leader: %v", err)
	}
	pgid := cmd.Process.Pid

	s := &vscodeServer{worktree: "/nowhere", cmd: cmd, exited: make(chan struct{})}
	var killed []syscall.Signal
	var killedWhileExitedOpen bool
	var killedPgid int
	s.killGroup = func(p int, sig syscall.Signal) error {
		killed = append(killed, sig)
		killedPgid = p
		select {
		case <-s.exited:
		default:
			killedWhileExitedOpen = true
		}
		return nil
	}

	s.reap()

	select {
	case <-s.exited:
	default:
		t.Fatal("reap() returned without closing exited; every waiter would hang")
	}
	if len(killed) != 1 || killed[0] != syscall.SIGKILL {
		t.Fatalf("reap() sent %v to the group, want exactly one SIGKILL — a child that outlived the leader would leak", killed)
	}
	if killedPgid != pgid {
		t.Fatalf("reap() signalled pgid %d, want the leader's own group %d", killedPgid, pgid)
	}
	if !killedWhileExitedOpen {
		t.Fatal("reap() closed exited BEFORE killing the group; stop() could return, and ArchiveSession move the worktree, while the editor's children still held it open")
	}
}

// TestVSCodeServer_StopDoesNotSignalAReapedLeadersGroup is codex's [17]/[23], and
// it is the negative half of the teardown rule: the group kill is safe ONLY while
// it is adjacent to the reap.
//
// POSIX XBD 3.297 pins a pid against reuse exactly while a process group with that
// id exists — i.e. while there is something to kill. Once the leader is reaped and
// the group is empty, that guarantee is spent and the kernel is free to hand the
// id to an unrelated new group leader. Nothing prunes a dead server from v.servers
// (no ticker, no sweep), so its entry can sit there for days until a tab close
// calls stop() — and a stop() that signalled -pgid then would SIGKILL whatever
// group had since been given that id. macOS makes it concrete: PID_MAX is 99999.
func TestVSCodeServer_StopDoesNotSignalAReapedLeadersGroup(t *testing.T) {
	// A real leader in its own group, started and then REAPED: exactly the state a
	// crashed editor's entry sits in until some later lifecycle event reaches it.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the throwaway leader: %v", err)
	}
	_ = cmd.Wait() // reaped — the kernel may hand this pid to anyone now

	s := &vscodeServer{worktree: "/nowhere", cmd: cmd, exited: make(chan struct{})}
	close(s.exited) // what reap() does once it has killed the group
	var signalled []syscall.Signal
	s.killGroup = func(_ int, sig syscall.Signal) error {
		signalled = append(signalled, sig)
		return nil
	}

	s.stop()

	if len(signalled) != 0 {
		t.Fatalf("stop() sent %v to a REAPED leader's pgid; that id can already belong to an unrelated process group, and this is a SIGKILL at it", signalled)
	}
}

// TestVSCodeServer_StopSignalsWhileTheLeaderIsUnreaped is the positive half: the
// rule above must not be over-applied into a stop() that never signals anything.
// A LIVE leader is unreaped, which is itself the proof that -pgid is still ours.
func TestVSCodeServer_StopSignalsWhileTheLeaderIsUnreaped(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	// The seam goes on the SUPERVISOR, before the spawn: startOne copies it into
	// the server it builds, so it is in place before the reaper goroutine that
	// reads it exists. Setting it on the server ensureServer hands back — the
	// obvious spelling — is a write racing that already-running reaper, which
	// signals the group through this same seam on its way out.
	//
	// Buffered for every signal this teardown can produce (stop()'s SIGTERM, its
	// SIGKILL escalation, and the reaper's own SIGKILL) so no send can wedge the
	// reaper on a channel the assertion below has stopped reading.
	sigs := make(chan syscall.Signal, 4)
	v.killGroup = func(pgid int, sig syscall.Signal) error {
		sigs <- sig
		return syscall.Kill(-pgid, sig) // still really tear it down
	}

	if _, err := v.ensureServer("k", t.TempDir()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	if s := v.servers["k"]; s == nil || !s.alive() {
		t.Fatal("the editor is not alive; the fixture proves nothing")
	}

	v.stopFor("k")

	// SIGTERM is first by construction, not by scheduling luck: the reaper cannot
	// signal until cmd.Wait() returns, and the leader only exits because of the
	// SIGTERM this channel recorded before it was even delivered.
	select {
	case got := <-sigs:
		if got != syscall.SIGTERM {
			t.Fatalf("stop() opened with %v, want SIGTERM before any escalation", got)
		}
	default:
		t.Fatal("stop() signalled a LIVE editor's group not at all; the editor would be left running")
	}
}

// TestVSCodeSupervisor_StartupExitIsTheSentinel is codex's [15] prerequisite. An
// editor that exits before it listens must surface errVSCodeStartExited through
// the WRAP, not as a bare error: the proxy matches the sentinel to render a styled
// notice, and a bare error reaches the pane as a raw JSON 404 instead.
func TestVSCodeSupervisor_StartupExitIsTheSentinel(t *testing.T) {
	// A binary that starts fine and exits at once, without ever listening — a
	// broken install, in one line.
	dir := t.TempDir()
	binary := filepath.Join(dir, "code-server")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("writing the exiting fake: %v", err)
	}
	v := newTestVSCodeSupervisor(t, binary)
	v.startGrace = 2 * time.Second
	v.cooldown = time.Hour // the replay below must be the recorded error, not a respawn
	worktree := t.TempDir()

	_, err := v.ensureServer("k", worktree)
	if err == nil {
		t.Fatal("an editor that exited before listening was reported as healthy")
	}
	if !errors.Is(err, errVSCodeStartExited) {
		t.Fatalf("ensureServer err = %v, want one wrapping errVSCodeStartExited; the proxy matches the sentinel to render a notice", err)
	}

	// The cooldown replays the RECORDED error, so the sentinel must survive that
	// path too — it is the one every auto-refresh of the notice takes.
	_, replayed := v.ensureServer("k", worktree)
	if !errors.Is(replayed, errVSCodeStartExited) {
		t.Fatalf("the cooldown replayed %v, want one wrapping errVSCodeStartExited", replayed)
	}
}

// --- helpers ---

func newTestVSCodeSupervisor(t *testing.T, binary string) *vscodeSupervisor {
	t.Helper()
	// The editor's socket lives under the af home, so every supervisor test needs
	// one of its own. Without it a test would bind sockets in the REAL
	// ~/.agent-factory/vscode — the live daemon's directory on a dev box.
	shortAFHome(t)
	v := newVSCodeSupervisor()
	v.configuredBinary = func() string { return binary }
	v.startGrace = 5 * time.Second
	v.cooldown = 50 * time.Millisecond
	t.Cleanup(v.Stop)
	return v
}

func vscodeProxyPath(sessionID, tabID, sub string) string {
	return fmt.Sprintf("/v1/webtab/%s/%s/%s", sessionID, tabID, sub)
}

// getVSCodeProxy issues the proxy request, retrying while the editor reports that
// it is still starting (the notice page the browser would auto-refresh through).
func getVSCodeProxy(t *testing.T, mux *http.ServeMux, id, tabID, sub string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, sub), nil))
		body := rec.Body.String()
		if rec.Code == http.StatusOK {
			return body
		}
		if !strings.Contains(body, "still starting") || time.Now().After(deadline) {
			t.Fatalf("GET %s: status %d, body %s", vscodeProxyPath(id, tabID, sub), rec.Code, body)
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

// argvValueSpaced returns the value following flag in an argv SLICE (as opposed to
// argvValue, which reads the newline-joined argv the fake writes out).
func argvValueSpaced(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// bareArgs returns argv's positional entries: those that neither start with "--"
// nor are consumed as the value of a flag in valueFlags.
func bareArgs(argv []string, valueFlags map[string]bool) []string {
	var bare []string
	for i := 0; i < len(argv); i++ {
		if valueFlags[argv[i]] {
			i++ // skip the value
			continue
		}
		if strings.HasPrefix(argv[i], "--") {
			continue
		}
		bare = append(bare, argv[i])
	}
	return bare
}

func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// assertProcessGone waits for pid to stop EXECUTING. Teardown SIGTERMs then
// SIGKILLs the group, so a well-behaved child exits promptly; this tolerates the
// schedule.
//
// A zombie counts as gone, and that is the honest reading of what the supervisor
// promises. It Waits its DIRECT child, so the leader's pid really is reaped and
// signal 0 turns ESRCH. But a process the editor itself spawned is a GRANDchild:
// nothing on this side can Wait it, so the group SIGKILL leaves it reparented to
// PID 1 and `<defunct>` until PID 1 reaps it. Signal 0 keeps succeeding for that
// corpse, so signal 0 ALONE makes this test assert something the supervisor never
// guaranteed — that somebody reaped the descendant — and it fails wherever PID 1
// does not reap (a bare `docker run`, whose PID 1 is the test binary itself, since
// run-tests.sh `exec`s go test; the sanctioned `make test-container` passes only
// because it runs --init/tini). The supervisor's guarantee is that the descendant
// stops running, and state Z satisfies it.
//
// Contrast TestStartDaemonChildReapsExitedChild (#816), where af IS the parent and
// must reap: a zombie there is the bug, and signal 0 is the right detector.
func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// Signal 0 probes existence: it succeeds for a live process AND for a
		// zombie, so it only rules out a leak together with the Z check below.
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if processIsZombie(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the editor (pid %d) is still running; it leaked", pid)
}

// processIsZombie reports whether pid is a corpse awaiting a reap (state Z) —
// dead, executing nothing, holding only its process-table slot.
//
// Linux-only, reading the state field of /proc/<pid>/stat. Where /proc is absent
// this reports false and leaves the signal-0 probe exactly as it was, which is
// right on those systems: their PID 1 reaps orphans, so the zombie window this
// exists for does not stay open.
func processIsZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false // raced with the reap, or no /proc; the signal-0 probe decides
	}
	// Format: `pid (comm) state ...`. comm can itself contain spaces and ')', so
	// anchor on the LAST ')' — same parse as internal/proctree.readStat.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || end+2 > len(data) {
		return false
	}
	fields := strings.Fields(string(data[end+2:]))
	return len(fields) > 0 && fields[0] == "Z"
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
	manager, id, tabID, _ := newVSCodeFixture(t, binary)

	mux := newHTTPMux(&controlServer{manager: manager})
	body := getVSCodeProxy(t, mux, id, tabID, "")
	want := "xfp=/v1/webtab/" + id + "/" + tabID
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

	// The web-tab fixture is id-keyed too (#1810), so this addresses its tab by id.
	mux, id, webTabID := newWebTabProxyFixture(t, upstream.URL)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, webTabID, ""), nil))
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

	if _, err := v.ensureServer("k", worktree); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	dead := v.servers["k"]
	deadPid := dead.cmd.Process.Pid
	_ = syscall.Kill(-deadPid, syscall.SIGKILL)
	<-dead.exited

	if _, err := v.ensureServer("k", worktree); err != nil {
		t.Fatalf("a READY-then-crashed editor must respawn immediately, not wait out the cooldown: %v", err)
	}
	// The socket path is derived from the key and so is reused by design; the
	// process is what must be new.
	revived := v.servers["k"]
	if revived == nil || revived.cmd.Process.Pid == deadPid {
		t.Fatalf("reused the dead editor (pid %d) instead of respawning", deadPid)
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
