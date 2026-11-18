//go:build linux

package proctree

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every kind of unix socket a process can hold, read one socket at a time
// (#7536): a listener, the connection it accepted (which carries the
// listener's path but is not listening), the client end (unbound), an abstract
// listener, and an unbound socketpair. A TCP listener is not a unix socket and
// is not reported.
func TestUnixSocketsReadsEachSocketKind(t *testing.T) {
	dir := privateSocketDir(t)
	sock := filepath.Join(dir, "k")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	client, err := net.Dial("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	accepted, err := ln.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = accepted.Close() })
	abstractName := "af-7536-" + filepath.Base(dir)
	abs, err := net.Listen("unix", "@"+abstractName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = abs.Close() })
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Close(pair[0]); _ = syscall.Close(pair[1]) })
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tcp.Close() })

	socks, err := UnixSockets(os.Getpid())
	require.NoError(t, err)
	count := func(want UnixSocket) int {
		n := 0
		for _, s := range socks {
			if s == want {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, count(UnixSocket{Path: sock, Listening: true}), "the listener: %+v", socks)
	require.Equal(t, 1, count(UnixSocket{Path: sock}), "the accepted connection carries the path, not listening: %+v", socks)
	require.Equal(t, 1, count(UnixSocket{Path: "@" + abstractName, Abstract: true, Listening: true}), "the abstract listener: %+v", socks)
	require.GreaterOrEqual(t, count(UnixSocket{}), 3, "the client end and both socketpair ends are unbound: %+v", socks)

	paths, err := UnixListenerPaths(os.Getpid())
	require.NoError(t, err)
	require.Contains(t, paths, sock)
	require.NotContains(t, paths, "@"+abstractName, "an abstract listener names no file")
}

// The table reader finds the same listener: it is the positive-only fallback
// for when sock_diag cannot answer (#7536).
func TestUnixListenerPathsByTableFindsTheListener(t *testing.T) {
	dir := privateSocketDir(t)
	sock := filepath.Join(dir, "t")
	server := socketTmux(t, dir, sock)
	paths, err := UnixListenerPathsByTable(server)
	require.NoError(t, err)
	require.Equal(t, []string{sock}, paths)
}

// A socket the process still holds and the kernel cannot find, and a kernel
// with no unix_diag, both fail the whole read (#7536). Neither may read as
// "holds none": that would rule a server out, or prove one private, on
// nothing.
func TestUnixSocketsRefusesWhatSockDiagCannotAnswer(t *testing.T) {
	dir := privateSocketDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "r"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	prevLookup := unixDiagLookup
	unixDiagLookup = func(*unixDiagConn, uint32) (unixDiagAnswer, error) { return unixDiagAnswer{}, errUnixDiagNoSuchSocket }
	_, err = UnixSockets(os.Getpid())
	unixDiagLookup = prevLookup
	require.Error(t, err, "a socket still held that sock_diag cannot find is a failed read")
	require.ErrorIs(t, err, errUnixDiagNoSuchSocket)

	prevOpen := openUnixDiagFn
	openUnixDiagFn = func() (*unixDiagConn, error) { return nil, syscall.EPROTONOSUPPORT }
	_, err = UnixSockets(os.Getpid())
	openUnixDiagFn = prevOpen
	require.Error(t, err, "no unix_diag is a failed read")
	require.True(t, errors.Is(err, syscall.EPROTONOSUPPORT), "%v", err)

	_, err = UnixListenerPaths(os.Getpid())
	require.NoError(t, err, "control: the real kernel answers")
}
