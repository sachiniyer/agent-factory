package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/agent"
)

// #1684 regression. agentSigners() probes a running ssh-agent for signers; the
// signers are agentKeyringSigner values that sign by calling back into the agent
// over the Unix-socket connection during the ssh handshake, so agentSigners()
// cannot self-contain them — it hands the live connection back to the caller as
// an io.Closer to close once the dial is done.
//
// The bug (introduced in #1665) was that agentSigners() opened the connection,
// pulled the signers, and returned WITHOUT ever surfacing or closing the conn on
// the success path — and likewise left the probe conn open when the agent held no
// keys. The agent client's readLoop goroutine stayed blocked reading from the
// socket forever (a GC root, so nothing was reclaimed) and the Unix socket FD
// stayed open. Every ssh session creation leaked one of each, unbounded.
//
// These tests assert the fix DETERMINISTICALLY rather than by sampling goroutine
// counts (which flakes under CI load): a stub ssh-agent signals on connClosed the
// instant a served connection's handler returns — i.e. the moment the client end
// is actually closed. "Conn not closed" is then a missing event (a blocked
// waitClosed), not a fuzzy count delta.

// TestAgentSignersClosesConnOnSuccess covers the success path: an agent holding a
// key. agentSigners() must return the live conn as a NON-NIL closer (pre-fix it
// returned no closer at all, so the caller could never reclaim it), and closing
// that closer must actually close the underlying agent conn — which the stub
// observes as its handler returning.
func TestAgentSignersClosesConnOnSuccess(t *testing.T) {
	stub := startStubAgent(t, keyringWithKey(t))
	t.Setenv("SSH_AUTH_SOCK", stub.sock)

	signers, closer := agentSigners()
	if len(signers) == 0 {
		t.Fatal("agentSigners returned no signers; the stub agent holds one key")
	}
	if closer == nil {
		t.Fatal("agentSigners returned signers but a nil closer — the caller cannot reclaim the agent conn (the #1684 leak)")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("closing the returned agent conn: %v", err)
	}
	// Deterministic: the stub's handler returns only when its read errors, i.e.
	// when the client end is closed. If Close() had not closed the real conn, this
	// blocks until the timeout and fails.
	stub.waitClosed(t)
}

// TestAgentSignersClosesConnWhenNoKeys covers the no-usable-signers path: an agent
// that holds no keys. agentSigners() opens the probe connection, finds nothing,
// and must close that connection ITSELF (returning a nil closer) — otherwise an
// empty or wedged agent socket leaks a goroutine + FD on every call just the same.
// This is the direct red-on-pre-fix / green-on-fix assertion: the old code left
// this conn open, so waitClosed would block; the fix closes it, so waitClosed
// returns promptly.
func TestAgentSignersClosesConnWhenNoKeys(t *testing.T) {
	stub := startStubAgent(t, agent.NewKeyring())
	t.Setenv("SSH_AUTH_SOCK", stub.sock)

	signers, closer := agentSigners()
	if len(signers) != 0 {
		t.Fatalf("empty agent returned %d signers, want 0", len(signers))
	}
	if closer != nil {
		t.Fatal("empty agent returned a non-nil closer; agentSigners must close the probe conn itself when there are no signers")
	}
	// We closed nothing — agentSigners itself must have closed the probe conn.
	stub.waitClosed(t)
}

// TestAgentSignersNoSocket covers the trivial no-agent path: with SSH_AUTH_SOCK
// unset there is nothing to dial, so nothing is opened and nothing can leak.
func TestAgentSignersNoSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	if signers, closer := agentSigners(); signers != nil || closer != nil {
		t.Fatalf("agentSigners with no SSH_AUTH_SOCK = (%v, %v), want (nil, nil)", signers, closer)
	}
}

// stubAgent is an in-process ssh-agent served over a Unix socket. Each served
// connection sends on connClosed the moment its handler returns — ServeAgent
// returns when the connection read errors, which happens when the client end is
// closed — giving tests a deterministic "the agent conn was closed" signal.
type stubAgent struct {
	sock       string
	connClosed chan struct{}
}

// waitClosed blocks until a served connection's client end is closed, or fails
// the test after a generous timeout (only reached if the conn is genuinely never
// closed — the leak).
func (s *stubAgent) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-s.connClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("ssh-agent conn was never closed — leaked FD + readLoop goroutine (#1684)")
	}
}

// keyringWithKey returns an in-memory agent.Agent holding a single ed25519 key.
func keyringWithKey(t *testing.T) agent.Agent {
	t.Helper()
	keyring := agent.NewKeyring()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatalf("add key to stub agent: %v", err)
	}
	return keyring
}

// startStubAgent serves keyring over a Unix socket and returns its handle. It
// SKIPS (does not fail) the test where Unix sockets are unavailable or the socket
// path would exceed the platform's sun_path limit — the socket path is kept short
// (a minimal temp dir + a one-char name) to stay well under it.
func startStubAgent(t *testing.T, keyring agent.Agent) *stubAgent {
	t.Helper()
	dir, err := os.MkdirTemp("", "afa")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix-socket ssh-agent unavailable here (%v); skipping ssh-agent leak test", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &stubAgent{sock: sock, connClosed: make(chan struct{}, 64)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed on cleanup
			}
			go func(c net.Conn) {
				_ = agent.ServeAgent(keyring, c)
				_ = c.Close()
				select {
				case s.connClosed <- struct{}{}:
				default:
				}
			}(conn)
		}
	}()
	return s
}
