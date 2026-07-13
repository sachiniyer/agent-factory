package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/agent"
)

// TestAgentSignersNoLeak is the #1684 regression. agentSigners() probes a running
// ssh-agent for signers; the signers are agentKeyringSigner values that sign by
// calling back into the agent over the Unix-socket connection during the ssh
// handshake, so agentSigners() cannot self-contain them — it hands the live
// connection back to the caller as an io.Closer to close once the dial is done.
//
// The bug (introduced in #1665) was that agentSigners() opened the connection,
// pulled the signers, and returned WITHOUT ever surfacing or closing the conn on
// the success path: the agent client's readLoop goroutine stayed blocked reading
// from the socket forever (a GC root, so nothing was reclaimed) and the Unix
// socket FD stayed open. Every ssh session creation leaked one of each, unbounded.
//
// This test starts an in-process ssh-agent holding one key and asserts:
//  1. agentSigners() returns a NON-NIL closer whenever it returns signers — the
//     structural contract that lets the caller reclaim the connection at all.
//  2. Calling agentSigners() N times and closing each returned conn leaves the
//     goroutine (and FD) count flat — the leak is gone.
//  3. Discarding the closer instead (the old, leaky shape) DOES grow goroutines —
//     proving the no-growth assertion in (2) is meaningful, not vacuously true.
func TestAgentSignersNoLeak(t *testing.T) {
	sock := startTestSSHAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sock)

	const n = 20

	// (2) The fixed path: obtain signers, close the returned conn each time. The
	// readLoop goroutine and its socket FD must not accumulate.
	t.Run("closed_conn_does_not_leak", func(t *testing.T) {
		settleGoroutines()
		baseGo := runtime.NumGoroutine()
		baseFD := openFDCount(t)

		for i := 0; i < n; i++ {
			signers, conn := agentSigners()
			if len(signers) == 0 {
				t.Fatalf("call %d: agentSigners returned no signers; the test agent holds one key", i)
			}
			// (1) The core contract: a live conn backs the signers, so the caller
			// must be handed a closer to reclaim it.
			if conn == nil {
				t.Fatalf("call %d: agentSigners returned signers but a nil closer — the conn cannot be reclaimed (the #1684 leak)", i)
			}
			if err := conn.Close(); err != nil {
				t.Fatalf("call %d: closing agent conn: %v", i, err)
			}
		}

		settleGoroutines()
		if grew := runtime.NumGoroutine() - baseGo; grew > 2 {
			t.Errorf("goroutines grew by %d after %d closed agentSigners calls; want ~0 (leaked readLoop goroutines)", grew, n)
		}
		if fd := openFDCount(t); fd > 0 && baseFD > 0 && fd-baseFD > 2 {
			t.Errorf("open FDs grew by %d after %d closed agentSigners calls; want ~0 (leaked ssh-agent socket FDs)", fd-baseFD, n)
		}
	})

	// (3) Sanity check on the methodology: keep the conns OPEN (the pre-fix shape,
	// where the caller had no closer to call) and confirm goroutines DO grow. If
	// this did not grow, the no-growth assertion above would prove nothing.
	t.Run("leaked_conn_grows_goroutines", func(t *testing.T) {
		settleGoroutines()
		baseGo := runtime.NumGoroutine()

		conns := make([]io.Closer, 0, n)
		for i := 0; i < n; i++ {
			signers, conn := agentSigners()
			if len(signers) == 0 || conn == nil {
				t.Fatalf("call %d: expected signers + closer from the test agent", i)
			}
			conns = append(conns, conn) // deliberately do NOT close yet
		}

		if grew := runtime.NumGoroutine() - baseGo; grew < n/2 {
			t.Errorf("goroutines grew by only %d with %d open agent conns; expected a leaked readLoop per open conn — the leak-detection methodology is not working", grew, n)
		}

		// Clean up: close them so the leaked goroutines exit before the next test.
		for _, c := range conns {
			_ = c.Close()
		}
		settleGoroutines()
	})
}

// TestAgentSignersEmptyAgentNoLeak covers the no-usable-signers path: an agent
// that holds no keys. agentSigners() opens the probe connection, finds nothing,
// and must close that connection itself (returning a nil closer) — otherwise an
// empty or wedged agent socket leaks a goroutine + FD on every call just the same.
func TestAgentSignersEmptyAgentNoLeak(t *testing.T) {
	sock := startTestSSHAgentEmpty(t)
	t.Setenv("SSH_AUTH_SOCK", sock)

	settleGoroutines()
	baseGo := runtime.NumGoroutine()

	const n = 20
	for i := 0; i < n; i++ {
		signers, conn := agentSigners()
		if len(signers) != 0 {
			t.Fatalf("call %d: empty agent returned %d signers, want 0", i, len(signers))
		}
		if conn != nil {
			t.Fatalf("call %d: empty agent returned a non-nil closer; agentSigners must close the probe conn itself when there are no signers", i)
		}
	}

	settleGoroutines()
	if grew := runtime.NumGoroutine() - baseGo; grew > 2 {
		t.Errorf("goroutines grew by %d after %d calls against an empty agent; want ~0 (probe conn not closed)", grew, n)
	}
}

// startTestSSHAgent serves an in-process ssh-agent holding a single ed25519 key
// over a Unix socket and returns its path. The listener + accept loop are torn
// down on test cleanup.
func startTestSSHAgent(t *testing.T) string {
	t.Helper()
	keyring := agent.NewKeyring()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatalf("add key to test agent: %v", err)
	}
	return serveTestSSHAgent(t, keyring)
}

// startTestSSHAgentEmpty serves an in-process ssh-agent with no keys.
func startTestSSHAgentEmpty(t *testing.T) string {
	t.Helper()
	return serveTestSSHAgent(t, agent.NewKeyring())
}

func serveTestSSHAgent(t *testing.T, keyring agent.Agent) string {
	t.Helper()
	// Keep the socket path short — Unix socket paths are capped (~108 bytes on
	// Linux) and t.TempDir() can be long, so use a compact temp dir name.
	dir, err := os.MkdirTemp("", "af-agent")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on agent socket: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed on cleanup
			}
			go func() { _ = agent.ServeAgent(keyring, conn) }()
		}
	}()
	return sock
}

// settleGoroutines gives asynchronously-exiting goroutines (agent readLoops that
// unblock when a conn closes) a moment to actually finish before the count is
// sampled, so the assertions do not race the teardown.
func settleGoroutines() {
	for i := 0; i < 30; i++ {
		runtime.GC()
		time.Sleep(15 * time.Millisecond)
	}
}

// openFDCount returns the number of open file descriptors for this process on
// Linux (via /proc/self/fd), or 0 where that is unavailable — callers treat 0 as
// "FD counting not supported" and skip the FD assertion.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}
