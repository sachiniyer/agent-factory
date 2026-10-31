//go:build darwin

package sessionenv

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Measures, on the macOS runner, whether the kernel offers any per-message
// sender attestation on a unix stream socket. It asserts only that the
// documented posture (perMessageAttestation == false) matches what it sees: if
// ancillary credentials ever do arrive, this fails and says so, so the residual
// is re-evaluated instead of silently outliving its premise.
func TestDarwinOffersNoPerMessageSenderAttestation(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(shortSocketDir(t), "d.sock"))
	require.NoError(t, err)
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	client, err := net.Dial("unix", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	server := <-accepted
	defer server.Close()

	_, err = server.Write([]byte("x\n"))
	require.NoError(t, err)
	buf, oob := make([]byte, 16), make([]byte, 256)
	n, oobn, flags, _, err := client.(*net.UnixConn).ReadMsgUnix(buf, oob)
	require.NoError(t, err)
	msgs, _ := unix.ParseSocketControlMessage(oob[:oobn])
	t.Logf("darwin: read %d bytes, %d ancillary bytes, flags=%#x, control messages=%d", n, oobn, flags, len(msgs))
	require.Zero(t, oobn, "darwin delivered ancillary data without being asked; re-evaluate perMessageAttestation")
	require.False(t, perMessageAttestation)
}

func dialedPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(shortSocketDir(t), "p.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	client, err = net.Dial("unix", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	server = <-accepted
	t.Cleanup(func() { _ = server.Close() })
	return client, server
}

// The mechanism behind #6455: once the peer has closed, macOS answers
// LOCAL_PEERPID with ENOTCONN, so an identity read after the request was sent
// (and the server replied and closed) fails for a live client. Read at connect,
// it holds, and the reply the peer wrote before closing is still delivered.
func TestDarwin_PeerIdentityIsReadAtConnect(t *testing.T) {
	client, server := dialedPair(t)
	st, err := enableAttestation(client)
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), st.pid)

	_, err = server.Write([]byte("answer\n"))
	require.NoError(t, err)
	require.NoError(t, server.Close())

	_, lateErr := peerPID(client)
	require.ErrorIs(t, lateErr, errPeerGone, "a read after the peer closed must fail with the stable peer-gone error")

	line, pid, err := readAttestedLine(client, st)
	require.NoError(t, err)
	require.Equal(t, "answer", string(line))
	require.Equal(t, os.Getpid(), pid, "the identity captured at connect must survive the peer closing")
}

// A peer that is truly gone before it can be identified is refused with the
// stable error, never a bare getsockopt string.
func TestDarwin_PeerGoneBeforeIdentificationIsAStableError(t *testing.T) {
	client, server := dialedPair(t)
	require.NoError(t, client.Close())
	_, err := enableAttestation(server)
	require.ErrorIs(t, err, errPeerGone)
}
