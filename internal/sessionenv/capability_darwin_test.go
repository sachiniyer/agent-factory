//go:build darwin

package sessionenv

import (
	"net"
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
