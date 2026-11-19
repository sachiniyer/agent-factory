//go:build darwin

package proctree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// The decoder's facts, read off a real tmux server on the runner (#7537). For
// every socket descriptor of the server this logs the raw socket_fdinfo fields
// the decoder reads, then requires the listener to decode to the server's own
// path. A refused decode fails with the bytes the kernel wrote, so a layout is
// fixed from those rather than from a transcription. A socket that closes
// between the listing and its query is skipped under the unixSockets walk's
// own EBADF/ENOENT rule (#4217): it was held, and its loss is not a failed
// read.
func TestSocketFdInfoOfARealTmuxServer(t *testing.T) {
	dir := privateSocketDir(t)
	sock := filepath.Join(dir, "s")
	server := socketTmux(t, dir, sock)

	fds, err := listProcFds(server)
	require.NoError(t, err)
	var listeners []string
	sockets := 0
	for _, fi := range fds {
		fd := int(int32(binary.LittleEndian.Uint32(fi[procFdInfoFdOffset:])))
		if binary.LittleEndian.Uint32(fi[procFdInfoFdTypeOffset:]) != procFdTypeSocket {
			continue
		}
		sockets++
		var buf [socketFdInfoSize]byte
		n, err := procInfoRaw(procInfoCallPIDFDInfo, server, procPIDFdSocketInfo,
			uint64(fd), unsafe.Pointer(&buf[0]), int32(len(buf)))
		if fdClosedSinceListing(err) {
			continue // the unixSockets walk's own rule: closed since the listing
		}
		require.NoError(t, err, "proc_pidfdinfo(PROC_PIDFDSOCKETINFO) for fd %d", fd)
		dump := socketFdInfoDump(buf[:])
		t.Logf("server pid %d fd %d: the kernel wrote %d bytes\n%s", server, fd, n, dump)
		require.EqualValues(t, socketFdInfoSize, n, "fd %d\n%s", fd, dump)
		s, err := unixSocketFromSocketFdInfo(buf[:])
		require.NoError(t, err, "fd %d refused\n%s", fd, dump)
		if s.unix && s.listening {
			listeners = append(listeners, s.path)
		}
	}
	require.NotZero(t, sockets, "a tmux server holds at least its listening socket")
	require.Equal(t, []string{sock}, listeners, "the server's one listener decodes to its own -S path")
}

// socketFdInfoDump renders the socket_fdinfo fields at the offsets unixsocket.go
// declares, and the raw bytes around them.
func socketFdInfoDump(buf []byte) string {
	le16 := func(off int) uint16 { return binary.LittleEndian.Uint16(buf[off:]) }
	le32 := func(off int) int32 { return int32(binary.LittleEndian.Uint32(buf[off:])) }
	path := buf[socketFdInfoSunPathOff : socketFdInfoSunPathOff+darwinSunPathLen]
	if end := bytes.IndexByte(path, 0); end >= 0 {
		path = path[:end]
	}
	return fmt.Sprintf("  pfi: fi_openflags=%#x fi_status=%#x fi_type=%d\n"+
		"  psi: soi_type=%d soi_protocol=%d soi_family=%d soi_options=%#x soi_state=%#x soi_kind=%d\n"+
		"  unsi_addr: sun_len=%d sun_family=%d sun_path=%q\n"+
		"  raw[0:24]    % x\n  raw[24:40]   % x\n  raw[176:196] % x\n  raw[256:268] % x\n  raw[280:330] % x",
		le32(0), le32(4), le32(16),
		le32(socketFdInfoTypeOffset), le32(socketFdInfoTypeOffset+4), le32(socketFdInfoFamilyOffset),
		le16(socketFdInfoOptsOffset), le16(socketFdInfoOptsOffset+4), le32(socketFdInfoKindOffset),
		buf[socketFdInfoSunLenOffset], buf[socketFdInfoSunFamOffset], path,
		buf[0:24], buf[24:40], buf[176:196], buf[256:268], buf[280:330])
}
