//go:build darwin

package sessionenv

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
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

// The broker keeps the connection open after its final reply until the client
// closes. Closing first made the client's identity read fail with ENOTCONN and
// refused a live claim (#6455). Uses only the wire types and peerPID, so it also
// compiles against the code before the fix.
func TestDarwin_TheBrokerHoldsTheConnectionUntilTheClientCloses(t *testing.T) {
	b, sock := startBroker(t, paneOf("s", os.Getpid()))
	token, err := b.Mint(testCap())
	require.NoError(t, err)
	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer conn.Close()
	rd := bufio.NewReader(conn)
	for _, req := range []claimRequest{{Hello: true}, {Token: token, Pane: "%1"}} {
		require.NoError(t, json.NewEncoder(conn).Encode(req))
		line, err := rd.ReadBytes('\n')
		require.NoError(t, err)
		var resp claimResponse
		require.NoError(t, json.Unmarshal(line, &resp))
		require.True(t, resp.OK, resp.Error)
	}
	// The final reply is read; the broker must not have hung up yet.
	pid, err := peerPID(conn)
	require.NoError(t, err, "the broker closed before the client read its identity")
	require.Equal(t, os.Getpid(), pid)
}

// LOCAL_PEERPID names the last process to use the peer socket, not the
// connector (measured on macos-latest and macos-15-intel). The first message is
// attributed to the identity read at connect; a later one is read afresh, so a
// descriptor handed to another process after connect is not attributed to the
// connector.
func TestDarwin_ALaterMessageIsAttributedByAFreshRead(t *testing.T) {
	client, server := dialedPair(t)
	st, err := enableAttestation(server)
	require.NoError(t, err)
	_, err = client.Write([]byte("first\n"))
	require.NoError(t, err)
	line, pid, err := readAttestedLine(server, st)
	require.NoError(t, err)
	require.Equal(t, "first", string(line))
	require.Equal(t, os.Getpid(), pid)

	f, err := client.(*net.UnixConn).File()
	require.NoError(t, err)
	defer f.Close()
	child := exec.Command("/bin/sh", "-c", "printf 'second\\n' >&3")
	child.ExtraFiles = []*os.File{f}
	require.NoError(t, child.Run())
	line, pid, err = readAttestedLine(server, st)
	require.NoError(t, err)
	require.Equal(t, "second", string(line))
	require.NotEqual(t, os.Getpid(), pid, "a message written by another process was attributed to the connector")
	require.Positive(t, pid)
}
