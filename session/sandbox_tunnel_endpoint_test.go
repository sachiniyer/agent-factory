package session

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The probes for #7495: the local end of a sandbox tunnel is a unix socket
// inside a uid-verified, per-daemon directory, and af writes nothing
// credential-bearing to an endpoint that is not provably the tunnel's own
// ssh child. Each mechanism gets its own pin.

// verifyTunnelSocketRoot must create a private tunnels/ when it can — and
// REFUSE anything that is not a real 0700 directory owned by this uid. The
// checks run on the opened directory, so a swap between stat and use cannot
// slip a foreign path past them.
func TestTunnelSocketRootVerifiesTheOpenedDir(t *testing.T) {
	// Fresh home: af creates tunnels/ 0700 and accepts it.
	home := t.TempDir()
	root, err := verifyTunnelSocketRoot(home)
	require.NoError(t, err)
	st, err := os.Stat(root)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), st.Mode().Perm(), "af must create tunnels/ 0700")

	// A pre-existing dir whose mode was loosened is refused, and the error
	// carries the fix rather than repairing silently.
	home2 := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home2, tunnelSocketRootName), 0o755))
	_, err = verifyTunnelSocketRoot(home2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0700")
	assert.Contains(t, err.Error(), "chmod 0700", "the refusal must name the fix")

	// A symlink is refused at the open itself.
	home3 := t.TempDir()
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(home3, tunnelSocketRootName)))
	_, err = verifyTunnelSocketRoot(home3)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	// A regular file is refused.
	home4 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home4, tunnelSocketRootName), nil, 0o600))
	_, err = verifyTunnelSocketRoot(home4)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// The spawn must pass the stream-local options explicitly (#7495): the mask
// binds the socket file 0600 (defense in depth inside the private dir), and
// Unlink is what lets ssh displace whatever already holds the path — the
// mechanism that makes a squatter impossible to keep.
func TestSpawnTunnelPassesStreamLocalOptions(t *testing.T) {
	forceTunnelLifelineExe(t, "")
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	p.forward = "/s/s0-x.sock:127.0.0.1:40001"
	child := spawnTestTunnel(t, p)
	script := child.cmd.Args[2]
	assert.Contains(t, script, "-o StreamLocalBindMask=0177",
		"the socket must be bound 0600, explicitly, not by default")
	assert.Contains(t, script, "-o StreamLocalBindUnlink=yes",
		"ssh must unlink whatever already holds the path before binding")
	// Both land after the operator's sandbox.ssh, and after the -L spec's
	// pinned literal — the source grep of
	// TestEveryTransportStepRidesTheOnePinnedCommand owns that text, so af's
	// stream-local options ride behind it. ssh reads the whole command line
	// before it binds anything, so position after the spec changes nothing.
	assert.Less(t, strings.Index(script, tunnelTestSleep), strings.Index(script, "StreamLocalBindMask"),
		"the options are af's, appended after the operator's command")
	assert.Less(t, strings.Index(script, `-N -L`), strings.Index(script, "StreamLocalBindMask"),
		"and after the pinned forward-spec literal")
}

// StreamLocalBindUnlink can only ever unlink inside this daemon's own
// generation dir: the socket path is minted there, and nowhere else.
func TestTunnelSocketStaysInsideTheGenerationDir(t *testing.T) {
	dir := testguard.SocketTempDir(t)
	prev := tunnelGenerationDir
	tunnelGenerationDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { tunnelGenerationDir = prev })

	forceTunnelLifelineExe(t, "")
	prevProbe := waitForSandboxTunnelFn
	waitForSandboxTunnelFn = func(*sandboxProvisioner, time.Duration, time.Duration) error { return nil }
	t.Cleanup(func() { waitForSandboxTunnelFn = prevProbe })

	var seen []string
	for i := 0; i < 2; i++ {
		p := &sandboxProvisioner{sshCmd: tunnelTestSleep, spec: ProvisionSpec{Title: "s"}}
		sockPath, err := p.openTunnel("127.0.0.1:9")
		require.NoError(t, err)
		t.Cleanup(func() {
			p.reapMu.Lock()
			p.stopTunnel()
			p.reapMu.Unlock()
		})
		assert.True(t, strings.HasPrefix(sockPath, dir+string(os.PathSeparator)),
			"socket %q must live inside this daemon's generation dir %q — the only place Unlink may ever reach", sockPath, dir)
		// The -L spec targets that socket, on this generation's contract.
		assert.Equal(t, sockPath+":127.0.0.1:9", p.forward)
		seen = append(seen, sockPath)
	}
	assert.NotEqual(t, seen[0], seen[1], "each tunnel mints a unique name within the generation dir")
}

// The sweep never removes a socket a live af process serves: a generation
// dir whose .lock is HELD — by this daemon, or a predecessor mid-upgrade —
// is skipped, while dirs whose lock provably died with their owner (flock
// frees on process death, which pid reuse cannot fake) are removed.
func TestSweepStaleTunnelSocketDirsPreservesLiveGenerations(t *testing.T) {
	root := t.TempDir()

	// A live generation: its .lock held — here by THIS process, standing in
	// for a predecessor daemon still serving sessions during a hand-off.
	live := filepath.Join(root, "d111111-aaaaaa")
	require.NoError(t, os.Mkdir(live, 0o700))
	liveLock, err := os.OpenFile(filepath.Join(live, tunnelGenLockName), os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(liveLock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	t.Cleanup(func() { _ = liveLock.Close() })
	require.NoError(t, os.WriteFile(filepath.Join(live, "s1-x.sock"), nil, 0o600))

	// A dead generation: its .lock was released with the process that held
	// it (here: written and closed) — provably sweepable.
	dead := filepath.Join(root, "d222222-bbbbbb")
	require.NoError(t, os.Mkdir(dead, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dead, tunnelGenLockName), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dead, "s1-y.sock"), nil, 0o600))

	// A generation that died between mkdir and its lock: empty, sweepable.
	emptySetup := filepath.Join(root, "d333333-cccccc")
	require.NoError(t, os.Mkdir(emptySetup, 0o700))

	// Unprovable: no lock, but content — it is not af's to reap.
	busy := filepath.Join(root, "d444444-dddddd")
	require.NoError(t, os.Mkdir(busy, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(busy, "s1-z.sock"), nil, 0o600))

	// Not af's shape at all: never classified, never touched.
	foreign := filepath.Join(root, "not-a-gen-dir")
	require.NoError(t, os.Mkdir(foreign, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(foreign, "x"), nil, 0o600))
	// A loose file in the root: same.
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.sock"), nil, 0o600))

	swept, err := sweepStaleTunnelSocketDirs(root)
	require.NoError(t, err)
	assert.Equal(t, 2, swept, "the dead generation and the dead setup, nothing more")

	assert.DirExists(t, live, "a live-held generation dir is never touched — hand-off safe")
	assert.FileExists(t, filepath.Join(live, "s1-x.sock"))
	assert.NoDirExists(t, dead, "a generation whose lock freed is swept")
	assert.NoDirExists(t, emptySetup, "an empty lockless dir is a provably-dead setup")
	assert.DirExists(t, busy, "content without a lock is unproven — it stays")
	assert.DirExists(t, foreign)
	assert.FileExists(t, filepath.Join(root, "stray.sock"))
}

// The collision proof: socket paths minted concurrently never repeat — which
// is what makes parallel sandbox provisions (and parallel ssh tests) unable
// to collide, where a TCP port reservation could be lost between close and
// ssh's bind.
func TestTunnelSocketPathsNeverCollide(t *testing.T) {
	dir := testguard.SocketTempDir(t)
	prev := tunnelGenerationDir
	tunnelGenerationDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { tunnelGenerationDir = prev })

	const n = 32
	paths := make([]string, n)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &sandboxProvisioner{spec: ProvisionSpec{Title: "s"}}
			path, err := p.tunnelSocketPath()
			assert.NoError(t, err)
			paths[i] = path
		}(i)
	}
	wg.Wait()
	seen := make(map[string]bool, n)
	for _, path := range paths {
		require.True(t, strings.HasPrefix(path, dir+string(os.PathSeparator)))
		require.False(t, seen[path], "two tunnels minted the same socket path %q", path)
		seen[path] = true
	}
}

// foreignTunnelListener stands up a unix listener at the endpoint af will
// dial and records every byte it receives — the stand-in for any process
// that is not af's own ssh child holding the path.
func foreignTunnelListener(t *testing.T, sockPath string) <-chan []byte {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	got := make(chan []byte, 1)
	go func() {
		defer func() { _ = ln.Close() }()
		conn, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf, _ := io.ReadAll(conn)
		got <- buf
	}()
	return got
}

// A foreign listener holding the tunnel's socket path is REFUSED before af
// writes a byte: the kernel names the acceptor's pid, it is not in the
// tunnel's process group, and nothing credential-bearing crosses (#7495).
func TestTunnelEndpointSendsNoCredentialToAForeignListener(t *testing.T) {
	sockPath := testguard.SocketPath(t, "foreign.sock")
	got := foreignTunnelListener(t, sockPath)

	// The tunnel this endpoint was minted for: a child in a DIFFERENT process
	// group than the listener's owner (this test process).
	fakeChild := exec.Command("sleep", "60")
	fakeChild.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, fakeChild.Start())
	t.Cleanup(func() {
		_ = fakeChild.Process.Kill()
		_ = fakeChild.Wait()
	})
	p := &sandboxProvisioner{socketPath: sockPath}
	p.tunnel = &tunnelChild{cmd: fakeChild, done: make(chan struct{})}

	ep := p.tunnelEndpoint("the-session-bearer")
	rc, err := newRemoteAgentClient(*ep, "t")
	require.NoError(t, err)
	err = rc.call("/v1/agent/provision", agentLifecycleReq{FirstTimeSetup: true}, nil)
	require.Error(t, err, "the connection must be refused before the credential is written")
	assert.Contains(t, err.Error(), "process group")

	select {
	case buf := <-got:
		assert.Empty(t, buf,
			"the foreign listener received %d bytes (%q) — af may write nothing to an endpoint that is not provably its own ssh child",
			len(buf), buf)
	case <-time.After(5 * time.Second):
		t.Fatal("the foreign listener never saw a connection at all — the dial itself should still happen")
	}
}

// And the same endpoint DOES reach a listener owned by the tunnel's own
// process group — the proof admits af's own forward rather than refusing
// everything, and the credential crosses exactly there.
func TestTunnelEndpointReachesItsOwnForward(t *testing.T) {
	sockPath := testguard.SocketPath(t, "own.sock")
	var mu sync.Mutex
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/provision", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get(agentproto.AuthHeader)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})

	// The listener is this test process: the tunnel it stands in for names
	// this process's own process group — af's own ssh child, proven.
	p := &sandboxProvisioner{socketPath: sockPath, tunnel: tunnelChildOnMyGroup(t)}
	ep := p.tunnelEndpoint("the-session-bearer")
	rc, err := newRemoteAgentClient(*ep, "t")
	require.NoError(t, err)
	require.NoError(t, rc.call("/v1/agent/provision", agentLifecycleReq{FirstTimeSetup: true}, nil))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, agentproto.BearerScheme+"the-session-bearer", gotAuth,
		"the credential reaches af's own proven endpoint")
}
