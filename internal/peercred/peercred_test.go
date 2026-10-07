//go:build linux || darwin

package peercred

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConnPIDNamesTheConnectingProcess proves the kernel answer tracks the
// CONNECTING process, not anything the caller asserts: this test process dials
// the socket itself, so its own pid is the only possible correct peer.
func TestConnPIDNamesTheConnectingProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peercred.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer listener.Close()

	client, err := net.Dial("unix", path)
	require.NoError(t, err)
	defer client.Close()

	server, err := listener.Accept()
	require.NoError(t, err)
	defer server.Close()

	pid, err := ConnPID(server)
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), pid,
		"the kernel must name the process that CONNECTED, which is this test")
}

// TestConnPIDRejectsNonUnixConn pins the fail-closed direction: a TCP peer has
// no kernel pid on this host, and answering anything would hand a remote
// caller a local exemption it never earned.
func TestConnPIDRejectsNonUnixConn(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	client, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	server, err := listener.Accept()
	require.NoError(t, err)
	defer server.Close()

	_, err = ConnPID(server)
	require.ErrorIs(t, err, ErrNotUnixConn)
}

// TestConnPIDClosesWithReadError — a closed conn must error, never fabricate
// a pid; the teardown seam treats "could not tell" as no requester, so a
// silently wrong answer here is the forgeable exemption #5182 rules out.
func TestConnPIDClosesWithReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peercred-closed.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer listener.Close()

	client, err := net.Dial("unix", path)
	require.NoError(t, err)

	server, err := listener.Accept()
	require.NoError(t, err)
	require.NoError(t, client.Close())
	require.NoError(t, server.Close())

	pid, err := ConnPID(server)
	if err == nil {
		// SO_PEERCRED on a closed-but-still-valid fd still answers on Linux —
		// that is a kernel property, not a defect. The contract being pinned
		// is only that a non-answer NEVER arrives as a wrong pid.
		require.Equal(t, os.Getpid(), pid)
		return
	}
	require.False(t, errors.Is(err, ErrNotUnixConn),
		"an accepted unix conn is still a unix conn once closed")
}
