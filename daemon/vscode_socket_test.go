package daemon

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The #1873 regression suite: the editor is reachable ONLY over a 0600 unix
// socket in a 0700 directory, never over TCP, and the readiness probe can no
// longer adopt a listener the daemon did not start.

// TestVSCodeSocket_NoTCPListenerExists is the headline security regression. The
// editor runs with authentication disabled, so a TCP listener is a second,
// unguarded route to worktree write + terminal exec for any local process — the
// bypass of require_token/require_loopback_token that #1873 fixes. Prove the argv
// asks for no TCP listener at all, for BOTH flavors: a socket merely ADDED
// alongside a port would close nothing.
func TestVSCodeSocket_NoTCPListenerExists(t *testing.T) {
	for _, tc := range []struct {
		binary    string
		socketArg string
	}{
		{"code-server", "--socket"},
		{"openvscode-server", "--socket-path"},
	} {
		t.Run(tc.binary, func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "argv")
			binary := writeFakeVSCodeBinary(t, tc.binary, map[string]string{fakeVSCodeArgsEnv: argsFile})
			v := newTestVSCodeSupervisor(t, binary)

			ep, err := v.ensureServer("repo/session", t.TempDir())
			if err != nil {
				t.Fatalf("ensureServer: %v", err)
			}
			args := readArgv(t, argsFile)

			// The socket flag is present and names the endpoint we were handed.
			if got := argvValue(args, tc.socketArg); got != ep.SocketPath {
				t.Fatalf("editor argv %s = %q, want the endpoint %q", tc.socketArg, got, ep.SocketPath)
			}
			fi, err := os.Lstat(ep.SocketPath)
			if err != nil {
				t.Fatalf("the editor socket %s does not exist: %v", ep.SocketPath, err)
			}
			if fi.Mode()&os.ModeSocket == 0 {
				t.Fatalf("%s is not a socket (mode %v)", ep.SocketPath, fi.Mode())
			}

			// No TCP flag survives. Any of these would reopen the unguarded route.
			for _, tcpFlag := range []string{"--bind-addr", "--host", "--port"} {
				if got := argvValue(args, tcpFlag); got != "" {
					t.Errorf("editor argv still asks for a TCP listener via %s=%q; the "+
						"unauthenticated editor must have no port to scan (#1873)", tcpFlag, got)
				}
			}
		})
	}
}

// TestVSCodeSocket_ModeIsOwnerOnly asserts the perms that ARE the authentication.
// 0600 gives the editor the posture af's own control socket has, which is exactly
// what config_types.go holds up as the stronger primitive where it documents
// require_loopback_token.
//
// openvscode-server is the case that matters: it has no --socket-mode, so its
// socket lands under the daemon's umask and only the daemon's own chmod makes it
// 0600. The fake reproduces that faithfully, so this fails if that chmod is lost.
func TestVSCodeSocket_ModeIsOwnerOnly(t *testing.T) {
	for _, binaryName := range []string{"code-server", "openvscode-server"} {
		t.Run(binaryName, func(t *testing.T) {
			binary := writeFakeVSCodeBinary(t, binaryName, nil)
			v := newTestVSCodeSupervisor(t, binary)

			ep, err := v.ensureServer("repo/session", t.TempDir())
			if err != nil {
				t.Fatalf("ensureServer: %v", err)
			}

			fi, err := os.Lstat(ep.SocketPath)
			if err != nil {
				t.Fatalf("stat %s: %v", ep.SocketPath, err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Errorf("editor socket has mode %#o, want 0600: a group/world-reachable socket "+
					"leaves the unauthenticated editor open to other users (#1873)", perm)
			}

			// The directory is the other half, and the load-bearing half for a
			// flavor with no mode flag: it fences the socket during the window
			// between the child's bind() and any chmod.
			dir := filepath.Dir(ep.SocketPath)
			dirInfo, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat socket dir: %v", err)
			}
			if perm := dirInfo.Mode().Perm(); perm != 0o700 {
				t.Errorf("socket directory %s has mode %#o, want 0700", dir, perm)
			}
		})
	}
}

// TestVSCodeSocketDir_TightensLoosePermissions: the 0700 directory is access
// control, so it cannot be left to MkdirAll — which is a no-op on a directory
// that already exists. A dir left loose by an older build or a permissive umask
// must be tightened, not inherited.
func TestVSCodeSocketDir_TightensLoosePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	loose := filepath.Join(home, vscodeSocketDirName)
	if err := os.MkdirAll(loose, 0o777); err != nil {
		t.Fatalf("seeding a loose socket dir: %v", err)
	}
	if err := os.Chmod(loose, 0o777); err != nil { // defeat the umask
		t.Fatalf("chmod: %v", err)
	}

	dir, err := vscodeSocketDir()
	if err != nil {
		t.Fatalf("vscodeSocketDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket dir left at %#o, want 0700: a world-writable directory lets any "+
			"local user swap the socket for their own (#1873)", perm)
	}
}

// TestVSCodeSocketPath_RejectsOverlongPath: sockaddr_un caps a socket path at
// ~104 bytes, and AGENT_FACTORY_HOME can point anywhere. Over the limit,
// net.Listen fails with a bare "invalid argument" naming nothing, so the length
// is checked up front where the message can name the cause and the fix.
func TestVSCodeSocketPath_RejectsOverlongPath(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 90), strings.Repeat("e", 90))
	t.Setenv("AGENT_FACTORY_HOME", deep)

	if _, err := vscodeSocketPath("repo/session"); err == nil {
		t.Fatal("vscodeSocketPath accepted an over-long path; net.Listen would fail opaquely")
	} else if !strings.Contains(err.Error(), "AGENT_FACTORY_HOME") {
		t.Errorf("error %q does not name the knob the operator can change", err)
	}
}

// TestVSCodeSocketPath_IsUniquePerCall pins the invariant that makes "unlink my
// own socket" safe: every call yields a DISTINCT path, so a dying editor and the
// one replacing it can never own the same file. The session's hash still leads
// the name, which is what lets an operator tell whose socket is whose without
// making the path collide.
func TestVSCodeSocketPath_IsUniquePerCall(t *testing.T) {
	shortAFHome(t)

	first, err := vscodeSocketPath("repo/alpha")
	if err != nil {
		t.Fatalf("vscodeSocketPath: %v", err)
	}
	again, err := vscodeSocketPath("repo/alpha")
	if err != nil {
		t.Fatalf("vscodeSocketPath: %v", err)
	}
	// Same key, different path: a respawn must not reuse the outgoing editor's
	// socket, or its teardown would unlink the live one (#1873 regression).
	if first == again {
		t.Errorf("two spawns of one session share the socket path %q; the outgoing "+
			"editor's teardown would unlink the incoming editor's endpoint", first)
	}
	// Same key, same leading hash: the name still identifies the session.
	sessionOf := func(path string) string {
		name := strings.TrimSuffix(filepath.Base(path), vscodeSocketExt)
		prefix, _, _ := strings.Cut(name, "-")
		return prefix
	}
	if sessionOf(first) != sessionOf(again) {
		t.Errorf("one session's sockets carry different hashes: %q vs %q", first, again)
	}
	other, err := vscodeSocketPath("repo/beta")
	if err != nil {
		t.Fatalf("vscodeSocketPath: %v", err)
	}
	if sessionOf(other) == sessionOf(first) {
		t.Errorf("two sessions hash alike: %q and %q", other, first)
	}
	// A title with a path separator or spaces must not escape the directory.
	odd, err := vscodeSocketPath("repo/../../etc/evil name")
	if err != nil {
		t.Fatalf("vscodeSocketPath: %v", err)
	}
	if filepath.Dir(odd) != filepath.Dir(first) {
		t.Errorf("a key with separators escaped the socket directory: %q", odd)
	}
}

// TestVSCodeSocket_StartOneEvictsSquatter is the second #1873 finding, tested at
// the level where the guarantee lives.
//
// The old flow bound :0, closed the listener, and handed the number to the child
// — so a process that grabbed the port in that window was ADOPTED: the probe's
// dial succeeded against the stranger, ready latched, and the proxy relayed a
// foreign service into the VS Code iframe under af's auth. Note the fix is NOT a
// smarter probe. waitForSocket dials a path and cannot tell who is behind it, any
// more than waitForPort could. What changed is that there is no window to lose:
// the daemon names the path itself — unguessably (a nonce) and inside a directory
// only it can write — and unlinks it before the child binds.
//
// This exercises that last step directly, by handing startOne a path a foreign
// server already holds and proving the editor still ends up being ours.
func TestVSCodeSocket_StartOneEvictsSquatter(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	dir, err := vscodeSocketDir()
	if err != nil {
		t.Fatalf("vscodeSocketDir: %v", err)
	}
	path := filepath.Join(dir, "squatted"+vscodeSocketExt)

	const foreignMarker = "FOREIGN_SERVICE_NOT_THE_EDITOR"
	foreign, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("foreign listen: %v", err)
	}
	defer foreign.Close()
	foreignSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(foreignMarker))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = foreignSrv.Serve(foreign) }()
	t.Cleanup(func() { _ = foreignSrv.Close() })

	server, err := v.startOne(binary, flavorCodeServer, path, t.TempDir())
	if err != nil {
		t.Fatalf("startOne over a squatted socket: %v", err)
	}
	t.Cleanup(server.stop)

	client := &http.Client{Transport: server.transport, Timeout: 5 * time.Second}
	resp, err := client.Get(vscodeUpstreamURL + "/")
	if err != nil {
		t.Fatalf("dialing the editor endpoint: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if strings.Contains(string(body), foreignMarker) {
		t.Fatal("the supervisor adopted a foreign listener that held the socket path; " +
			"the proxy would relay a stranger's service into the editor iframe under af's auth (#1873)")
	}
	if !strings.Contains(string(body), fakeVSCodeMarker) {
		t.Fatalf("the endpoint is not served by our editor: %q", body)
	}
}

// TestWaitForSocket_ReportsChildExit pins the failure shape the readiness wait is
// responsible for: a child that dies during startup (a bad binary, an unreadable
// worktree) must be reported as dead immediately, not mistaken for a slow start
// and waited out until the grace expires.
func TestWaitForSocket_ReportsChildExit(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "editor.sock") // never created

	exited := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(exited)
	}()

	err := waitForSocket(socketPath, exited, 10*time.Second)
	if err == nil {
		t.Fatal("waitForSocket reported ready for an editor that never listened")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("waitForSocket error = %q, want the child's exit reported", err)
	}
}

// TestVSCodeSocket_StopUnlinksSocket: nothing else prunes the socket directory,
// so an editor that stops without unlinking leaves a file per session for the
// daemon's life. A SIGKILLed child never cleans up after itself, which is why
// the supervisor does it.
func TestVSCodeSocket_StopUnlinksSocket(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	ep, err := v.ensureServer("repo/session", t.TempDir())
	if err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	if _, err := os.Lstat(ep.SocketPath); err != nil {
		t.Fatalf("the editor socket is missing before teardown: %v", err)
	}

	v.stopFor("repo/session")

	if _, err := os.Lstat(ep.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("socket %s survived teardown (err=%v); the directory would accumulate "+
			"one dead socket per session", ep.SocketPath, err)
	}
}

// TestVSCodeSocket_RespawnGetsFreshTransport: the transport is tied to the
// PROCESS, so an editor that died and was replaced must not hand the proxy a
// transport still pooling connections to the dead socket.
func TestVSCodeSocket_RespawnGetsFreshTransport(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	const key = "repo/session"
	worktree := t.TempDir()
	first, err := v.ensureServer(key, worktree)
	if err != nil {
		t.Fatalf("ensureServer: %v", err)
	}

	// Kill the editor the way a crash would, then let the self-heal respawn it.
	v.mu.Lock()
	server := v.servers[key]
	v.mu.Unlock()
	server.stop()

	second, err := v.ensureServer(key, worktree)
	if err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if second.Transport == first.Transport {
		t.Error("the respawned editor reuses the dead process's transport, whose pool " +
			"holds connections to a socket that no longer exists")
	}
}

// readArgv returns the argv the fake editor recorded, newline-joined.
func readArgv(t *testing.T, argsFile string) string {
	t.Helper()
	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading the editor argv: %v", err)
	}
	return string(argv)
}

// TestVSCodeSocket_RespawnSurvivesOutgoingTeardown guards a regression the socket
// transport introduced and nearly shipped.
//
// Teardown unlinks the editor's socket, and it runs CONCURRENTLY with the respawn
// that replaces it: ensureServer drops a dead-or-stale editor with `go s.stop()`
// and spawns the replacement under the same key straight away. While the socket
// name was derived from the session key alone, both processes owned the same
// path — so the outgoing editor's unlink deleted the INCOMING editor's socket,
// which then listened on an unnamed socket that no dial could ever reach. The
// pane broke permanently, and only for a session that had been restored to a new
// worktree or had its editor die: exactly the self-heal path the supervisor
// exists for.
//
// The fake ignores SIGTERM here, so the old editor's stop() must wait out the
// full grace and its unlink is guaranteed to land after the new one has bound —
// making the window deterministic rather than a race the test would usually win.
func TestVSCodeSocket_RespawnSurvivesOutgoingTeardown(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{
		fakeVSCodeIgnoreTermEnv: "1",
	})
	v := newTestVSCodeSupervisor(t, binary)

	const key = "repo/session"
	first, err := v.ensureServer(key, t.TempDir())
	if err != nil {
		t.Fatalf("first ensureServer: %v", err)
	}

	// The session is restored onto a DIFFERENT worktree: the old editor is torn
	// down out of band and a new one spawns under the same key.
	second, err := v.ensureServer(key, t.TempDir())
	if err != nil {
		t.Fatalf("second ensureServer: %v", err)
	}
	if second.SocketPath == first.SocketPath {
		t.Fatalf("both editors share the socket path %s; the outgoing one's teardown "+
			"will unlink the incoming one's endpoint", second.SocketPath)
	}

	// Outlast the old editor's stop grace, then prove the live editor is still
	// serving on a socket that still exists.
	deadline := time.Now().Add(vscodeStopGrace + 3*time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(second.SocketPath); os.IsNotExist(err) {
			t.Fatalf("the outgoing editor's teardown unlinked the LIVE editor's socket %s: "+
				"every dial now gets ENOENT and the pane is permanently broken", second.SocketPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
	client := &http.Client{Transport: second.Transport, Timeout: 5 * time.Second}
	resp, err := client.Get(vscodeUpstreamURL + "/")
	if err != nil {
		t.Fatalf("the live editor is unreachable after the old one's teardown: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the live editor answered %d", resp.StatusCode)
	}
}

// TestVSCodeSocket_SweepsAbandonedSockets: socket names carry a nonce, so a
// SIGKILLed daemon's sockets are never reused and nothing else would ever remove
// them — one leaked file per session, for the life of the af home. The first
// spawn of a daemon's life clears them, which is safe precisely because that
// daemon owns no editors yet.
func TestVSCodeSocket_SweepsAbandonedSockets(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)

	dir, err := vscodeSocketDir()
	if err != nil {
		t.Fatalf("vscodeSocketDir: %v", err)
	}
	// A previous daemon's leftovers: a dead socket and a plain file.
	abandoned := filepath.Join(dir, "deadbeef-cafebabe"+vscodeSocketExt)
	ln, err := net.Listen("unix", abandoned)
	if err != nil {
		t.Fatalf("planting an abandoned socket: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()

	// A non-socket file must be left ALONE: the sweep removes what the daemon
	// creates, not whatever happens to share the directory.
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("seeding a foreign file: %v", err)
	}

	live, err := v.ensureServer("repo/session", t.TempDir())
	if err != nil {
		t.Fatalf("ensureServer: %v", err)
	}

	if _, err := os.Lstat(abandoned); !os.IsNotExist(err) {
		t.Errorf("the abandoned socket survived the sweep (err=%v)", err)
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Errorf("the sweep removed a file the daemon did not create: %v", err)
	}
	// The sweep must not have taken the editor it was spawning with it.
	if _, err := os.Lstat(live.SocketPath); err != nil {
		t.Errorf("the sweep removed the live editor's own socket: %v", err)
	}
}
