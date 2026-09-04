package commands

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sshrelay"
)

// `af ssh-relay` is the ProxyCommand af hands to its own ssh invocations (#3086),
// which makes its stdout THE SSH TRANSPORT STREAM.
//
// That is the property these tests exist for, and it is a property of the whole
// PROGRAM rather than of sshrelay.Run: a log line from an init(), a cobra usage
// dump, a migration notice, a daemon-autostart banner — any of them corrupts the
// protocol, and none is visible from the function under test. So these drive the
// REAL binary and compare bytes.

// TestSSHRelayWritesNothingButRelayedBytesToStdout is the one that would catch a
// stray Println anywhere in af's startup.
func TestSSHRelayWritesNothingButRelayedBytesToStdout(t *testing.T) {
	// Deliberately BINARY, not a greeting: ssh's transport is framed binary, and a
	// payload of printable text could hide a stray newline written beside it.
	payload := make([]byte, 0, 4096)
	for i := 0; i < 4096; i++ {
		payload = append(payload, byte(i%251))
	}
	addr := serveOnce(t, func(conn net.Conn) {
		_, _ = conn.Write(payload)
		_ = conn.Close()
	})

	stdout, stderr, err := runRelay(t, nil, addr)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.True(t, bytes.Equal(payload, stdout),
		"stdout must carry the relayed bytes and NOTHING else — it is the ssh transport stream, so one extra "+
			"byte corrupts the session rather than merely being noisy (got %d bytes, want %d)",
		len(stdout), len(payload))
}

// The other direction, and the half-close that makes it terminate. af streams its
// own binary to the remote over ssh's stdin, and the remote `cat` finishes only
// when it sees EOF.
func TestSSHRelayForwardsStdinAndHalfClosesOnEOF(t *testing.T) {
	payload := bytes.Repeat([]byte("stream-me\n"), 5000)
	addr := serveOnce(t, func(conn net.Conn) {
		// Reads until EOF, which only arrives if the relay shuts down its write half
		// rather than holding the connection open or closing it outright.
		got, _ := io.ReadAll(conn)
		_, _ = fmt.Fprintf(conn, "%d", len(got))
		_ = conn.Close()
	})

	stdout, stderr, err := runRelay(t, payload, addr)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Equal(t, strconv.Itoa(len(payload)), string(stdout),
		"the far side must receive every byte AND then see EOF; a relay that never half-closes hangs here "+
			"instead, and one that closes the whole connection loses this reply")
}

// A relay that cannot dial must say so where diagnostics belong, and must leave
// the transport stream untouched.
func TestSSHRelayReportsADialFailureOnStderrOnly(t *testing.T) {
	// A port nothing is listening on: bind one and release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := ln.Addr().String()
	require.NoError(t, ln.Close())

	stdout, stderr, err := runRelay(t, nil, dead)
	require.Error(t, err, "a relay that cannot reach the pinned address must fail rather than exit 0 on an "+
		"empty stream, which ssh would read as a closed connection")
	assert.Empty(t, stdout, "not one byte on the transport stream, even on the failure path")
	assert.Contains(t, string(stderr), dead, "the operator needs to see WHICH address could not be reached")
}

// Two arguments, not one host:port string — so an IPv6 literal is bracketed by
// net.JoinHostPort inside the relay rather than by a caller who might forget.
func TestSSHRelayBracketsAnIPv6Literal(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		_, _ = conn.Write([]byte("V6_OK"))
		_ = conn.Close()
	}()
	t.Cleanup(func() { _ = ln.Close() })

	stdout, stderr, err := runRelayArgs(t, nil, "::1", strconv.Itoa(port))
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Equal(t, "V6_OK", string(stdout))
}

// A malformed invocation must not put a usage block on the transport stream. This
// is why the command points cobra's output writer at stderr.
func TestSSHRelayKeepsCobraOutputOffStdout(t *testing.T) {
	for _, args := range [][]string{
		{sshrelay.Subcommand},
		{sshrelay.Subcommand, "127.0.0.1"},
		{sshrelay.Subcommand, "127.0.0.1", "22", "extra"},
		{sshrelay.Subcommand, "--help"},
	} {
		stdout, stderr, _ := runAF(t, nil, args...)
		assert.Empty(t, stdout,
			"`af %s` wrote to stdout; cobra's usage and help writers default to os.Stdout, and this command's "+
				"stdout is the ssh transport stream", strings.Join(args, " "))
		assert.NotEmpty(t, stderr, "the diagnostic still has to go somewhere")
	}
}

// The command is af talking to af, so it stays out of `af --help`.
func TestSSHRelayIsHidden(t *testing.T) {
	stdout, _, err := runAF(t, nil, "--help")
	require.NoError(t, err)
	assert.NotContains(t, string(stdout), sshrelay.Subcommand,
		"the relay is an internal seam, not a command an operator runs")
}

// serveOnce accepts exactly one connection, hands it to handle, and returns the
// address to dial.
func serveOnce(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		handle(conn)
	}()
	return ln.Addr().String()
}

func runRelay(t *testing.T, stdin []byte, addr string) (stdout, stderr []byte, err error) {
	t.Helper()
	host, port, splitErr := net.SplitHostPort(addr)
	require.NoError(t, splitErr)
	return runRelayArgs(t, stdin, host, port)
}

func runRelayArgs(t *testing.T, stdin []byte, host, port string) (stdout, stderr []byte, err error) {
	t.Helper()
	return runAF(t, stdin, sshrelay.Subcommand, host, port)
}

// runAF invokes the REAL af binary. Nothing short of that can decide the question
// these tests ask, which is what the whole program writes to fd 1.
func runAF(t *testing.T, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	t.Helper()
	cmd := exec.Command(afTestBinary(t), args...)
	// A throwaway AF home, so a relay can neither read nor disturb the real one.
	cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+t.TempDir(), "HOME="+t.TempDir())
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// A relay that never exits is a wedged provision step, so bound it here rather
	// than letting the suite hang.
	done := make(chan error, 1)
	require.NoError(t, cmd.Start())
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("af %s did not exit; a relay must terminate when the remote closes", strings.Join(args, " "))
	}
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// afTestBinary builds af once per test binary run.
var (
	afBuildOnce sync.Once
	afBuildDir  string
	afBuildPath string
	afBuildErr  error
)

// removeAFTestBinary deletes the once-per-package build directory. TestMain
// calls it, NOT t.Cleanup: afBuildOnce caches the build across every test in the
// package, so a per-test cleanup would delete the binary the next test still
// resolves to. Without it each `go test ./commands/...` run left one more
// /tmp/af-relay-cmd* directory on the box — 182 of them by the time #3842
// counted.
func removeAFTestBinary() {
	if afBuildDir != "" {
		_ = os.RemoveAll(afBuildDir)
	}
}

func afTestBinary(t *testing.T) string {
	t.Helper()
	afBuildOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			afBuildErr = fmt.Errorf("no go toolchain: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "af-relay-cmd")
		if err != nil {
			afBuildErr = err
			return
		}
		afBuildDir = dir
		out := filepath.Join(dir, "af")
		build := exec.Command("go", "build", "-o", out, "github.com/sachiniyer/agent-factory")
		build.Dir = ".."
		if combined, buildErr := build.CombinedOutput(); buildErr != nil {
			afBuildErr = fmt.Errorf("building af: %w: %s", buildErr, combined)
			return
		}
		afBuildPath = out
	})
	if afBuildErr != nil {
		t.Skipf("cannot build the af binary under test (%v)", afBuildErr)
	}
	return afBuildPath
}
